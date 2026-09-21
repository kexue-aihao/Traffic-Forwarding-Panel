package platform

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

func sharedParent(r contract.Rule) string {
	if r.SharedTLS != nil {
		return r.SharedTLS.ParentID
	}
	return ""
}

// Serialize every shared-port edit on its physical node, including operations
// by different group administrators. Children never own a second port record.
func (s *Server) validateSharedTx(ctx context.Context, tx *sql.Tx, r contract.Rule) error {
	q := "SELECT id FROM cp_nodes WHERE id=?"
	if s.Store.Dialect != "sqlite" {
		q += " FOR UPDATE"
	}
	var node string
	if e := tx.QueryRowContext(ctx, s.q(q), r.NodeID).Scan(&node); e != nil {
		return e
	}
	rows, e := tx.QueryContext(ctx, s.q("SELECT payload FROM cp_rules WHERE node_id=? AND deleted=0"), r.NodeID)
	if e != nil {
		return e
	}
	var siblings []contract.Rule
	for rows.Next() {
		var raw string
		var v contract.Rule
		if e = rows.Scan(&raw); e != nil {
			rows.Close()
			return e
		}
		if e = json.Unmarshal([]byte(raw), &v); e != nil {
			rows.Close()
			return e
		}
		siblings = append(siblings, v)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	parentFound := sharedParent(r) == ""
	count := 0
	for _, v := range siblings {
		if v.ID == r.ID {
			continue
		}
		if sharedParent(v) == r.ID && (r.SharedTLS == nil || sharedParent(r) != "") {
			return errors.New("shared TLS parent still has children")
		}
		if v.ID == sharedParent(r) {
			parentFound = v.SharedTLS != nil && sharedParent(v) == "" && v.UserID == r.UserID && v.GroupID == r.GroupID && v.Listen == r.Listen && v.Network == r.Network
		}
		if r.SharedTLS != nil && v.SharedTLS != nil && v.Listen == r.Listen {
			count++
			if r.SharedTLS.ServerName == v.SharedTLS.ServerName {
				return errors.New("SNI already reserved")
			}
		}
	}
	if !parentFound {
		return errors.New("shared TLS parent is unavailable or belongs to another owner/placement")
	}
	if count >= 64 {
		return errors.New("at most 64 SNI routes per port")
	}
	return nil
}

func filterSharedChildren(rules []contract.Rule) []contract.Rule {
	roots := map[string]bool{}
	for _, r := range rules {
		if r.SharedTLS != nil && r.SharedTLS.ParentID == "" {
			roots[r.ID] = true
		}
	}
	result := rules[:0]
	for _, r := range rules {
		if sharedParent(r) == "" || roots[sharedParent(r)] {
			result = append(result, r)
		}
	}
	return result
}
