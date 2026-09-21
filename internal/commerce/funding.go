package commerce

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math/big"
	"strings"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
)

func (s *Service) migrateFunding(ctx context.Context, conn *sql.Conn) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS commerce_fund_accounts(user_id VARCHAR(64) PRIMARY KEY)`,
		`CREATE TABLE IF NOT EXISTS commerce_fund_sources(id VARCHAR(128) PRIMARY KEY,user_id VARCHAR(64) NOT NULL,order_id VARCHAR(64) NOT NULL,amount BIGINT NOT NULL,remaining BIGINT NOT NULL,created_at VARCHAR(40) NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS commerce_fund_allocations(reference_id VARCHAR(128) NOT NULL,source_id VARCHAR(128) NOT NULL,amount BIGINT NOT NULL,restored BIGINT NOT NULL,PRIMARY KEY(reference_id,source_id))`,
		`CREATE TABLE IF NOT EXISTS commerce_purchase_refunds(id VARCHAR(64) PRIMARY KEY,user_id VARCHAR(64) NOT NULL,purchase_id VARCHAR(64) NOT NULL,amount BIGINT NOT NULL,quota_removed BIGINT NOT NULL,actor_id VARCHAR(64) NOT NULL,reason VARCHAR(500) NOT NULL,idempotency_key VARCHAR(128) NOT NULL,created_at VARCHAR(40) NOT NULL,UNIQUE(purchase_id,idempotency_key))`,
		`CREATE TABLE IF NOT EXISTS commerce_commission_refunds(commission_id VARCHAR(64) PRIMARY KEY,amount BIGINT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS commerce_notification_formats(subscription_id VARCHAR(64) PRIMARY KEY,format VARCHAR(20) NOT NULL)`,
	} {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return storage.EnsureIndex(ctx, conn, s.Dialect, "commerce_fund_sources", "commerce_funds_user", "user_id,created_at,id", false)
}

// Called under the wallet row lock. Historical balance is explicitly unassigned;
// old purchases never acquire fabricated recharge provenance during migration.
func (s *Service) trackFunds(ctx context.Context, tx *sql.Tx, user string, balance, delta int64, kind, reference string) error {
	var marker string
	err := tx.QueryRowContext(ctx, s.q("SELECT user_id FROM commerce_fund_accounts WHERE user_id=?"), user).Scan(&marker)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err = tx.ExecContext(ctx, s.q("INSERT INTO commerce_fund_accounts(user_id) VALUES(?)"), user); err != nil {
			return err
		}
		if balance > 0 {
			if _, err = tx.ExecContext(ctx, s.q("INSERT INTO commerce_fund_sources(id,user_id,order_id,amount,remaining,created_at) VALUES(?,?,'',?,?,?)"), "legacy:"+user, user, balance, balance, stamp(s.Now())); err != nil {
				return err
			}
		}
	} else if err != nil {
		return err
	}
	if delta == 0 {
		return nil
	}
	if delta > 0 {
		restore := ""
		if kind == "refund_release" {
			restore = "refund-reserve:" + strings.TrimPrefix(reference, "refund-release:")
		}
		if kind == "purchase_refund" {
			if err := tx.QueryRowContext(ctx, s.q("SELECT purchase_id FROM commerce_purchase_refunds WHERE id=?"), reference).Scan(&restore); err != nil {
				return err
			}
		}
		if restore != "" {
			return s.restoreFunds(ctx, tx, user, restore, delta, kind == "refund_release")
		}
		order := ""
		if kind == "recharge" {
			order = strings.TrimPrefix(reference, "payment:")
		}
		_, err = tx.ExecContext(ctx, s.q("INSERT INTO commerce_fund_sources(id,user_id,order_id,amount,remaining,created_at) VALUES(?,?,?,?,?,?)"), reference, user, order, delta, delta, stamp(s.Now()))
		return err
	}
	where := "user_id=? AND remaining>0"
	args := []any{user}
	if kind == "refund_reserve" {
		var order string
		if err := tx.QueryRowContext(ctx, s.q("SELECT order_id FROM commerce_refunds WHERE id=?"), strings.TrimPrefix(reference, "refund-reserve:")).Scan(&order); err != nil {
			return err
		}
		var sources int
		if err := tx.QueryRowContext(ctx, s.q("SELECT COUNT(*) FROM commerce_fund_sources WHERE user_id=? AND order_id=?"), user, order).Scan(&sources); err != nil {
			return err
		}
		if sources > 0 {
			where += " AND order_id=?"
			args = append(args, order)
		} // legacy refunds retain the established balance-reservation flow
	}
	rows, err := tx.QueryContext(ctx, s.q("SELECT id,remaining FROM commerce_fund_sources WHERE "+where+" ORDER BY created_at,id"), args...)
	if err != nil {
		return err
	}
	type source struct {
		id        string
		remaining int64
	}
	list := []source{}
	for rows.Next() {
		var v source
		if err = rows.Scan(&v.id, &v.remaining); err != nil {
			rows.Close()
			return err
		}
		list = append(list, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	left := -delta
	for _, v := range list {
		amount := min(left, v.remaining)
		if amount == 0 {
			break
		}
		if _, err = tx.ExecContext(ctx, s.q("UPDATE commerce_fund_sources SET remaining=remaining-? WHERE id=? AND remaining>=?"), amount, v.id, amount); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, s.q("INSERT INTO commerce_fund_allocations(reference_id,source_id,amount,restored) VALUES(?,?,?,0)"), reference, v.id, amount); err != nil {
			return err
		}
		left -= amount
	}
	if left != 0 {
		return errors.New("original recharge funds are spent; refund the associated purchase to wallet first")
	}
	return nil
}
func (s *Service) restoreFunds(ctx context.Context, tx *sql.Tx, user, reference string, amount int64, legacy bool) error {
	rows, err := tx.QueryContext(ctx, s.q("SELECT a.source_id,a.amount-a.restored FROM commerce_fund_allocations a JOIN commerce_fund_sources f ON f.id=a.source_id WHERE a.reference_id=? AND f.user_id=? ORDER BY f.created_at,f.id"), reference, user)
	if err != nil {
		return err
	}
	type allocation struct {
		id        string
		available int64
	}
	list := []allocation{}
	for rows.Next() {
		var v allocation
		if err = rows.Scan(&v.id, &v.available); err != nil {
			rows.Close()
			return err
		}
		list = append(list, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(list) == 0 && legacy {
		_, err = tx.ExecContext(ctx, s.q("INSERT INTO commerce_fund_sources(id,user_id,order_id,amount,remaining,created_at) VALUES(?,?,'',?,?,?)"), "release:"+reference, user, amount, amount, stamp(s.Now()))
		return err
	}
	left := amount
	for _, v := range list {
		n := min(left, v.available)
		if n == 0 {
			continue
		}
		if _, err = tx.ExecContext(ctx, s.q("UPDATE commerce_fund_allocations SET restored=restored+? WHERE reference_id=? AND source_id=?"), n, reference, v.id); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, s.q("UPDATE commerce_fund_sources SET remaining=remaining+? WHERE id=?"), n, v.id); err != nil {
			return err
		}
		left -= n
		if left == 0 {
			break
		}
	}
	if left != 0 {
		return errors.New("purchase lacks verified refundable funding history")
	}
	return nil
}

type PurchaseRefund struct {
	ID           string `json:"id"`
	PurchaseID   string `json:"purchase_id"`
	Amount       int64  `json:"amount_cents,string"`
	QuotaRemoved int64  `json:"quota_removed,string"`
}

func fraction(total, n, d int64) int64 {
	return new(big.Int).Quo(new(big.Int).Mul(big.NewInt(total), big.NewInt(n)), big.NewInt(d)).Int64()
}
func (s *Service) RefundPurchase(ctx context.Context, actor, purchase, key, reason string, amount int64) (PurchaseRefund, error) {
	var out PurchaseRefund
	if actor == "" || amount <= 0 || len(key) < 1 || len(key) > 128 || strings.TrimSpace(reason) == "" || len(reason) > 500 {
		return out, errors.New("refund amount, reason and idempotency key required")
	}
	err := s.Write(ctx, func(tx *sql.Tx) error {
		var user, ent, raw string
		err := tx.QueryRowContext(ctx, s.q("SELECT user_id,entitlement_id,payload FROM commerce_purchase_snapshots WHERE entitlement_id=?"), purchase).Scan(&user, &ent, &raw)
		if errors.Is(err, sql.ErrNoRows) {
			err = tx.QueryRowContext(ctx, s.q("SELECT user_id,entitlement_id,payload FROM commerce_addon_purchases WHERE id=?"), purchase).Scan(&user, &ent, &raw)
		}
		if err != nil {
			return err
		}
		if s.CheckAccount != nil {
			if err = s.CheckAccount(ctx, tx, user); err != nil {
				return err
			}
		}
		if _, _, err = s.walletTx(ctx, tx, user); err != nil {
			return err
		}
		var previousReason string
		err = tx.QueryRowContext(ctx, s.q("SELECT id,amount,quota_removed,reason FROM commerce_purchase_refunds WHERE purchase_id=? AND idempotency_key=?"), purchase, key).Scan(&out.ID, &out.Amount, &out.QuotaRemoved, &previousReason)
		out.PurchaseID = purchase
		if err == nil {
			if out.Amount != amount || previousReason != reason {
				return ErrConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var plan Plan
		if err = json.Unmarshal([]byte(raw), &plan); err != nil {
			return err
		}
		if plan.Price <= 0 {
			return errors.New("free or redeemed plans have no refundable price")
		}
		var refunded int64
		if err = tx.QueryRowContext(ctx, s.q("SELECT COALESCE(SUM(amount),0) FROM commerce_purchase_refunds WHERE purchase_id=?"), purchase).Scan(&refunded); err != nil {
			return err
		}
		if amount > plan.Price-refunded {
			return errors.New("refund exceeds purchase price")
		}
		removed := fraction(plan.Quota, refunded+amount, plan.Price) - fraction(plan.Quota, refunded, plan.Price)
		res, err := tx.ExecContext(ctx, s.q("UPDATE commerce_entitlements SET quota=quota-? WHERE id=? AND quota-?>=allocated AND quota-?>=used"), removed, ent, removed, removed)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 1 && removed != 0 {
			return errors.New("refund would remove used or reserved quota")
		}
		out = PurchaseRefund{ID: id(), PurchaseID: purchase, Amount: amount, QuotaRemoved: removed}
		if _, err = tx.ExecContext(ctx, s.q("INSERT INTO commerce_purchase_refunds(id,user_id,purchase_id,amount,quota_removed,actor_id,reason,idempotency_key,created_at) VALUES(?,?,?,?,?,?,?,?,?)"), out.ID, user, purchase, amount, removed, actor, reason, key, stamp(s.Now())); err != nil {
			return err
		}
		if err = s.reversePurchaseCommission(ctx, tx, actor, purchase, refunded+amount, plan.Price, reason); err != nil {
			return err
		}
		if err = s.post(ctx, tx, user, amount, "purchase_refund", out.ID); err != nil {
			return err
		}
		return s.emitEventTx(ctx, tx, user, "purchase.refunded", map[string]any{"purchase_id": purchase, "refund_id": out.ID})
	})
	return out, err
}
func (s *Service) reversedCommission(ctx context.Context, tx *sql.Tx, id string) (int64, error) {
	var amount int64
	err := tx.QueryRowContext(ctx, s.q("SELECT amount FROM commerce_commission_refunds WHERE commission_id=?"), id).Scan(&amount)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return amount, err
}
func (s *Service) reversePurchaseCommission(ctx context.Context, tx *sql.Tx, actor, purchase string, refunded, price int64, reason string) error {
	q := "SELECT id,inviter_id,amount,status FROM commerce_commissions WHERE source_id=?"
	if s.Dialect != "sqlite" {
		q += " FOR UPDATE"
	}
	var cid, owner, status string
	var amount int64
	err := tx.QueryRowContext(ctx, s.q(q), purchase).Scan(&cid, &owner, &amount, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if status == "reversed" {
		return nil
	}
	previous, err := s.reversedCommission(ctx, tx, cid)
	if err != nil {
		return err
	}
	target := fraction(amount, refunded, price)
	delta := target - previous
	if delta <= 0 {
		return nil
	}
	if status == "paid" {
		if err = s.post(ctx, tx, owner, -delta, "commission_reversal", id()); err != nil {
			return err
		}
	}
	upsert := "INSERT INTO commerce_commission_refunds(commission_id,amount) VALUES(?,?) ON CONFLICT(commission_id) DO UPDATE SET amount=excluded.amount"
	if s.Dialect == "mysql" {
		upsert = "INSERT INTO commerce_commission_refunds(commission_id,amount) VALUES(?,?) ON DUPLICATE KEY UPDATE amount=VALUES(amount)"
	}
	if _, err = tx.ExecContext(ctx, s.q(upsert), cid, target); err != nil {
		return err
	}
	if target == amount {
		if _, err = tx.ExecContext(ctx, s.q("UPDATE commerce_commissions SET status='reversed' WHERE id=?"), cid); err != nil {
			return err
		}
	}
	return s.emitEventTx(ctx, tx, owner, "commission.refunded", map[string]any{"commission_id": cid, "purchase_id": purchase})
}

func (s *Service) PurchaseFunding(ctx context.Context, user, purchase string, admin bool) ([]map[string]any, error) {
	var owner string
	err := s.DB.QueryRowContext(ctx, s.q("SELECT user_id FROM commerce_purchase_snapshots WHERE entitlement_id=?"), purchase).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		err = s.DB.QueryRowContext(ctx, s.q("SELECT user_id FROM commerce_addon_purchases WHERE id=?"), purchase).Scan(&owner)
	}
	if err != nil {
		return nil, err
	}
	if !admin && owner != user {
		return nil, sql.ErrNoRows
	}
	rows, err := s.DB.QueryContext(ctx, s.q("SELECT f.order_id,a.amount,a.restored FROM commerce_fund_allocations a JOIN commerce_fund_sources f ON f.id=a.source_id WHERE a.reference_id=? ORDER BY f.created_at,f.id"), purchase)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var order string
		var amount, restored int64
		if err = rows.Scan(&order, &amount, &restored); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"order_id": order, "amount_cents": new(big.Int).SetInt64(amount).String(), "refunded_cents": new(big.Int).SetInt64(restored).String()})
	}
	return out, rows.Err()
}
