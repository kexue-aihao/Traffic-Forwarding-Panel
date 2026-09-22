package platform

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

func (s *Server) lockRuleOwner(ctx context.Context, tx *sql.Tx, user string) error {
	q := "SELECT disabled FROM cp_users WHERE id=?"
	if s.Store.Dialect != "sqlite" {
		q += " FOR UPDATE"
	}
	var disabled int
	if err := tx.QueryRowContext(ctx, s.q(q), user).Scan(&disabled); err != nil {
		return err
	}
	if disabled != 0 {
		return errors.New("owner disabled")
	}
	return nil
}

func (s *Server) accountLimits(ctx context.Context, tx *sql.Tx, user string) (contract.ResourceLimits, error) {
	if s.opts.ResourceLimits == nil {
		return contract.ResourceLimits{}, nil
	}
	return s.opts.ResourceLimits(ctx, tx, user)
}

func (s *Server) resourceCapabilities(ctx context.Context, tx *sql.Tx, rule contract.Rule) (bool, error) {
	limits, err := s.accountLimits(ctx, tx, rule.UserID)
	if err != nil {
		return false, err
	}
	if limits == (contract.ResourceLimits{}) && !rule.Advanced() && rule.ProxyProtocol == nil {
		return true, nil
	}
	var raw string
	if err = tx.QueryRowContext(ctx, s.q("SELECT payload FROM cp_nodes WHERE id=?"), rule.NodeID).Scan(&raw); err != nil {
		return false, err
	}
	var node contract.Node
	if err = json.Unmarshal([]byte(raw), &node); err != nil {
		return false, err
	}
	return (rule.ProxyProtocol == nil || contains(node.Capabilities, "proxy-protocol-v1")) && (limits == (contract.ResourceLimits{}) || contains(node.Capabilities, "resource-limits-v1")) && (!rule.Advanced() || contains(node.Capabilities, "advanced-routing-v1")) && (rule.Transport != "direct-tls" || contains(node.Capabilities, "direct-tls-v1")), nil
}

// Existing rules are ordered by immutable ID on every node. A downgrade keeps
// only the first N eligible; disabled drafts still consume a rule slot.
func (s *Server) withinPlanRuleLimit(ctx context.Context, tx *sql.Tx, rule contract.Rule, maximum int) (bool, error) {
	if maximum == 0 {
		return true, nil
	}
	var rank int
	err := tx.QueryRowContext(ctx, s.q("SELECT COUNT(*) FROM cp_rules WHERE user_id=? AND deleted=0 AND id<=?"), rule.UserID, rule.ID).Scan(&rank)
	return rank <= maximum, err
}
