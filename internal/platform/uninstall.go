package platform

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

// Keep the node and operation as tombstones for usage, historical rule foreign
// keys and idempotent acknowledgements. Only a confirmed uninstall hides it.
const liveNodeSQL = "NOT EXISTS(SELECT 1 FROM cp_node_operations removed WHERE removed.node_id=n.id AND removed.kind='uninstall' AND removed.status='succeeded')"

func (s *Server) lockNodeTx(ctx context.Context, tx *sql.Tx, node string) error {
	query := "SELECT id FROM cp_nodes WHERE id=?"
	if s.Store.Dialect != "sqlite" {
		query += " FOR UPDATE"
	}
	var found string
	return tx.QueryRowContext(ctx, s.q(query), node).Scan(&found)
}

func (s *Server) nodeAvailableTx(ctx context.Context, tx *sql.Tx, node string) error {
	if e := s.lockNodeTx(ctx, tx, node); e != nil {
		return e
	}
	query := "SELECT id FROM cp_node_operations WHERE node_id=? AND kind='uninstall' AND (status IN ('running','succeeded') OR (status='pending' AND expires_at>?)) LIMIT 1"
	if s.Store.Dialect != "sqlite" {
		query += " FOR UPDATE"
	}
	var operation string
	e := tx.QueryRowContext(ctx, s.q(query), node, time.Now().Unix()).Scan(&operation)
	if e == nil {
		return errors.New("设备正在卸载或已卸载，不能再分配规则或执行运维")
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	return nil
}

func (s *Server) lockUninstallGroups(ctx context.Context, tx *sql.Tx, node string) error {
	// Match the group -> node lock order used when allocating rules/exits.
	rows, e := tx.QueryContext(ctx, s.q("SELECT group_id FROM cp_node_groups WHERE node_id=? ORDER BY group_id"), node)
	if e != nil {
		return e
	}
	var groups []string
	for rows.Next() {
		var g string
		if e = rows.Scan(&g); e != nil {
			break
		}
		groups = append(groups, g)
	}
	if e == nil {
		e = rows.Err()
	}
	rows.Close()
	if e != nil {
		return e
	}
	for _, g := range groups {
		if _, e = s.groupTx(ctx, tx, g); e != nil {
			return e
		}
	}
	return nil
}

func (s *Server) canUninstallTx(ctx context.Context, tx *sql.Tx, node string) error {
	// Exit groups (including chain parents) must be unused. This also protects
	// downstream hops whose selected exit is represented inside a tunnel JSON.
	groups := map[string]bool{}
	locking := ""
	if s.Store.Dialect != "sqlite" {
		locking = " FOR UPDATE"
	}
	rows, e := tx.QueryContext(ctx, s.q("SELECT group_id FROM cp_exits WHERE node_id=?"+locking), node)
	if e != nil {
		return e
	}
	for rows.Next() {
		var g string
		if e = rows.Scan(&g); e != nil {
			break
		}
		groups[g] = true
	}
	if e == nil {
		e = rows.Err()
	}
	rows.Close()
	if e != nil {
		return e
	}
	rows, e = tx.QueryContext(ctx, "SELECT payload FROM cp_groups"+locking)
	if e != nil {
		return e
	}
	var all []contract.Group
	for rows.Next() {
		var raw string
		var g contract.Group
		if e = rows.Scan(&raw); e == nil {
			e = json.Unmarshal([]byte(raw), &g)
		}
		if e != nil {
			break
		}
		all = append(all, g)
	}
	if e == nil {
		e = rows.Err()
	}
	rows.Close()
	if e != nil {
		return e
	}
	for changed := true; changed; {
		changed = false
		for _, g := range all {
			for _, child := range g.ChainGroupIDs {
				if groups[child] && !groups[g.ID] {
					groups[g.ID] = true
					changed = true
				}
			}
		}
	}
	rows, e = tx.QueryContext(ctx, "SELECT r.payload,r.deleted,r.release_version,n.applied_version FROM cp_rules r JOIN cp_nodes n ON n.id=r.node_id"+locking)
	if e != nil {
		return e
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		var deleted int
		var release, applied int64
		var rule contract.Rule
		if e = rows.Scan(&raw, &deleted, &release, &applied); e != nil {
			return e
		}
		if e = json.Unmarshal([]byte(raw), &rule); e != nil {
			return e
		}
		if (rule.NodeID == node || groups[rule.ExitGroupID]) && (deleted == 0 || applied < release) {
			return errors.New("设备或其出口组仍有转发规则，请先移除相关规则并等待节点确认停止，再卸载")
		}
	}
	return rows.Err()
}
