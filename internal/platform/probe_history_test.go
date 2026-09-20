package platform

import (
	"bytes"
	"context"
	"database/sql"
	"math"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
)

func probeNumber(v float64) *float64 { return &v }
func historyPath(node, resolution string, from, to time.Time) string {
	return "/probes/" + node + "/history?" + url.Values{"resolution": {resolution}, "from": {from.Format(time.RFC3339)}, "to": {to.Format(time.RFC3339)}}.Encode()
}
func TestProbeHistoryAggregationRetryRestartAndPermissions(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if err := f.s.MigrateProbeHistory(ctx); err != nil {
		t.Fatal(err)
	}
	g, n := f.node()
	base := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour)
	f.s.history = newProbeHistory(base)
	p := contract.Probe{NodeID: n.NodeID, SampledAt: base.Add(5 * time.Second), CPUPercent: probeNumber(10), UploadBPS: probeNumber(100), PublicIPs: []contract.IPObservation{{Address: "192.0.2.55"}}}
	if err := f.s.history.record(p, p.SampledAt); err != nil {
		t.Fatal(err)
	}
	if err := f.s.history.record(p, p.SampledAt); err != nil {
		t.Fatal(err)
	} // replay is not a new observation
	p.SampledAt = base.Add(10 * time.Second)
	p.CPUPercent = probeNumber(30)
	p.UploadBPS = nil
	if err := f.s.history.record(p, p.SampledAt); err != nil {
		t.Fatal(err)
	}
	p.SampledAt = base.Add(time.Minute + 5*time.Second)
	p.CPUPercent = probeNumber(80)
	p.UploadBPS = probeNumber(300)
	if err := f.s.history.record(p, p.SampledAt); err != nil {
		t.Fatal(err)
	}
	// Failure must retain pending data for retry.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := f.s.FlushProbeHistory(cancelled, base.Add(4*time.Minute)); err == nil {
		t.Fatal("cancelled flush succeeded")
	}
	if len(f.s.history.pending) != 2 {
		t.Fatal("failed flush lost samples")
	}
	if err := f.s.FlushProbeHistory(ctx, base.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// Simulate a committed batch whose acknowledgement was lost.
	f.s.history.pending[probeBucket{n.NodeID, base.Unix()}] = probeAggregate{Samples: 2, Metrics: [6]probeMetric{{Sum: 40, Count: 2}, {}, {}, {}, {Sum: 100, Count: 1}, {}}}
	if err := f.s.FlushProbeHistory(ctx, base.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Items []ProbeHistoryPoint `json:"items"`
		Total int                 `json:"total"`
	}
	result = read[struct {
		Items []ProbeHistoryPoint `json:"items"`
		Total int                 `json:"total"`
	}](t, f.req("GET", historyPath(n.NodeID, "minute", base, base.Add(time.Hour)), nil, ""), 200)
	if result.Total != 2 || result.Items[0].Samples != 2 || *result.Items[0].CPU != 20 || *result.Items[0].Upload != 100 || result.Items[0].Memory != nil || result.Items[0].Download != nil {
		t.Fatalf("incorrect minute aggregation: %+v", result)
	}
	hour := read[struct {
		Items []ProbeHistoryPoint `json:"items"`
	}](t, f.req("GET", historyPath(n.NodeID, "hour", base, base.Add(time.Hour)), nil, ""), 200)
	if len(hour.Items) != 1 || hour.Items[0].Samples != 3 || *hour.Items[0].CPU != 40 || *hour.Items[0].Upload != 200 {
		t.Fatalf("weighted aggregation or dedup failed: %+v", hour)
	}
	// A fresh server reads the same persisted values without its old live cache.
	restarted := New(f.s.Store, Options{})
	if err := restarted.MigrateProbeHistory(ctx); err != nil {
		t.Fatal(err)
	}
	if err := restarted.history.record(p, time.Now()); err == nil {
		t.Fatal("restart accepted already sealed sample")
	}
	f.s = restarted
	f.m = http.NewServeMux()
	f.s.Register(f.m)
	restored := read[struct {
		Items []ProbeHistoryPoint `json:"items"`
	}](t, f.req("GET", historyPath(n.NodeID, "hour", base, base.Add(time.Hour)), nil, ""), 200)
	if len(restored.Items) != 1 || restored.Items[0].Samples != 3 {
		t.Fatal("history did not survive server replacement")
	}
	adminCookie := f.cookie
	user := read[contract.User](t, f.req("POST", "/users", map[string]any{"username": "history-user", "password": "test-password-long", "role": "user"}, ""), 201)
	login := f.req("POST", "/auth/login", map[string]any{"username": "history-user", "password": "test-password-long"}, "")
	f.cookie = login.Result().Cookies()[0]
	path := historyPath(n.NodeID, "minute", base, base.Add(time.Hour))
	if r := f.req("GET", path, nil, ""); r.Code != 404 {
		t.Fatal("unauthorized history disclosed", r.Code)
	}
	f.cookie = adminCookie
	g.UserIDs = []string{user.ID}
	g = read[contract.Group](t, f.req("PUT", "/groups/"+g.ID, g, ""), 200)
	f.cookie = login.Result().Cookies()[0]
	r := f.req("GET", path, nil, "")
	if r.Code != 200 || bytes.Contains(r.Body.Bytes(), []byte("192.0.2.55")) || bytes.Contains(r.Body.Bytes(), []byte("public_ips")) {
		t.Fatal("history permission or IP filtering failed", r.Code, r.Body.String())
	}
	f.cookie = adminCookie
	g.UserIDs = nil
	read[contract.Group](t, f.req("PUT", "/groups/"+g.ID, g, ""), 200)
	f.cookie = login.Result().Cookies()[0]
	if r := f.req("GET", path, nil, ""); r.Code != 404 {
		t.Fatal("revoked history permission survived")
	}
}

func TestProbeHistoryRetentionAndBoundedInput(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if err := f.s.MigrateProbeHistory(ctx); err != nil {
		t.Fatal(err)
	}
	_, n := f.node()
	now := time.Now().UTC().Truncate(time.Hour).Add(34*time.Minute + 30*time.Second)
	h := newProbeHistory(now)
	for _, value := range []float64{math.NaN(), math.Inf(1), -1, 101} {
		if err := h.record(contract.Probe{NodeID: n.NodeID, SampledAt: now, CPUPercent: &value}, now); err == nil {
			t.Fatal("invalid metric accepted", value)
		}
	}
	for i := 0; i < historyBuckets; i++ {
		h.pending[probeBucket{Node: "queued", Minute: int64(i)}] = probeAggregate{}
	}
	if err := h.record(contract.Probe{NodeID: n.NodeID, SampledAt: now}, now); err == nil {
		t.Fatal("history queue exceeded bound")
	}
	err := f.s.Store.Write(ctx, storage.Background, func(tx *sql.Tx) error {
		for _, item := range []struct {
			resolution string
			at         time.Time
		}{{"minute", now.Add(-8 * 24 * time.Hour)}, {"minute", now.Add(-6 * 24 * time.Hour)}, {"hour", now.Add(-181 * 24 * time.Hour)}, {"hour", now.Add(-179 * 24 * time.Hour)}, {"minute", now.Add(-7 * 24 * time.Hour).Truncate(time.Minute)}, {"hour", now.Add(-180 * 24 * time.Hour).Truncate(time.Hour)}} {
			if _, err := tx.ExecContext(ctx, f.s.q(`INSERT INTO cp_probe_history(node_id,resolution,bucket,payload) VALUES(?,?,?,?)`), n.NodeID, item.resolution, item.at.Unix(), strJSON(probeAggregate{})); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.s.PruneProbeHistory(ctx, now); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = f.s.Store.DB.QueryRow("SELECT COUNT(*) FROM cp_probe_history").Scan(&count); err != nil || count != 4 {
		t.Fatal("retention mismatch", count, err)
	}
	for _, path := range []string{historyPath(n.NodeID, "minute", now.Add(-8*24*time.Hour), now), historyPath(n.NodeID, "hour", now.Add(-181*24*time.Hour), now), historyPath(n.NodeID, "invalid", now.Add(-time.Hour), now)} {
		if r := f.req("GET", path, nil, ""); r.Code != 400 {
			t.Fatal("unbounded history query accepted", r.Code)
		}
	}
}

func TestProbeHistoryLateAndSubsecondSamplesRemainBounded(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Minute).Add(5 * time.Second)
	h := newProbeHistory(now)
	for _, at := range []time.Time{now, now.Add(-10 * time.Second), now.Add(time.Nanosecond), now} {
		if err := h.record(contract.Probe{NodeID: "node", SampledAt: at, CPUPercent: probeNumber(50)}, now); err != nil {
			t.Fatal(err)
		}
	}
	current := probeBucket{"node", now.Truncate(time.Minute).Unix()}
	previous := probeBucket{"node", now.Add(-time.Minute).Truncate(time.Minute).Unix()}
	if h.pending[current].Samples != 2 || h.pending[previous].Samples != 1 {
		t.Fatal("late/subsecond sample dropped or replay counted")
	}
	for i := 2; i < 120; i++ {
		if err := h.record(contract.Probe{NodeID: "node", SampledAt: now.Add(time.Duration(i) * time.Nanosecond)}, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.record(contract.Probe{NodeID: "node", SampledAt: now.Add(120 * time.Nanosecond)}, now); err == nil {
		t.Fatal("unbounded per-bucket dedup set")
	}
	if len(h.seen[current]) != 120 || h.pending[current].Samples != 120 {
		t.Fatal("rejected sample changed aggregation")
	}
}
