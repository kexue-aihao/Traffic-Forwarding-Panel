package platform

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
)

func (s *Server) deleteGroup(w http.ResponseWriter, r *http.Request) {
	version, err := strconv.ParseInt(r.URL.Query().Get("version"), 10, 64)
	if err != nil || version < 1 {
		fail(w, 400, "请提供设备组版本")
		return
	}
	target := r.PathValue("id")
	actor, _ := UserFromContext(r.Context())
	err = s.Store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		ctx := r.Context()
		group, err := s.groupTx(ctx, tx, target)
		if err != nil {
			return err
		}
		if group.Version != version {
			return fmt.Errorf("%w：设备组已变更，请刷新后重试", errConflict)
		}
		// Chain and advanced references live in JSON, so compare decoded IDs,
		// never a substring match against the stored payload.
		rows, err := tx.QueryContext(ctx, s.q("SELECT payload FROM cp_groups WHERE id<>?"), target)
		if err != nil {
			return err
		}
		for rows.Next() {
			var raw string
			var other contract.Group
			if err = rows.Scan(&raw); err == nil {
				err = json.Unmarshal([]byte(raw), &other)
			}
			if err == nil && (contains(other.ChainGroupIDs, target) || other.Advanced != nil && (contains(other.Advanced.IPv6Group, target) || contains(other.Advanced.ReverseGroup, target))) {
				err = fmt.Errorf("%w：设备组仍被「%s」引用，请先修改该组的链式出口或高级设置", errConflict, other.Name)
			}
			if err != nil {
				rows.Close()
				return err
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		exits := map[string]bool{}
		rows, err = tx.QueryContext(ctx, s.q("SELECT id FROM cp_exits WHERE group_id=?"), target)
		if err != nil {
			return err
		}
		for rows.Next() {
			var exitID string
			if err = rows.Scan(&exitID); err != nil {
				rows.Close()
				return err
			}
			exits[exitID] = true
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		// Include deleted rules until their Agent acknowledges removal. Shared
		// TLS child rules need this check even though they own no port row.
		rows, err = tx.QueryContext(ctx, "SELECT r.payload,r.deleted,r.release_version,n.applied_version FROM cp_rules r JOIN cp_nodes n ON n.id=r.node_id")
		if err != nil {
			return err
		}
		for rows.Next() {
			var raw string
			var rule contract.Rule
			var deleted int
			var release, applied int64
			if err = rows.Scan(&raw, &deleted, &release, &applied); err == nil {
				err = json.Unmarshal([]byte(raw), &rule)
			}
			if err == nil && (rule.GroupID == target || rule.ExitGroupID == target || exits[rule.ExitID] || exits[rule.SelectedExitID]) {
				if deleted == 0 {
					err = fmt.Errorf("%w：设备组仍被转发规则「%s」使用，请先删除规则或修改出口", errConflict, rule.Name)
				} else if applied < release {
					err = fmt.Errorf("%w：已删除的转发规则正在等待节点确认停止，请稍后重试", errConflict)
				}
			}
			if err != nil {
				rows.Close()
				return err
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		var ports int
		if err = tx.QueryRowContext(ctx, s.q("SELECT COUNT(*) FROM cp_ports p JOIN cp_rules r ON r.id=p.rule_id WHERE r.group_id=?"), target).Scan(&ports); err != nil {
			return err
		}
		if ports != 0 {
			return fmt.Errorf("%w：转发端口尚未释放，请等待节点确认停止", errConflict)
		}
		query := "SELECT n.payload FROM cp_nodes n JOIN cp_node_groups ng ON ng.node_id=n.id WHERE ng.group_id=? ORDER BY n.id"
		if s.Store.Dialect != "sqlite" {
			query += " FOR UPDATE"
		}
		rows, err = tx.QueryContext(ctx, s.q(query), target)
		if err != nil {
			return err
		}
		nodes := []contract.Node{}
		for rows.Next() {
			var raw string
			var node contract.Node
			if err = rows.Scan(&raw); err == nil {
				err = json.Unmarshal([]byte(raw), &node)
			}
			if err != nil {
				rows.Close()
				return err
			}
			node.GroupIDs = slices.DeleteFunc(node.GroupIDs, func(id string) bool { return id == target })
			nodes = append(nodes, node)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, node := range nodes {
			if _, err = tx.ExecContext(ctx, s.q("UPDATE cp_nodes SET payload=?,desired_version=desired_version+1 WHERE id=?"), strJSON(node), node.ID); err != nil {
				return err
			}
		}
		// Retain leases and usage facts: late usage uploads and settlement use
		// those records independently of the removed rule tombstones.
		for _, query := range []string{
			"DELETE FROM cp_rules WHERE group_id=? AND deleted=1",
			"DELETE FROM cp_exits WHERE group_id=?",
			"DELETE FROM cp_node_groups WHERE group_id=?",
			"DELETE FROM cp_group_users WHERE group_id=?",
			"DELETE FROM cp_group_identity_groups WHERE group_id=?",
		} {
			if _, err = tx.ExecContext(ctx, s.q(query), target); err != nil {
				return err
			}
		}
		result, err := tx.ExecContext(ctx, s.q("DELETE FROM cp_groups WHERE id=? AND version=?"), target, version)
		if err != nil {
			return err
		}
		if count, err := result.RowsAffected(); err != nil {
			return err
		} else if count != 1 {
			return errConflict
		}
		return s.AuditTx(ctx, tx, actor.ID, "group.delete", target)
	})
	if err != nil {
		switch {
		case errors.Is(err, sql.ErrNoRows):
			fail(w, 404, "设备组不存在")
		case errors.Is(err, errConflict):
			fail(w, 409, err.Error())
		default:
			fail(w, 409, "设备组删除失败，关联数据可能已变更，请刷新后重试")
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
