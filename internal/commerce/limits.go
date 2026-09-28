package commerce

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

func (s *Service) migrateLimits(ctx context.Context, conn *sql.Conn) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS commerce_plan_limits(plan_id VARCHAR(64) PRIMARY KEY,payload TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS commerce_entitlement_limits(entitlement_id VARCHAR(64) PRIMARY KEY,payload TEXT NOT NULL)`,
	} {
		if s.Dialect == "mysql" {
			q = strings.ReplaceAll(q, " TEXT", " LONGTEXT")
		}
		if _, err := conn.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

type limitsQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (s *Service) entitlementLimits(ctx context.Context, q limitsQuery, ent string) (contract.PlanLimits, error) {
	var limits contract.PlanLimits
	var raw string
	err := q.QueryRowContext(ctx, s.q("SELECT payload FROM commerce_entitlement_limits WHERE entitlement_id=?"), ent).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return limits, nil
	} // Historical grants had no commercial restrictions.
	if err != nil {
		return limits, err
	}
	if err = json.Unmarshal([]byte(raw), &limits); err != nil {
		return limits, err
	}
	return limits, limits.Validate()
}

func ruleOnlyLimits(limits contract.PlanLimits) contract.PlanLimits {
	return contract.PlanLimits{MaxRules: limits.MaxRules}
}

// LimitsTx reads the immutable entitlement policy, never today's plan settings.
// The caller serializes rule creation and purchases with the account row lock.
func (s *Service) LimitsTx(ctx context.Context, tx *sql.Tx, user string) (contract.ResourceLimits, error) {
	var limits contract.PlanLimits
	var ent string
	q := "SELECT id FROM commerce_entitlements WHERE user_id=? ORDER BY version DESC LIMIT 1"
	if s.Dialect != "sqlite" {
		q += " FOR UPDATE"
	}
	err := tx.QueryRowContext(ctx, s.q(q), user).Scan(&ent)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return contract.ResourceLimits{}, err
	}
	if err == nil {
		limits, err = s.entitlementLimits(ctx, tx, ent)
		if err != nil {
			return contract.ResourceLimits{}, err
		}
	}
	if override, exists, err := s.userRuleLimit(ctx, tx, user); err != nil {
		return contract.ResourceLimits{}, err
	} else if exists {
		limits.MaxRules = override
	}
	return contract.ResourceLimits{MaxRules: limits.MaxRules}, nil
}

func (s *Service) savePlanLimits(ctx context.Context, tx *sql.Tx, p Plan) error {
	raw, err := json.Marshal(ruleOnlyLimits(p.Limits))
	if err != nil {
		return err
	}
	q := "INSERT INTO commerce_plan_limits(plan_id,payload) VALUES(?,?) ON CONFLICT(plan_id) DO UPDATE SET payload=excluded.payload"
	if s.Dialect == "mysql" {
		q = "INSERT INTO commerce_plan_limits(plan_id,payload) VALUES(?,?) ON DUPLICATE KEY UPDATE payload=VALUES(payload)"
	}
	_, err = tx.ExecContext(ctx, s.q(q), p.ID, string(raw))
	return err
}

func (s *Service) snapshotLimits(ctx context.Context, tx *sql.Tx, ent string, limits contract.PlanLimits) error {
	raw, err := json.Marshal(ruleOnlyLimits(limits))
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, s.q("INSERT INTO commerce_entitlement_limits(entitlement_id,payload) VALUES(?,?)"), ent, string(raw))
	return err
}
