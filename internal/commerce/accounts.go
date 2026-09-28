package commerce

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

type UserAccount struct {
	Balance           int64 `json:"balance_cents,string"`
	RuleCount         int   `json:"rule_count"`
	PlanMaxRules      int   `json:"plan_max_rules"`
	MaxRules          int   `json:"max_rules"`
	RuleLimitOverride *int  `json:"rule_limit_override"`
}

func (s *Service) migrateAccountAdjustments(ctx context.Context, conn *sql.Conn) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS commerce_user_rule_limits(user_id VARCHAR(64) PRIMARY KEY,max_rules INTEGER NOT NULL,actor_id VARCHAR(64) NOT NULL,reason VARCHAR(500) NOT NULL,updated_at VARCHAR(40) NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS commerce_admin_adjustments(id VARCHAR(64) PRIMARY KEY,user_id VARCHAR(64) NOT NULL,actor_id VARCHAR(64) NOT NULL,kind VARCHAR(24) NOT NULL,amount BIGINT NOT NULL,max_rules INTEGER,idempotency_key VARCHAR(128) NOT NULL,reason VARCHAR(500) NOT NULL,created_at VARCHAR(40) NOT NULL)`,
	} {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) lockAccount(ctx context.Context, tx *sql.Tx, user string) error {
	q := "SELECT id FROM cp_users WHERE id=?"
	if s.Dialect != "sqlite" {
		q += " FOR UPDATE"
	}
	var found string
	return tx.QueryRowContext(ctx, s.q(q), user).Scan(&found)
}

func (s *Service) userRuleLimit(ctx context.Context, q limitsQuery, user string) (int, bool, error) {
	var maximum int
	err := q.QueryRowContext(ctx, s.q("SELECT max_rules FROM commerce_user_rule_limits WHERE user_id=?"), user).Scan(&maximum)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return maximum, err == nil, err
}

func (s *Service) UserAccount(ctx context.Context, user string) (UserAccount, error) {
	var account UserAccount
	var found string
	if err := s.DB.QueryRowContext(ctx, s.q("SELECT id FROM cp_users WHERE id=?"), user).Scan(&found); err != nil {
		return account, err
	}
	wallet, err := s.Wallet(ctx, user)
	if err != nil {
		return account, err
	}
	account.Balance = wallet.Balance
	var entitlement string
	err = s.DB.QueryRowContext(ctx, s.q("SELECT id FROM commerce_entitlements WHERE user_id=? ORDER BY version DESC LIMIT 1"), user).Scan(&entitlement)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return account, err
	}
	if err == nil {
		limits, err := s.entitlementLimits(ctx, s.DB, entitlement)
		if err != nil {
			return account, err
		}
		account.PlanMaxRules = limits.MaxRules
	}
	if value, ok, err := s.userRuleLimit(ctx, s.DB, user); err != nil {
		return account, err
	} else if ok {
		account.RuleLimitOverride = &value
		account.MaxRules = value
	} else {
		account.MaxRules = account.PlanMaxRules
	}
	if err := s.DB.QueryRowContext(ctx, s.q("SELECT COUNT(*) FROM cp_rules WHERE user_id=? AND deleted=0"), user).Scan(&account.RuleCount); err != nil {
		return account, err
	}
	return account, nil
}

func (s *Service) AdjustUserBalance(ctx context.Context, actor, user, key, reason string, delta int64) error {
	key, reason = strings.TrimSpace(key), strings.TrimSpace(reason)
	if delta == 0 || delta > 1_000_000_000_000 || delta < -1_000_000_000_000 || len(key) == 0 || len(key) > 128 || len(reason) == 0 || len(reason) > 500 {
		return errors.New("invalid balance adjustment")
	}
	return s.Write(ctx, func(tx *sql.Tx) error {
		if err := s.lockAccount(ctx, tx, user); err != nil {
			return err
		}
		if _, _, err := s.walletTx(ctx, tx, user); err != nil {
			return err
		}
		var priorActor, priorReason string
		var priorAmount int64
		err := tx.QueryRowContext(ctx, s.q("SELECT actor_id,amount,reason FROM commerce_admin_adjustments WHERE user_id=? AND kind='balance' AND idempotency_key=?"), user, key).Scan(&priorActor, &priorAmount, &priorReason)
		if err == nil {
			if priorActor == actor && priorAmount == delta && priorReason == reason {
				return nil
			}
			return ErrConflict
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		adjustmentID := id()
		if err = s.post(ctx, tx, user, delta, "adjustment", "admin-adjustment:"+adjustmentID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, s.q("INSERT INTO commerce_admin_adjustments(id,user_id,actor_id,kind,amount,idempotency_key,reason,created_at) VALUES(?,?,?,'balance',?,?,?,?)"), adjustmentID, user, actor, delta, key, reason, stamp(s.Now()))
		return err
	})
}

func (s *Service) SetUserRuleLimit(ctx context.Context, actor, user string, maximum *int, reason string) error {
	reason = strings.TrimSpace(reason)
	if maximum != nil && (*maximum < 0 || *maximum > 100000) || len(reason) == 0 || len(reason) > 500 {
		return errors.New("invalid account rule limit")
	}
	return s.Write(ctx, func(tx *sql.Tx) error {
		if err := s.lockAccount(ctx, tx, user); err != nil {
			return err
		}
		previous, hadPrevious, err := s.userRuleLimit(ctx, tx, user)
		if err != nil {
			return err
		}
		if maximum == nil && !hadPrevious || maximum != nil && hadPrevious && *maximum == previous {
			return nil
		}
		adjustmentID := id()
		kind := "rule_limit"
		var value any = 0
		if maximum == nil {
			kind = "rule_limit_clear"
			if _, err = tx.ExecContext(ctx, s.q("DELETE FROM commerce_user_rule_limits WHERE user_id=?"), user); err != nil {
				return err
			}
		} else {
			value = *maximum
			q := "INSERT INTO commerce_user_rule_limits(user_id,max_rules,actor_id,reason,updated_at) VALUES(?,?,?,?,?) ON CONFLICT(user_id) DO UPDATE SET max_rules=excluded.max_rules,actor_id=excluded.actor_id,reason=excluded.reason,updated_at=excluded.updated_at"
			if s.Dialect == "mysql" {
				q = "INSERT INTO commerce_user_rule_limits(user_id,max_rules,actor_id,reason,updated_at) VALUES(?,?,?,?,?) ON DUPLICATE KEY UPDATE max_rules=VALUES(max_rules),actor_id=VALUES(actor_id),reason=VALUES(reason),updated_at=VALUES(updated_at)"
			}
			if _, err = tx.ExecContext(ctx, s.q(q), user, *maximum, actor, reason, stamp(s.Now())); err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, s.q("INSERT INTO commerce_admin_adjustments(id,user_id,actor_id,kind,amount,max_rules,idempotency_key,reason,created_at) VALUES(?,?,?, ?,0,?,'',?,?)"), adjustmentID, user, actor, kind, value, reason, stamp(s.Now())); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, s.q("UPDATE cp_nodes SET desired_version=desired_version+1 WHERE id IN(SELECT node_id FROM cp_rules WHERE user_id=? AND deleted=0)"), user)
		return err
	})
}
