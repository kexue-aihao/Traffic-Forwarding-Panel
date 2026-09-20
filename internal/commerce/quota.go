package commerce

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"math/big"
	"time"
)

func (s *Service) Allocate(ctx context.Context, tx *sql.Tx, user, rule, node string) (*contract.Lease, error) {
	return s.AllocateWithMultiplier(ctx, tx, user, rule, node, "1")
}
func (s *Service) AllocateWithMultiplier(ctx context.Context, tx *sql.Tx, user, rule, node, multiplier string) (*contract.Lease, error) {
	m, ok := new(big.Rat).SetString(multiplier)
	if !ok || m.Sign() <= 0 {
		return nil, errors.New("invalid multiplier")
	}
	e, err := scanEnt(tx.QueryRowContext(ctx, s.q("SELECT "+entFields+" FROM commerce_entitlements WHERE user_id=? ORDER BY version DESC LIMIT 1"), user))
	if err != nil {
		return nil, err
	}
	now := s.Now()
	if !now.Before(e.ExpiresAt) {
		return nil, errors.New("entitlement expired")
	}
	var allocated int64
	if err = tx.QueryRowContext(ctx, s.q("SELECT allocated FROM commerce_entitlements WHERE id=?"), e.ID).Scan(&allocated); err != nil {
		return nil, err
	}
	remaining := e.Quota - allocated
	if remaining <= 0 {
		return nil, errors.New("quota exhausted")
	}
	budget := int64(16 << 20)
	if budget > remaining {
		budget = remaining
	}
	raw := new(big.Int).Quo(new(big.Int).Mul(big.NewInt(budget), m.Denom()), m.Num())
	if !raw.IsInt64() || raw.Sign() <= 0 {
		return nil, errors.New("quota too small")
	}
	deadline := now.Add(5 * time.Minute)
	if e.ExpiresAt.Before(deadline) {
		deadline = e.ExpiresAt
	}
	lease := &contract.Lease{ID: id(), EntitlementID: e.ID, Bytes: raw.Int64(), ExpiresAt: deadline}
	r, err := tx.ExecContext(ctx, s.q("UPDATE commerce_entitlements SET allocated=allocated+? WHERE id=? AND allocated=?"), budget, e.ID, allocated)
	if err != nil {
		return nil, err
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return nil, ErrConflict
	}
	_, err = tx.ExecContext(ctx, s.q("INSERT INTO commerce_leases(id,user_id,node_id,rule_id,entitlement_id,expires_at,bytes,used,multiplier) VALUES(?,?,?,?,?,?,?,0,?)"), lease.ID, user, node, rule, e.ID, stamp(deadline), lease.Bytes, m.RatString())
	return lease, err
}

// SettleUsage accepts delayed old-cycle facts, but never charges a newer entitlement.
func (s *Service) SettleUsage(ctx context.Context, tx *sql.Tx, u contract.UsageRecord) error {
	if u.ID == "" || len(u.ID) > 128 || u.UploadBytes < 0 || u.DownloadBytes < 0 || u.UploadBytes > int64(^uint64(0)>>1)-u.DownloadBytes || u.EndedAt.Before(u.StartedAt) {
		return errors.New("invalid usage")
	}
	payload, _ := json.Marshal(u)
	var old string
	err := tx.QueryRowContext(ctx, s.q("SELECT payload FROM commerce_usage WHERE id=?"), u.ID).Scan(&old)
	if err == nil {
		if old != string(payload) {
			return ErrConflict
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var node, rule, ent, expiry, mult string
	var bytes, used int64
	err = tx.QueryRowContext(ctx, s.q("SELECT node_id,rule_id,entitlement_id,expires_at,bytes,used,multiplier FROM commerce_leases WHERE id=?"), u.LeaseID).Scan(&node, &rule, &ent, &expiry, &bytes, &used, &mult)
	if err != nil {
		return err
	}
	if node != u.NodeID || rule != u.RuleID || ent != u.EntitlementID || u.EndedAt.After(parse(expiry)) {
		return errors.New("usage outside lease")
	}
	raw := u.UploadBytes + u.DownloadBytes
	if raw > bytes-used {
		return errors.New("lease quota exceeded")
	}
	m, _ := new(big.Rat).SetString(mult)
	ceil := func(v int64) *big.Int {
		n := new(big.Int).Mul(big.NewInt(v), m.Num())
		n.Add(n, new(big.Int).Sub(m.Denom(), big.NewInt(1)))
		return n.Quo(n, m.Denom())
	}
	charge := new(big.Int).Sub(ceil(used+raw), ceil(used))
	if !charge.IsInt64() {
		return errors.New("charge overflow")
	}
	r, err := tx.ExecContext(ctx, s.q("UPDATE commerce_leases SET used=used+? WHERE id=? AND used=?"), raw, u.LeaseID, used)
	if err != nil {
		return err
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return ErrConflict
	}
	_, err = tx.ExecContext(ctx, s.q("UPDATE commerce_entitlements SET used=used+? WHERE id=?"), charge.Int64(), ent)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, s.q("INSERT INTO commerce_usage(id,lease_id,payload,charged) VALUES(?,?,?,?)"), u.ID, u.LeaseID, string(payload), charge.Int64())
	return err
}
