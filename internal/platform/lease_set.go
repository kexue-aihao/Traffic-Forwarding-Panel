package platform

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
)

var (
	errPrefetchStale     = errors.New("lease prefetch requires current configuration")
	errPrefetchForbidden = errors.New("lease prefetch forbidden by current policy")
)

func (s *Server) liveLeaseTx(ctx context.Context, tx *sql.Tx, user string, l *contract.Lease) (bool, error) {
	if l == nil || !time.Now().Before(l.ExpiresAt) {
		return false, nil
	}
	var retired int
	if e := tx.QueryRowContext(ctx, s.q("SELECT COUNT(*) FROM cp_lease_retirements WHERE id=?"), l.ID).Scan(&retired); e != nil {
		return false, e
	}
	if retired != 0 {
		return false, nil
	}
	if l.EntitlementID == "admin-test" {
		return true, nil
	}
	if s.opts.LeaseCurrent == nil {
		return false, errors.New("lease current validation required")
	}
	return s.opts.LeaseCurrent(ctx, tx, user, l)
}

func (s *Server) allocateStandbyTx(ctx context.Context, tx *sql.Tx, rule contract.Rule, multiplier string, budget int64) (*contract.Lease, error) {
	a, ok := s.opts.Entitlements.(interface {
		AllocateBudgetWithMultiplier(context.Context, *sql.Tx, string, string, string, string, int64) (*contract.Lease, error)
	})
	if !ok || rule.Lease == nil || rule.Lease.EntitlementID == "admin-test" {
		return nil, nil
	}
	var outstanding int
	if e := tx.QueryRowContext(ctx, s.q("SELECT COUNT(*) FROM cp_rule_leases l WHERE l.rule_id=? AND NOT EXISTS(SELECT 1 FROM cp_lease_retirements r WHERE r.id=l.id)"), rule.ID).Scan(&outstanding); e != nil {
		return nil, e
	}
	if outstanding >= 4 {
		return nil, nil
	}
	l, e := a.AllocateBudgetWithMultiplier(ctx, tx, rule.UserID, rule.ID, rule.NodeID, multiplier, budget)
	if errors.Is(e, contract.ErrEntitlementUnavailable) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	if l == nil || l.Bytes <= 0 || !time.Now().Before(l.ExpiresAt) || l.ExpiresAt.After(time.Now().Add(5*time.Minute+time.Second)) || l.EntitlementID != rule.Lease.EntitlementID {
		return nil, errors.New("invalid standby allocation")
	}
	_, e = tx.ExecContext(ctx, s.q("INSERT INTO cp_rule_leases(id,rule_id,node_id,entitlement_id,bytes_allocated,bytes_used,expires_at) VALUES(?,?,?,?,?,0,?)"), l.ID, rule.ID, rule.NodeID, l.EntitlementID, l.Bytes, l.ExpiresAt.Unix())
	return l, e
}

func (s *Server) prefetchLease(w http.ResponseWriter, r *http.Request) {
	var in struct {
		RuleID    string `json:"rule_id"`
		LeaseID   string `json:"lease_id"`
		RawBudget int64  `json:"raw_budget,string"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.RawBudget < 1 || in.RawBudget > 256<<20 {
		fail(w, 400, "raw budget outside 1..256 MiB")
		return
	}
	node := r.Context().Value(nodeKey{}).(string)
	var leaseID string
	e := s.Store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		var nodeRaw string
		nodeQuery := "SELECT payload FROM cp_nodes WHERE id=?"
		if s.Store.Dialect != "sqlite" {
			nodeQuery += " FOR UPDATE"
		}
		if e := tx.QueryRowContext(r.Context(), s.q(nodeQuery), node).Scan(&nodeRaw); e != nil {
			return e
		}
		var info contract.Node
		if json.Unmarshal([]byte(nodeRaw), &info) != nil || !contains(info.Capabilities, "lease-set-v1") {
			return errPrefetchForbidden
		}
		query := "SELECT payload FROM cp_rules WHERE id=? AND node_id=? AND deleted=0"
		// Match rule-save lock order: lock the owner before the rule row. The
		// second read below detects an owner/config change during acquisition.
		var initialRaw string
		if e := tx.QueryRowContext(r.Context(), s.q(query), in.RuleID, node).Scan(&initialRaw); e != nil {
			if errors.Is(e, sql.ErrNoRows) {
				return errPrefetchStale
			}
			return e
		}
		var initial contract.Rule
		if e := json.Unmarshal([]byte(initialRaw), &initial); e != nil {
			return e
		}
		if e := s.lockRuleOwner(r.Context(), tx, initial.UserID); e != nil {
			return e
		}
		if s.Store.Dialect != "sqlite" {
			query += " FOR UPDATE"
		}
		var raw string
		if e := tx.QueryRowContext(r.Context(), s.q(query), in.RuleID, node).Scan(&raw); e != nil {
			if errors.Is(e, sql.ErrNoRows) {
				return errPrefetchStale
			}
			return e
		}
		var rule contract.Rule
		if e := json.Unmarshal([]byte(raw), &rule); e != nil {
			return e
		}
		if rule.UserID != initial.UserID {
			return errPrefetchStale
		}
		if !rule.Enabled || rule.ExitUnavailable || rule.Lease == nil || rule.Lease.ID != in.LeaseID {
			return errPrefetchStale
		}
		g, e := s.groupTx(r.Context(), tx, rule.GroupID)
		if e != nil {
			return e
		}
		if entryPolicyDenied(g, rule) {
			return errPrefetchForbidden
		}
		var role string
		if e := tx.QueryRowContext(r.Context(), s.q("SELECT role FROM cp_users WHERE id=?"), rule.UserID).Scan(&role); e != nil {
			return e
		}
		if role != "admin" {
			authorized, e := s.groupAuthorized(r.Context(), tx, g.ID, rule.UserID)
			if e != nil {
				return e
			}
			if !authorized {
				return errPrefetchForbidden
			}
		}
		limits, e := s.accountLimits(r.Context(), tx, rule.UserID)
		if e != nil {
			return e
		}
		allowed, e := s.withinPlanRuleLimit(r.Context(), tx, rule, limits.MaxRules)
		if e != nil {
			return e
		}
		if !allowed {
			return errPrefetchForbidden
		}
		valid, e := s.liveLeaseTx(r.Context(), tx, rule.UserID, rule.Lease)
		if e != nil {
			return e
		}
		if !valid {
			return errPrefetchStale
		}
		if rule.StandbyLease != nil {
			leaseID = rule.StandbyLease.ID
			return nil
		}
		m, ok := new(big.Rat).SetString(leaseMultiplier(rule, g))
		if !ok || m.Sign() <= 0 {
			return errors.New("invalid multiplier")
		}
		b := new(big.Int).Mul(big.NewInt(in.RawBudget), m.Num())
		b.Add(b, new(big.Int).Sub(m.Denom(), big.NewInt(1)))
		b.Quo(b, m.Denom())
		budget := int64(256 << 20)
		if b.IsInt64() {
			budget = min(budget, max(16<<20, b.Int64()))
		}
		l, e := s.allocateStandbyTx(r.Context(), tx, rule, leaseMultiplier(rule, g), budget)
		if e != nil || l == nil {
			return e
		}
		rule.StandbyLease = l
		leaseID = l.ID
		if _, e = tx.ExecContext(r.Context(), s.q("UPDATE cp_rules SET payload=? WHERE id=? AND version=? AND deleted=0"), strJSON(rule), rule.ID, rule.Version); e != nil {
			return e
		}
		_, e = tx.ExecContext(r.Context(), s.q("UPDATE cp_nodes SET desired_version=desired_version+1 WHERE id=?"), node)
		return e
	})
	if e != nil {
		status := http.StatusInternalServerError
		if errors.Is(e, errPrefetchStale) {
			status = http.StatusConflict
		} else if errors.Is(e, errPrefetchForbidden) {
			status = http.StatusForbidden
		}
		fail(w, status, "lease prefetch unavailable")
		return
	}
	reply(w, 200, map[string]string{"lease_id": leaseID})
}
