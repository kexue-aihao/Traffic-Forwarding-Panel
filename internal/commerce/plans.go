package commerce

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"math"
	"strings"
)

func validatePlan(p Plan) error {
	if err := p.Limits.Validate(); err != nil {
		return err
	}
	if p.Kind == "addon" && p.Limits != (contract.ResourceLimits{}) {
		return errors.New("add-ons cannot change limits")
	}
	if strings.TrimSpace(p.Name) == "" || len(p.Name) > 200 || p.Price <= 0 || p.Price > 100000000 || p.Quota <= 0 {
		return errors.New("invalid plan")
	}
	if p.Kind == "period" && p.Months >= 1 && p.Months <= 120 || p.Kind == "addon" && p.Months == 0 {
		return nil
	}
	return errors.New("invalid plan kind or duration")
}
func (s *Service) planTx(ctx context.Context, tx *sql.Tx, id string) (Plan, error) {
	q := "SELECT p.id,p.name,p.price,p.quota,p.months,st.active,st.version,st.kind FROM commerce_plans p JOIN commerce_plan_states st ON st.plan_id=p.id WHERE p.id=?"
	if s.Dialect != "sqlite" {
		q += " FOR UPDATE"
	}
	var p Plan
	err := tx.QueryRowContext(ctx, s.q(q), id).Scan(&p.ID, &p.Name, &p.Price, &p.Quota, &p.Months, &p.Active, &p.Version, &p.Kind)
	if err == nil {
		var raw string
		err = tx.QueryRowContext(ctx, s.q("SELECT payload FROM commerce_plan_limits WHERE plan_id=?"), id).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			err = nil
		} else if err == nil {
			err = json.Unmarshal([]byte(raw), &p.Limits)
		}
	}
	return p, err
}
func (s *Service) UpdatePlan(ctx context.Context, p Plan) (Plan, error) {
	if err := validatePlan(p); err != nil {
		return p, err
	}
	err := s.Write(ctx, func(tx *sql.Tx) error {
		old, err := s.planTx(ctx, tx, p.ID)
		if err != nil {
			return err
		}
		if p.Version != old.Version || p.Kind != old.Kind {
			return ErrConflict
		}
		if _, err = tx.ExecContext(ctx, s.q("UPDATE commerce_plans SET name=?,price=?,quota=?,months=? WHERE id=?"), p.Name, p.Price, p.Quota, p.Months, p.ID); err != nil {
			return err
		}
		if err = s.savePlanLimits(ctx, tx, p); err != nil {
			return err
		}
		p.Version++
		p.Active = old.Active
		_, err = tx.ExecContext(ctx, s.q("UPDATE commerce_plan_states SET version=?,updated_at=? WHERE plan_id=?"), p.Version, stamp(s.Now()), p.ID)
		return err
	})
	return p, err
}

// Add-ons add quota to the current period; its start, expiry and base plan remain
// unchanged. The snapshot and returned entitlement are retained for exact replay.
func (s *Service) PurchaseAddon(ctx context.Context, user, plan, key string, expected, planVersion int64) (Entitlement, error) {
	var result Entitlement
	if len(key) < 1 || len(key) > 128 {
		return result, errors.New("invalid idempotency key")
	}
	err := s.Write(ctx, func(tx *sql.Tx) error {
		if s.CheckAccount != nil {
			if err := s.CheckAccount(ctx, tx, user); err != nil {
				return err
			}
		}
		if _, _, err := s.walletTx(ctx, tx, user); err != nil {
			return err
		}
		var priorPlan, raw string
		var version, quote int64
		err := tx.QueryRowContext(ctx, s.q("SELECT plan_id,expected_version,plan_version,result FROM commerce_addon_purchases WHERE user_id=? AND idempotency_key=?"), user, key).Scan(&priorPlan, &version, &quote, &raw)
		if err == nil {
			if priorPlan != plan || expected != version || planVersion != 0 && quote != planVersion {
				return ErrConflict
			}
			return json.Unmarshal([]byte(raw), &result)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		p, err := s.planTx(ctx, tx, plan)
		if err != nil {
			return err
		}
		if p.Kind != "addon" || !p.Active {
			return errors.New("add-on unavailable")
		}
		if planVersion != 0 && planVersion != p.Version {
			return ErrConflict
		}
		q := "SELECT " + entFields + " FROM commerce_entitlements WHERE user_id=? ORDER BY version DESC LIMIT 1"
		if s.Dialect != "sqlite" {
			q += " FOR UPDATE"
		}
		result, err = scanEnt(tx.QueryRowContext(ctx, s.q(q), user))
		if err != nil {
			return err
		}
		result.Limits, err = s.entitlementLimits(ctx, tx, result.ID)
		if err != nil {
			return err
		}
		if result.Version != expected {
			return ErrConflict
		}
		if !s.Now().Before(result.ExpiresAt) {
			return errors.New("active entitlement required")
		}
		if result.Quota > math.MaxInt64-p.Quota {
			return errors.New("quota overflow")
		}
		purchaseID := id()
		if err = s.post(ctx, tx, user, -p.Price, "addon", purchaseID); err != nil {
			return err
		}
		result.Quota += p.Quota
		if _, err = tx.ExecContext(ctx, s.q("UPDATE commerce_entitlements SET quota=? WHERE id=?"), result.Quota, result.ID); err != nil {
			return err
		}
		snapshot, err := json.Marshal(p)
		if err != nil {
			return err
		}
		response, err := json.Marshal(result)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, s.q("INSERT INTO commerce_addon_purchases(id,user_id,idempotency_key,plan_id,expected_version,plan_version,entitlement_id,payload,result,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)"), purchaseID, user, key, plan, expected, p.Version, result.ID, string(snapshot), string(response), stamp(s.Now())); err != nil {
			return err
		}
		return s.emitEventTx(ctx, tx, user, "entitlement.addon", map[string]any{"purchase_id": purchaseID, "entitlement_id": result.ID, "plan_id": plan})
	})
	return result, err
}
func (s *Service) PurchaseHistory(ctx context.Context, user string) ([]map[string]any, error) {
	rows, err := s.DB.QueryContext(ctx, s.q("SELECT entitlement_id,payload,created_at FROM commerce_purchase_snapshots WHERE user_id=? ORDER BY created_at DESC,entitlement_id DESC LIMIT 100"), user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var ent, raw, created string
		var p Plan
		if err = rows.Scan(&ent, &raw, &created); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(raw), &p); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"entitlement_id": ent, "plan": p, "created_at": parse(created)})
	}
	return out, rows.Err()
}
