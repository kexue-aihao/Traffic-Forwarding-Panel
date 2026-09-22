package platform

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

func (s *Server) groupTx(ctx context.Context, tx *sql.Tx, id string) (contract.Group, error) {
	var g contract.Group
	var raw string
	if err := tx.QueryRowContext(ctx, s.q("SELECT payload FROM cp_groups WHERE id=?"), id).Scan(&raw); err != nil {
		return g, err
	}
	err := json.Unmarshal([]byte(raw), &g)
	return g, err
}

func entryPolicyDenied(g contract.Group, rule contract.Rule) bool {
	if !g.CanEnter() || policyDenied(g, rule) {
		return true
	}
	direct := rule.Transport == "direct" || rule.Transport == "direct-tls"
	return g.DirectPolicy == "forbid" && direct || g.DirectPolicy == "force" && !direct
}

func (s *Server) validateGroupTypeTx(ctx context.Context, tx *sql.Tx, g contract.Group, create bool) error {
	if !create {
		old, err := s.groupTx(ctx, tx, g.ID)
		if err != nil {
			return err
		}
		if old.Type != "" && old.Type != g.Type {
			return errors.New("设备类型创建后不可修改")
		}
		if old.Type == "" && g.Type != "" {
			var rules, exits, nodes int
			if err := tx.QueryRowContext(ctx, s.q("SELECT COUNT(*) FROM cp_rules WHERE group_id=? AND deleted=0"), g.ID).Scan(&rules); err != nil {
				return err
			}
			if err := tx.QueryRowContext(ctx, s.q("SELECT COUNT(*) FROM cp_exits WHERE group_id=?"), g.ID).Scan(&exits); err != nil {
				return err
			}
			if err := tx.QueryRowContext(ctx, s.q("SELECT COUNT(*) FROM cp_node_groups WHERE group_id=?"), g.ID).Scan(&nodes); err != nil {
				return err
			}
			if !g.CanEnter() && rules > 0 || !g.CanHostExit() && exits > 0 || !g.CanEnroll() && nodes > 0 {
				return errors.New("现有规则、出口或设备与所选类型冲突，请先调整关联")
			}
		}
	}
	if g.Type == contract.GroupChainExit {
		seen := map[string]bool{g.ID: true}
		for _, id := range g.ChainGroupIDs {
			if id == "" || seen[id] {
				return errors.New("链式出口不能包含空出口、自身或重复出口")
			}
			seen[id] = true
			hop, err := s.groupTx(ctx, tx, id)
			if err != nil {
				return errors.New("链式出口引用了不存在的设备组")
			}
			if !hop.CanHostExit() {
				return errors.New("链式出口的每一跳必须是出口设备组")
			}
		}
	}
	return nil
}
