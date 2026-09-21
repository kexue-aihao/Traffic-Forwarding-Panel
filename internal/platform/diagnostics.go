package platform

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
)

func (s *Server) createDiagnostic(w http.ResponseWriter, r *http.Request) {
	actor, _ := UserFromContext(r.Context())
	settings, err := s.SiteSettings(r.Context())
	if err != nil || !settings.DiagnosticsEnabled {
		fail(w, 403, "diagnostics disabled")
		return
	}
	if !s.allow("diagnostic:"+actor.ID, settings.DiagnosticsPerMinute) {
		fail(w, 429, "diagnostic rate limited")
		return
	}
	var out contract.Diagnostic
	err = s.Store.Write(r.Context(), storage.Normal, func(tx *sql.Tx) error {
		var raw, owner, node string
		var seen int64
		if err := tx.QueryRowContext(r.Context(), s.q("SELECT r.payload,r.user_id,r.node_id,n.last_seen FROM cp_rules r JOIN cp_nodes n ON n.id=r.node_id WHERE r.id=? AND r.deleted=0"), r.PathValue("id")).Scan(&raw, &owner, &node, &seen); err != nil {
			return err
		}
		if actor.Role != "admin" && actor.ID != owner {
			return sql.ErrNoRows
		}
		var rule contract.Rule
		if err := json.Unmarshal([]byte(raw), &rule); err != nil {
			return err
		}
		var access int
		if err := tx.QueryRowContext(r.Context(), s.q("SELECT COUNT(*) FROM cp_group_users WHERE group_id=? AND user_id=?"), rule.GroupID, actor.ID).Scan(&access); err != nil {
			return err
		}
		if actor.Role != "admin" && access == 0 {
			return sql.ErrNoRows
		}
		if rule.Network != "tcp" {
			return errors.New("TCP diagnostic required; UDP connect alone cannot prove reachability")
		}
		if seen == 0 || time.Now().Unix()-seen > 90 || !rule.Enabled {
			return errors.New("online node and enabled rule required")
		}
		var nodeRaw string
		if err := tx.QueryRowContext(r.Context(), s.q("SELECT payload FROM cp_nodes WHERE id=?"), node).Scan(&nodeRaw); err != nil {
			return err
		}
		var info contract.Node
		if json.Unmarshal([]byte(nodeRaw), &info) != nil || !contains(info.Capabilities, "diagnostics-v1") {
			return errors.New("upgrade Agent for network diagnostics")
		}
		lockNode := "SELECT id FROM cp_nodes WHERE id=?"
		if s.Store.Dialect != "sqlite" {
			lockNode += " FOR UPDATE"
		}
		var locked string
		if err := tx.QueryRowContext(r.Context(), s.q(lockNode), node).Scan(&locked); err != nil {
			return err
		}
		var pending int
		if err := tx.QueryRowContext(r.Context(), s.q("SELECT COUNT(*) FROM cp_diagnostics WHERE node_id=? AND status IN ('pending','running') AND created_at>?"), node, time.Now().Add(-2*time.Minute).Unix()).Scan(&pending); err != nil {
			return err
		}
		if pending >= 100 {
			return errors.New("diagnostic node queue full")
		}
		out = contract.Diagnostic{ID: id(), RuleID: rule.ID, RuleVersion: rule.Version, Status: "pending", CreatedAt: time.Now().UTC(), Checks: []contract.DiagnosticCheck{{Stage: "panel", OK: true, Detail: "authorized rule queued"}}}
		if _, err := tx.ExecContext(r.Context(), s.q("DELETE FROM cp_diagnostics WHERE created_at<?"), time.Now().Add(-7*24*time.Hour).Unix()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(r.Context(), s.q("INSERT INTO cp_diagnostics(id,user_id,node_id,rule_id,payload,status,claim_token,created_at,claimed_at) VALUES(?,?,?,?,?,'pending','',?,0)"), out.ID, actor.ID, node, rule.ID, strJSON(out), out.CreatedAt.Unix()); err != nil {
			return err
		}
		return s.AuditTx(r.Context(), tx, actor.ID, "rule.network_diagnostic", rule.ID)
	})
	if err != nil {
		fail(w, 409, "diagnostic unavailable: check access, node capability, TCP rule and online status")
		return
	}
	reply(w, 202, out)
}
func (s *Server) diagnostic(w http.ResponseWriter, r *http.Request) {
	actor, _ := UserFromContext(r.Context())
	var raw, owner, group, status string
	var created int64
	err := s.Store.DB.QueryRowContext(r.Context(), s.q("SELECT d.payload,d.user_id,r.group_id,d.status,d.created_at FROM cp_diagnostics d JOIN cp_rules r ON r.id=d.rule_id WHERE d.id=? AND r.deleted=0"), r.PathValue("id")).Scan(&raw, &owner, &group, &status, &created)
	if err != nil || actor.Role != "admin" && actor.ID != owner {
		fail(w, 404, "diagnostic not found")
		return
	}
	if actor.Role != "admin" {
		var n int
		if s.Store.DB.QueryRowContext(r.Context(), s.q("SELECT COUNT(*) FROM cp_group_users WHERE group_id=? AND user_id=?"), group, actor.ID).Scan(&n) != nil || n == 0 {
			fail(w, 404, "diagnostic not found")
			return
		}
	}
	var out contract.Diagnostic
	if json.Unmarshal([]byte(raw), &out) != nil {
		fail(w, 500, "diagnostic unavailable")
		return
	}
	out.Status = status
	out.Claim = ""
	if (status == "pending" || status == "running") && time.Now().Unix()-created > 120 {
		out.Status = "expired"
	}
	reply(w, 200, out)
}
func (s *Server) claimDiagnostic(w http.ResponseWriter, r *http.Request) {
	settings, err := s.SiteSettings(r.Context())
	if err != nil {
		fail(w, 503, "settings unavailable")
		return
	}
	if !settings.DiagnosticsEnabled {
		reply(w, 200, map[string]any{"diagnostic": nil})
		return
	}
	node := r.Context().Value(nodeKey{}).(string)
	var out *contract.Diagnostic
	err = s.Store.Write(r.Context(), storage.Normal, func(tx *sql.Tx) error {
		// Recheck owner and current group access at dispatch. Requests expire in 2 minutes.
		q := `SELECT d.id,d.payload FROM cp_diagnostics d JOIN cp_rules r ON r.id=d.rule_id JOIN cp_users u ON u.id=d.user_id WHERE d.node_id=? AND d.created_at>? AND (d.status='pending' OR (d.status='running' AND d.claimed_at<?)) AND r.deleted=0 AND u.disabled=0 AND (u.role='admin' OR EXISTS(SELECT 1 FROM cp_group_users gu WHERE gu.group_id=r.group_id AND gu.user_id=u.id)) ORDER BY d.created_at,d.id LIMIT 1`
		var id, raw string
		e := tx.QueryRowContext(r.Context(), s.q(q), node, time.Now().Add(-2*time.Minute).Unix(), time.Now().Add(-30*time.Second).Unix()).Scan(&id, &raw)
		if errors.Is(e, sql.ErrNoRows) {
			return nil
		}
		if e != nil {
			return e
		}
		var v contract.Diagnostic
		if e = json.Unmarshal([]byte(raw), &v); e != nil {
			return e
		}
		v.Claim = token()
		v.Status = "running"
		res, e := tx.ExecContext(r.Context(), s.q("UPDATE cp_diagnostics SET status='running',claim_token=?,claimed_at=? WHERE id=? AND (status='pending' OR (status='running' AND claimed_at<?))"), v.Claim, time.Now().Unix(), id, time.Now().Add(-30*time.Second).Unix())
		if e != nil {
			return e
		}
		n, _ := res.RowsAffected()
		if n == 1 {
			out = &v
		}
		return nil
	})
	if err != nil {
		fail(w, 500, "diagnostic queue unavailable")
		return
	}
	reply(w, 200, map[string]any{"diagnostic": out})
}
func (s *Server) finishDiagnostic(w http.ResponseWriter, r *http.Request) {
	node := r.Context().Value(nodeKey{}).(string)
	var in contract.Diagnostic
	if !decode(w, r, &in) {
		return
	}
	if len(in.Checks) < 1 || len(in.Checks) > 6 {
		fail(w, 400, "invalid checks")
		return
	}
	for i := range in.Checks {
		c := &in.Checks[i]
		if !contains([]string{"entry_config", "entry_dns", "entry_target", "exit_path"}, c.Stage) || c.Milliseconds < 0 || c.Milliseconds > 20000 {
			fail(w, 400, "invalid check")
			return
		}
		c.Detail = "connection failed"
		if c.OK {
			c.Detail = "passed"
		}
	}
	err := s.Store.Write(r.Context(), storage.Normal, func(tx *sql.Tx) error {
		var raw, status string
		if err := tx.QueryRowContext(r.Context(), s.q("SELECT payload,status FROM cp_diagnostics WHERE id=? AND node_id=? AND claim_token=? AND created_at>?"), in.ID, node, in.Claim, time.Now().Add(-2*time.Minute).Unix()).Scan(&raw, &status); err != nil {
			return err
		}
		if status == "completed" {
			return nil
		}
		var v contract.Diagnostic
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			return err
		}
		v.Status = "completed"
		v.Checks = append(v.Checks, in.Checks...)
		_, err := tx.ExecContext(r.Context(), s.q("UPDATE cp_diagnostics SET status='completed',payload=? WHERE id=? AND claim_token=?"), strJSON(v), in.ID, in.Claim)
		return err
	})
	if err != nil {
		fail(w, 409, "diagnostic expired or claim changed")
		return
	}
	w.WriteHeader(204)
}
