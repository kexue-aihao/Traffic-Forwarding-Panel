package platform

import (
	"encoding/json"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"net/http"
)

// Audit reads the same immutable facts used for settlement, including recovery
// responsibility. Credentials and live reservations are never returned.
func (s *Server) usageAudit(w http.ResponseWriter, r *http.Request) {
	n, offset := pages(r)
	where := ""
	args := []any{}
	if rule := r.URL.Query().Get("rule_id"); rule != "" {
		if len(rule) > 64 {
			fail(w, 400, "invalid rule ID")
			return
		}
		where = " WHERE rule_id=?"
		args = append(args, rule)
	}
	var total int
	if e := s.Store.DB.QueryRowContext(r.Context(), s.q("SELECT COUNT(*) FROM cp_usage"+where), args...).Scan(&total); e != nil {
		fail(w, 500, "usage audit unavailable")
		return
	}
	args = append(args, n, offset)
	rows, e := s.Store.DB.QueryContext(r.Context(), s.q("SELECT payload FROM cp_usage"+where+" ORDER BY received_at DESC,id DESC LIMIT ? OFFSET ?"), args...)
	if e != nil {
		fail(w, 500, "usage audit unavailable")
		return
	}
	defer rows.Close()
	items := []contract.UsageRecord{}
	for rows.Next() {
		var raw string
		var u contract.UsageRecord
		if rows.Scan(&raw) != nil || json.Unmarshal([]byte(raw), &u) != nil {
			fail(w, 500, "usage audit unavailable")
			return
		}
		items = append(items, u)
	}
	if rows.Err() != nil {
		fail(w, 500, "usage audit unavailable")
		return
	}
	reply(w, 200, map[string]any{"items": items, "total": total})
}
