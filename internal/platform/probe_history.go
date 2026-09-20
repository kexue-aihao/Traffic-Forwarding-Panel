package platform

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"math"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
)

const historyBuckets = 8192

// Each metric has its own count: a missing value is never a zero sample.
type probeMetric struct {
	Sum   float64 `json:"sum"`
	Count int64   `json:"count"`
}
type probeAggregate struct {
	Samples int64          `json:"samples"`
	Metrics [6]probeMetric `json:"metrics"`
}
type probeBucket struct {
	Node   string
	Minute int64
}
type probeHistory struct {
	mu      sync.Mutex
	flushMu sync.Mutex
	cutoff  int64
	pending map[probeBucket]probeAggregate
	last    map[string]int64
}
type ProbeHistoryPoint struct {
	SampledAt  time.Time `json:"sampled_at"`
	Resolution string    `json:"resolution"`
	Samples    int64     `json:"samples"`
	CPU        *float64  `json:"cpu_percent"`
	Memory     *float64  `json:"memory_percent"`
	Disk       *float64  `json:"disk_percent"`
	Load       *float64  `json:"load1"`
	Upload     *float64  `json:"upload_bps"`
	Download   *float64  `json:"download_bps"`
}

func newProbeHistory(now time.Time) *probeHistory {
	return &probeHistory{cutoff: now.UTC().Truncate(time.Minute).Add(-time.Minute).Unix(), pending: map[probeBucket]probeAggregate{}, last: map[string]int64{}}
}

func (s *Server) MigrateProbeHistory(ctx context.Context) error {
	return storage.MigrateNamespace(ctx, s.Store.DB, s.Store.Dialect, "probe_history", 1, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS cp_probe_history(node_id VARCHAR(64) NOT NULL,resolution VARCHAR(8) NOT NULL,bucket BIGINT NOT NULL,payload TEXT NOT NULL,PRIMARY KEY(node_id,resolution,bucket))`)
		if err != nil {
			return err
		}
		return storage.EnsureIndex(ctx, conn, s.Store.Dialect, "cp_probe_history", "cp_probe_history_retention", "resolution,bucket", false)
	})
}

func probeValues(p contract.Probe) ([6]*float64, error) {
	v := [6]*float64{p.CPUPercent, nil, nil, p.Load1, p.UploadBPS, p.DownloadBPS}
	for i, pair := range [][2]*uint64{{p.MemoryUsed, p.MemoryTotal}, {p.DiskUsed, p.DiskTotal}} {
		if pair[0] != nil && pair[1] != nil {
			if *pair[0] > *pair[1] {
				return v, errors.New("invalid resource usage")
			}
			if *pair[1] > 0 {
				n := 100 * float64(*pair[0]) / float64(*pair[1])
				v[i+1] = &n
			}
		}
	}
	for i, n := range v {
		if n != nil && (math.IsNaN(*n) || math.IsInf(*n, 0) || *n < 0 || *n > 1e18 || (i < 3 && *n > 100)) {
			return v, errors.New("invalid probe metric")
		}
	}
	return v, nil
}

func (h *probeHistory) record(p contract.Probe, now time.Time) error {
	v, err := probeValues(p)
	if err != nil {
		return err
	}
	key := probeBucket{p.NodeID, p.SampledAt.UTC().Truncate(time.Minute).Unix()}
	h.mu.Lock()
	defer h.mu.Unlock()
	// Old/replayed samples cannot overwrite completed buckets, including after a restart.
	if key.Minute < h.cutoff || key.Minute < now.UTC().Truncate(time.Minute).Add(-time.Minute).Unix() {
		return errors.New("probe sample is too old")
	}
	if last, ok := h.last[p.NodeID]; ok && p.SampledAt.Unix() <= last {
		return nil
	}
	a, exists := h.pending[key]
	if !exists && len(h.pending) >= historyBuckets {
		return errors.New("probe history queue full")
	}
	if _, exists := h.last[p.NodeID]; !exists && len(h.last) >= historyBuckets {
		return errors.New("probe node capacity reached")
	}
	a.Samples++
	for i, n := range v {
		if n != nil {
			a.Metrics[i].Sum += *n
			a.Metrics[i].Count++
		}
	}
	h.pending[key] = a
	h.last[p.NodeID] = p.SampledAt.Unix()
	return nil
}

func (s *Server) FlushProbeHistory(ctx context.Context, now time.Time) error {
	h := s.history
	h.flushMu.Lock()
	defer h.flushMu.Unlock()
	// Keep one minute of lateness. Closed buckets cannot change while SQL is in flight.
	cutoff := now.UTC().Truncate(time.Minute).Add(-time.Minute).Unix()
	h.mu.Lock()
	if cutoff > h.cutoff {
		h.cutoff = cutoff
	}
	keys := []probeBucket{}
	for key := range h.pending {
		if key.Minute < cutoff {
			keys = append(keys, key)
		}
	}
	h.mu.Unlock()
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Minute == keys[j].Minute {
			return keys[i].Node < keys[j].Node
		}
		return keys[i].Minute < keys[j].Minute
	})
	for start := 0; start < len(keys); start += 100 {
		end := min(start+100, len(keys))
		batch := keys[start:end]
		h.mu.Lock()
		values := make([]probeAggregate, len(batch))
		for i, k := range batch {
			values[i] = h.pending[k]
		}
		h.mu.Unlock()
		err := s.Store.Write(ctx, storage.Background, func(tx *sql.Tx) error {
			for i, k := range batch {
				payload := strJSON(values[i])
				var old string
				err := tx.QueryRowContext(ctx, s.q(`SELECT payload FROM cp_probe_history WHERE node_id=? AND resolution='minute' AND bucket=?`), k.Node, k.Minute).Scan(&old)
				if err == nil {
					if old != payload {
						return errors.New("conflicting probe history bucket")
					}
					continue // A previous commit may have succeeded despite a lost response.
				}
				if !errors.Is(err, sql.ErrNoRows) {
					return err
				}
				if _, err = tx.ExecContext(ctx, s.q(`INSERT INTO cp_probe_history(node_id,resolution,bucket,payload) VALUES(?,'minute',?,?)`), k.Node, k.Minute, payload); err != nil {
					return err
				}
				hour := time.Unix(k.Minute, 0).UTC().Truncate(time.Hour).Unix()
				var aggregate probeAggregate
				err = tx.QueryRowContext(ctx, s.q(`SELECT payload FROM cp_probe_history WHERE node_id=? AND resolution='hour' AND bucket=?`), k.Node, hour).Scan(&old)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return err
				}
				exists := err == nil
				if exists {
					if err = json.Unmarshal([]byte(old), &aggregate); err != nil {
						return err
					}
				}
				aggregate.Samples += values[i].Samples
				for j, m := range values[i].Metrics {
					aggregate.Metrics[j].Sum += m.Sum
					aggregate.Metrics[j].Count += m.Count
				}
				if exists {
					_, err = tx.ExecContext(ctx, s.q(`UPDATE cp_probe_history SET payload=? WHERE node_id=? AND resolution='hour' AND bucket=?`), strJSON(aggregate), k.Node, hour)
				} else {
					_, err = tx.ExecContext(ctx, s.q(`INSERT INTO cp_probe_history(node_id,resolution,bucket,payload) VALUES(?,'hour',?,?)`), k.Node, hour, strJSON(aggregate))
				}
				if err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		h.mu.Lock()
		for _, k := range batch {
			delete(h.pending, k)
		}
		h.mu.Unlock()
	}
	h.mu.Lock()
	for node, at := range h.last {
		if at < cutoff {
			delete(h.last, node)
		}
	}
	h.mu.Unlock()
	return nil
}

// Clean a bounded number per call so retention cannot monopolize the writer.
func (s *Server) PruneProbeHistory(ctx context.Context, now time.Time) error {
	return s.Store.Write(ctx, storage.Background, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, s.q(`SELECT node_id,resolution,bucket FROM cp_probe_history WHERE (resolution='minute' AND bucket<?) OR (resolution='hour' AND bucket<?) ORDER BY bucket LIMIT 1000`), now.Add(-7*24*time.Hour).Unix(), now.Add(-180*24*time.Hour).Unix())
		if err != nil {
			return err
		}
		type expired struct {
			node, resolution string
			bucket           int64
		}
		items := []expired{}
		for rows.Next() {
			var x expired
			if err = rows.Scan(&x.node, &x.resolution, &x.bucket); err != nil {
				rows.Close()
				return err
			}
			items = append(items, x)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, x := range items {
			if _, err = tx.ExecContext(ctx, s.q(`DELETE FROM cp_probe_history WHERE node_id=? AND resolution=? AND bucket=?`), x.node, x.resolution, x.bucket); err != nil {
				return err
			}
		}
		return nil
	})
}

// RunProbeHistory is owned by the panel lifecycle. It must stop before Store.Close.
func (s *Server) RunProbeHistory(ctx context.Context) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			s.mu.Lock()
			for node, p := range s.probes {
				if p.SampledAt.Before(now.Add(-24 * time.Hour)) {
					delete(s.probes, node)
				}
			}
			s.mu.Unlock()
			work, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := s.FlushProbeHistory(work, now)
			if err == nil {
				err = s.PruneProbeHistory(work, now)
			}
			cancel()
			if err != nil && ctx.Err() == nil {
				log.Print("probe history persistence failed; pending buckets retained")
			}
		}
	}
}

func (s *Server) probeHistoryList(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFromContext(r.Context())
	node := r.PathValue("node_id")
	q := `SELECT COUNT(*) FROM cp_nodes n WHERE n.id=?`
	args := []any{node}
	if u.Role != "admin" {
		q += ` AND EXISTS(SELECT 1 FROM cp_node_groups ng JOIN cp_group_users gu ON gu.group_id=ng.group_id WHERE ng.node_id=n.id AND gu.user_id=?)`
		args = append(args, u.ID)
	}
	var visible int
	if err := s.Store.DB.QueryRowContext(r.Context(), s.q(q), args...).Scan(&visible); err != nil {
		fail(w, 500, "history unavailable")
		return
	}
	if visible != 1 {
		fail(w, 404, "node not found")
		return
	}
	resolution := r.URL.Query().Get("resolution")
	if resolution == "" {
		resolution = "minute"
	}
	step, retention := time.Minute, 7*24*time.Hour
	if resolution == "hour" {
		step, retention = time.Hour, 180*24*time.Hour
	} else if resolution != "minute" {
		fail(w, 400, "invalid resolution")
		return
	}
	now := time.Now().UTC()
	to := now.Truncate(step).Add(step)
	from := to.Add(-time.Hour)
	var err error
	if raw := r.URL.Query().Get("to"); raw != "" {
		to, err = time.Parse(time.RFC3339, raw)
		if err != nil {
			fail(w, 400, "invalid to time")
			return
		}
	}
	if raw := r.URL.Query().Get("from"); raw != "" {
		from, err = time.Parse(time.RFC3339, raw)
		if err != nil {
			fail(w, 400, "invalid from time")
			return
		}
	}
	if !to.After(from) || to.Sub(from) > retention || from.Before(now.Add(-retention).Truncate(step)) || to.After(now.Truncate(step).Add(step)) {
		fail(w, 400, "history range outside retention")
		return
	}
	ceilSecond := func(t time.Time) int64 {
		v := t.Unix()
		if t.Nanosecond() > 0 {
			v++
		}
		return v
	}
	rows, err := s.Store.DB.QueryContext(r.Context(), s.q(`SELECT bucket,payload FROM cp_probe_history WHERE node_id=? AND resolution=? AND bucket>=? AND bucket<? ORDER BY bucket LIMIT ?`), node, resolution, ceilSecond(from), ceilSecond(to), int(retention/step))
	if err != nil {
		fail(w, 500, "history unavailable")
		return
	}
	defer rows.Close()
	items := []ProbeHistoryPoint{}
	for rows.Next() {
		var bucket int64
		var payload string
		var a probeAggregate
		if rows.Scan(&bucket, &payload) != nil || json.Unmarshal([]byte(payload), &a) != nil {
			fail(w, 500, "history unavailable")
			return
		}
		p := ProbeHistoryPoint{SampledAt: time.Unix(bucket, 0).UTC(), Resolution: resolution, Samples: a.Samples}
		fields := []**float64{&p.CPU, &p.Memory, &p.Disk, &p.Load, &p.Upload, &p.Download}
		for i, m := range a.Metrics {
			if m.Count > 0 {
				v := m.Sum / float64(m.Count)
				*fields[i] = &v
			}
		}
		items = append(items, p)
	}
	if rows.Err() != nil {
		fail(w, 500, "history unavailable")
		return
	}
	reply(w, 200, map[string]any{"items": items, "total": len(items)})
}
