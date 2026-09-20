package platform

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
)

type nodeKey struct{}
type enrollment struct {
	Name     string   `json:"name"`
	GroupIDs []string `json:"group_ids"`
}

func (s *Server) enroll(w http.ResponseWriter, r *http.Request) {
	var in enrollment
	if !decode(w, r, &in) {
		return
	}
	if len(in.GroupIDs) == 0 || len(in.GroupIDs) > 100 || len(in.Name) > 190 {
		fail(w, 400, "name and groups required")
		return
	}
	t := token()
	expires := time.Now().UTC().Add(15 * time.Minute)
	actor, _ := UserFromContext(r.Context())
	e := s.Store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		for _, gid := range in.GroupIDs {
			var n int
			if e := tx.QueryRowContext(r.Context(), s.q(`SELECT COUNT(*) FROM cp_groups WHERE id=?`), gid).Scan(&n); e != nil {
				return e
			}
			if n != 1 {
				return errors.New("unknown group")
			}
		}
		_, e := tx.ExecContext(r.Context(), s.q(`INSERT INTO cp_enrollments(token_hash,payload,expires_at) VALUES(?,?,?)`), digest(t), strJSON(in), expires.Unix())
		if e != nil {
			return e
		}
		return s.AuditTx(r.Context(), tx, actor.ID, "node.enrollment", "")
	})
	if e != nil {
		fail(w, 400, "invalid enrollment")
		return
	}
	reply(w, 201, map[string]any{"token": t, "expires_at": expires})
}
func (s *Server) registerNode(w http.ResponseWriter, r *http.Request) {
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	if !s.allow("register:"+ip, 30) {
		fail(w, 429, "rate limited")
		return
	}
	var in contract.Registration
	if !decode(w, r, &in) {
		return
	}
	if len(in.Name) > 190 || len(in.Capabilities) > 64 || len(in.Token) > 128 {
		fail(w, 400, "invalid registration")
		return
	}
	node := contract.Node{ID: id(), Name: in.Name, Version: in.Version, OS: in.OS, Arch: in.Arch, Capabilities: in.Capabilities, DesiredVersion: 1}
	credential := token()
	e := s.Store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		var p string
		if e := tx.QueryRowContext(r.Context(), s.q(`SELECT payload FROM cp_enrollments WHERE token_hash=? AND expires_at>?`), digest(in.Token), time.Now().Unix()).Scan(&p); e != nil {
			return e
		}
		var en enrollment
		if e := json.Unmarshal([]byte(p), &en); e != nil {
			return e
		}
		node.GroupIDs = en.GroupIDs
		if en.Name != "" {
			node.Name = en.Name
		}
		res, e := tx.ExecContext(r.Context(), s.q(`DELETE FROM cp_enrollments WHERE token_hash=?`), digest(in.Token))
		if e != nil {
			return e
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return errConflict
		}
		if _, e = tx.ExecContext(r.Context(), s.q(`INSERT INTO cp_nodes(id,name,token_hash,payload,desired_version,applied_version,apply_error,last_seen) VALUES(?,?,?,?,1,0,'',?)`), node.ID, node.Name, digest(credential), strJSON(node), time.Now().Unix()); e != nil {
			return e
		}
		for _, g := range en.GroupIDs {
			if _, e = tx.ExecContext(r.Context(), s.q(`INSERT INTO cp_node_groups(node_id,group_id) VALUES(?,?)`), node.ID, g); e != nil {
				return e
			}
		}
		return nil
	})
	if e != nil {
		fail(w, 401, "invalid or consumed enrollment")
		return
	}
	reply(w, 201, contract.Registered{NodeID: node.ID, Token: credential})
}
func (s *Server) agent(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, "Bearer ") {
			fail(w, 401, "node identity required")
			return
		}
		var node string
		if s.Store.DB.QueryRowContext(r.Context(), s.q(`SELECT id FROM cp_nodes WHERE token_hash=?`), digest(strings.TrimPrefix(h, "Bearer "))).Scan(&node) != nil {
			fail(w, 401, "node identity required")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), nodeKey{}, node)))
	}
}
func (s *Server) nodes(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFromContext(r.Context())
	where := ""
	args := []any{}
	if u.Role != "admin" {
		where = ` WHERE EXISTS(SELECT 1 FROM cp_node_groups ng JOIN cp_group_users gu ON gu.group_id=ng.group_id WHERE ng.node_id=n.id AND gu.user_id=?)`
		args = append(args, u.ID)
	}
	n, o := pages(r)
	var total int
	if s.Store.DB.QueryRowContext(r.Context(), s.q("SELECT COUNT(*) FROM cp_nodes n"+where), args...).Scan(&total) != nil {
		fail(w, 500, "query failed")
		return
	}
	args = append(args, n, o)
	rows, e := s.Store.DB.QueryContext(r.Context(), s.q(`SELECT payload,desired_version,applied_version,apply_error,last_seen FROM cp_nodes n`+where+` ORDER BY id LIMIT ? OFFSET ?`), args...)
	if e != nil {
		fail(w, 500, "query failed")
		return
	}
	defer rows.Close()
	items := []contract.Node{}
	for rows.Next() {
		var p string
		var d, a, seen int64
		var msg string
		if rows.Scan(&p, &d, &a, &msg, &seen) != nil {
			fail(w, 500, "query failed")
			return
		}
		var node contract.Node
		json.Unmarshal([]byte(p), &node)
		node.DesiredVersion = d
		node.AppliedVersion = a
		node.ApplyError = msg
		if seen > 0 {
			at := time.Unix(seen, 0).UTC()
			node.LastSeen = &at
		}
		if u.Role != "admin" {
			node.GroupIDs = nil
		}
		items = append(items, node)
	}
	reply(w, 200, map[string]any{"items": items, "total": total})
}
func (s *Server) config(w http.ResponseWriter, r *http.Request) {
	node := r.Context().Value(nodeKey{}).(string)
	if e := s.refreshLeases(r.Context(), node); e != nil {
		fail(w, 409, "lease refresh unavailable or entitlement exhausted")
		return
	}
	cfg := contract.Config{ContractVersion: contract.Version, NodeID: node, ValidUntil: time.Now().UTC().Add(24 * time.Hour), Rules: []contract.Rule{}}
	// Snapshot transaction prevents an old rule set being labeled with a newer version.
	tx, e := s.Store.DB.BeginTx(r.Context(), &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelSerializable})
	if e != nil {
		fail(w, 500, "config unavailable")
		return
	}
	defer tx.Rollback()
	if e = tx.QueryRowContext(r.Context(), s.q(`SELECT desired_version FROM cp_nodes WHERE id=?`), node).Scan(&cfg.Version); e != nil {
		fail(w, 500, "config unavailable")
		return
	}
	rows, e := tx.QueryContext(r.Context(), s.q(`SELECT r.payload,g.payload,u.disabled,u.role FROM cp_rules r JOIN cp_groups g ON g.id=r.group_id JOIN cp_users u ON u.id=r.user_id WHERE r.node_id=? AND r.deleted=0`), node)
	if e != nil {
		fail(w, 500, "config unavailable")
		return
	}
	for rows.Next() {
		var rp, gp, role string
		var disabled int
		if rows.Scan(&rp, &gp, &disabled, &role) != nil {
			rows.Close()
			fail(w, 500, "config unavailable")
			return
		}
		var rule contract.Rule
		var g contract.Group
		if json.Unmarshal([]byte(rp), &rule) != nil || json.Unmarshal([]byte(gp), &g) != nil {
			rows.Close()
			fail(w, 500, "config unavailable")
			return
		}
		if disabled != 0 || !rule.Enabled || rule.Lease == nil || !rule.Lease.ExpiresAt.After(time.Now()) || policyDenied(g, rule) || (role != "admin" && !contains(g.UserIDs, rule.UserID)) {
			continue
		}
		rule.BlockedProtocols = applicationBlocks(rule.BlockedProtocols, g.BlockedProtocols)
		if rule.Lease.ExpiresAt.Before(cfg.ValidUntil) {
			cfg.ValidUntil = rule.Lease.ExpiresAt
		}
		cfg.Rules = append(cfg.Rules, rule)
	}
	if e = rows.Err(); e != nil {
		rows.Close()
		fail(w, 500, "config unavailable")
		return
	}
	rows.Close()
	if tx.Commit() != nil {
		fail(w, 500, "config unavailable")
		return
	}
	reply(w, 200, cfg)
}
func (s *Server) ack(w http.ResponseWriter, r *http.Request) {
	var in contract.Ack
	if !decode(w, r, &in) {
		return
	}
	if len(in.Error) > 2000 || in.Version < 1 || in.AppliedVersion < 0 || in.AppliedVersion > in.Version || (in.Error == "" && in.AppliedVersion != in.Version) {
		fail(w, 400, "invalid ACK")
		return
	}
	node := r.Context().Value(nodeKey{}).(string)
	e := s.Store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		res, e := tx.ExecContext(r.Context(), s.q(`UPDATE cp_nodes SET applied_version=?,apply_error=?,last_seen=? WHERE id=? AND desired_version=? AND applied_version<=?`), in.AppliedVersion, in.Error, time.Now().Unix(), node, in.Version, in.AppliedVersion)
		if e != nil {
			return e
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return errConflict
		}
		if in.Error == "" {
			_, e = tx.ExecContext(r.Context(), s.q(`DELETE FROM cp_ports WHERE node_id=? AND rule_id IN(SELECT id FROM cp_rules WHERE node_id=? AND deleted=1 AND release_version<=?)`), node, node, in.AppliedVersion)
		}
		return e
	})
	if e != nil {
		fail(w, 409, "stale or invalid ACK")
		return
	}
	w.WriteHeader(204)
}
func (s *Server) probe(w http.ResponseWriter, r *http.Request) {
	var p contract.Probe
	if !decode(w, r, &p) {
		return
	}
	node := r.Context().Value(nodeKey{}).(string)
	if p.NodeID != "" && p.NodeID != node {
		fail(w, 403, "node mismatch")
		return
	}
	if p.SampledAt.IsZero() || p.SampledAt.After(time.Now().Add(time.Minute)) {
		fail(w, 400, "invalid sample time")
		return
	}
	p.NodeID = node
	s.mu.Lock()
	old, ok := s.probes[node]
	if !ok || p.SampledAt.After(old.SampledAt) {
		s.probes[node] = p
	}
	s.mu.Unlock() // live samples intentionally do not create per-sample DB writes
	if s.allow("heartbeat:"+node, 1) {
		_ = s.Store.Write(r.Context(), storage.Background, func(tx *sql.Tx) error {
			_, e := tx.ExecContext(r.Context(), s.q(`UPDATE cp_nodes SET last_seen=? WHERE id=?`), time.Now().Unix(), node)
			return e
		})
	}
	w.WriteHeader(204)
}
func (s *Server) visibleProbes(ctx context.Context, u contract.User) ([]contract.Probe, error) {
	allowed := map[string]bool{}
	if u.Role != "admin" {
		rows, e := s.Store.DB.QueryContext(ctx, s.q(`SELECT DISTINCT ng.node_id FROM cp_node_groups ng JOIN cp_group_users gu ON gu.group_id=ng.group_id WHERE gu.user_id=?`), u.ID)
		if e != nil {
			return nil, e
		}
		for rows.Next() {
			var id string
			if e = rows.Scan(&id); e != nil {
				rows.Close()
				return nil, e
			}
			allowed[id] = true
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return nil, e
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := []contract.Probe{}
	for n, p := range s.probes {
		if u.Role == "admin" || allowed[n] {
			if u.Role != "admin" {
				p.PublicIPs = nil
			}
			items = append(items, p)
		}
	}
	return items, nil
}
func (s *Server) probeList(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFromContext(r.Context())
	items, e := s.visibleProbes(r.Context(), u)
	if e != nil {
		fail(w, 500, "query failed")
		return
	}
	reply(w, 200, map[string]any{"items": items})
}
func (s *Server) probeEvents(w http.ResponseWriter, r *http.Request) {
	f, ok := w.(http.Flusher)
	if !ok {
		fail(w, 500, "stream unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		u, e := s.Authenticate(r)
		if e != nil {
			return
		}
		items, e := s.visibleProbes(r.Context(), u)
		if e != nil {
			return
		}
		http.NewResponseController(w).SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, e = fmt.Fprintf(w, "event: probes\ndata: %s\n\n", strJSON(map[string]any{"items": items})); e != nil {
			return
		}
		f.Flush()
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
		}
	}
}
func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	var batch contract.UsageBatch
	if !decode(w, r, &batch) {
		return
	}
	if len(batch.Records) > 500 {
		fail(w, 400, "maximum 500 records per batch")
		return
	}
	node := r.Context().Value(nodeKey{}).(string)
	accepted := []string{}
	e := s.Store.Write(r.Context(), storage.Normal, func(tx *sql.Tx) error {
		for _, u := range batch.Records {
			if u.NodeID != node || u.ID == "" || len(u.ID) > 128 || u.UploadBytes < 0 || u.DownloadBytes < 0 || u.UploadBytes > math.MaxInt64-u.DownloadBytes || u.StartedAt.IsZero() || u.EndedAt.Before(u.StartedAt) || u.EndedAt.After(time.Now().Add(time.Minute)) {
				return errors.New("invalid usage record")
			}
			payload := strJSON(u)
			var old string
			e := tx.QueryRowContext(r.Context(), s.q(`SELECT payload FROM cp_usage WHERE id=?`), u.ID).Scan(&old)
			if e == nil {
				if old != payload {
					return errConflict
				}
				accepted = append(accepted, u.ID)
				continue
			}
			if !errors.Is(e, sql.ErrNoRows) {
				return e
			}
			var retired int
			if e = tx.QueryRowContext(r.Context(), s.q(`SELECT COUNT(*) FROM cp_lease_retirements WHERE id=?`), u.LeaseID).Scan(&retired); e != nil {
				return e
			}
			if retired != 0 {
				return errors.New("lease retired")
			}
			var rn, nn, ent string
			var budget, used, expiry int64
			if e = tx.QueryRowContext(r.Context(), s.q(`SELECT rule_id,node_id,entitlement_id,bytes_allocated,bytes_used,expires_at FROM cp_rule_leases WHERE id=?`), u.LeaseID).Scan(&rn, &nn, &ent, &budget, &used, &expiry); e != nil {
				return e
			}
			if rn != u.RuleID || nn != node || ent != u.EntitlementID || u.EndedAt.Unix() > expiry || u.UploadBytes+u.DownloadBytes > budget-used {
				return errors.New("usage outside lease")
			}
			amount := u.UploadBytes + u.DownloadBytes
			res, e := tx.ExecContext(r.Context(), s.q(`UPDATE cp_rule_leases SET bytes_used=bytes_used+? WHERE id=? AND bytes_used=?`), amount, u.LeaseID, used)
			if e != nil {
				return e
			}
			n, _ := res.RowsAffected()
			if n != 1 {
				return errConflict
			}
			if settle, ok := s.opts.Entitlements.(interface {
				SettleUsage(context.Context, *sql.Tx, contract.UsageRecord) error
			}); ok && ent != "admin-test" {
				if e = settle.SettleUsage(r.Context(), tx, u); e != nil {
					return e
				}
			}
			if _, e = tx.ExecContext(r.Context(), s.q(`INSERT INTO cp_usage(id,node_id,rule_id,lease_id,payload,received_at,settled) VALUES(?,?,?,?,?,?,?)`), u.ID, node, u.RuleID, u.LeaseID, payload, time.Now().Unix(), 1); e != nil {
				return e
			}
			accepted = append(accepted, u.ID)
		}
		return nil
	})
	if e != nil {
		fail(w, 409, "usage validation or persistence failed")
		return
	}
	reply(w, 200, map[string]any{"accepted": accepted})
}
