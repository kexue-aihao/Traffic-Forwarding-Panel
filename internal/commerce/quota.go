package commerce

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
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
		if errors.Is(err, sql.ErrNoRows) {
			return nil, contract.ErrEntitlementUnavailable
		}
		return nil, err
	}
	now := s.Now()
	if !now.Before(e.ExpiresAt) {
		return nil, contract.ErrEntitlementUnavailable
	}
	var allocated int64
	if err = tx.QueryRowContext(ctx, s.q("SELECT allocated FROM commerce_entitlements WHERE id=?"), e.ID).Scan(&allocated); err != nil {
		return nil, err
	}
	remaining := e.Quota - allocated
	if remaining <= 0 {
		return nil, contract.ErrEntitlementUnavailable
	}
	budget := int64(16 << 20)
	if budget > remaining {
		budget = remaining
	}
	raw := new(big.Int).Quo(new(big.Int).Mul(big.NewInt(budget), m.Denom()), m.Num())
	if !raw.IsInt64() || raw.Sign() <= 0 {
		return nil, contract.ErrEntitlementUnavailable
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
	if err == nil {
		_, err = tx.ExecContext(ctx, s.q("INSERT INTO commerce_lease_reservations(lease_id,budget,closed) VALUES(?,?,0)"), lease.ID, budget)
	}
	return lease, err
}

// LeaseCurrent rechecks the latest purchase version without reallocating quota.
func (s *Service) LeaseCurrent(ctx context.Context, tx *sql.Tx, user string, lease *contract.Lease) (bool, error) {
	if lease == nil || !s.Now().Before(lease.ExpiresAt) {
		return false, nil
	}
	var current string
	err := tx.QueryRowContext(ctx, s.q("SELECT id FROM commerce_entitlements WHERE user_id=? ORDER BY version DESC LIMIT 1"), user).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil || current != lease.EntitlementID {
		return false, err
	}
	var closed int
	err = tx.QueryRowContext(ctx, s.q("SELECT closed FROM commerce_lease_reservations WHERE lease_id=?"), lease.ID).Scan(&closed)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	} // pre-reservation migration lease
	return closed == 0, err
}

// RetireLease is accepted only after the Agent has durably stopped the lease
// and all its usage has been acknowledged. Unreported bytes prevent release.
func (s *Service) RetireLease(ctx context.Context, tx *sql.Tx, node, leaseID string, usedBytes int64) error {
	if usedBytes < 0 {
		return errors.New("invalid used bytes")
	}
	var storedNode, ent, multiplier string
	var bytes, used int64
	err := tx.QueryRowContext(ctx, s.q("SELECT node_id,entitlement_id,bytes,used,multiplier FROM commerce_leases WHERE id=?"), leaseID).Scan(&storedNode, &ent, &bytes, &used, &multiplier)
	if err != nil {
		return err
	}
	if storedNode != node || used != usedBytes {
		return ErrConflict
	}
	m, ok := new(big.Rat).SetString(multiplier)
	if !ok || m.Sign() <= 0 {
		return errors.New("invalid lease multiplier")
	}
	charge := func(v int64) int64 {
		n := new(big.Int).Mul(big.NewInt(v), m.Num())
		n.Add(n, new(big.Int).Sub(m.Denom(), big.NewInt(1)))
		n.Quo(n, m.Denom())
		return n.Int64()
	}
	var budget int64
	var closed int
	err = tx.QueryRowContext(ctx, s.q("SELECT budget,closed FROM commerce_lease_reservations WHERE lease_id=?"), leaseID).Scan(&budget, &closed)
	if errors.Is(err, sql.ErrNoRows) {
		budget = charge(bytes)
		_, err = tx.ExecContext(ctx, s.q("INSERT INTO commerce_lease_reservations(lease_id,budget,closed) VALUES(?,?,0)"), leaseID, budget)
	}
	if err != nil {
		return err
	}
	if closed != 0 {
		return nil
	}
	refund := budget - charge(used)
	if refund < 0 {
		return errors.New("invalid lease reservation")
	}
	r, err := tx.ExecContext(ctx, s.q("UPDATE commerce_lease_reservations SET closed=1 WHERE lease_id=? AND closed=0"), leaseID)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrConflict
	}
	r, err = tx.ExecContext(ctx, s.q("UPDATE commerce_entitlements SET allocated=allocated-? WHERE id=? AND allocated>=?"), refund, ent, refund)
	if err != nil {
		return err
	}
	n, err = r.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrConflict
	}
	return nil
}

// SettleUsage accepts delayed old-cycle facts, but never charges a newer entitlement.
func (s *Service) SettleUsage(ctx context.Context, tx *sql.Tx, u contract.UsageRecord) error {
	return s.SettleUsageBatch(ctx, tx, []contract.UsageRecord{u})
}
func (s *Service) SettleUsageBatch(ctx context.Context, tx *sql.Tx, records []contract.UsageRecord) error {
	queries := storage.NewQueries(tx)
	defer queries.Close()
	for _, record := range records {
		if err := s.settleUsage(ctx, queries, record); err != nil {
			return err
		}
	}
	return nil
}
func (s *Service) settleUsage(ctx context.Context, queries *storage.Queries, u contract.UsageRecord) error {
	if u.ID == "" || len(u.ID) > 128 || u.UploadBytes < 0 || u.DownloadBytes < 0 || u.UploadBytes > int64(^uint64(0)>>1)-u.DownloadBytes || u.EndedAt.Before(u.StartedAt) {
		return errors.New("invalid usage")
	}
	payload, _ := json.Marshal(u)
	var old string
	err := queries.Row(ctx, s.q("SELECT payload FROM commerce_usage WHERE id=?"), u.ID).Scan(&old)
	if err == nil {
		if old != string(payload) {
			return ErrConflict
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var closed int
	var node, rule, ent, expiry, mult string
	var bytes, used int64
	err = queries.Row(ctx, s.q("SELECT l.node_id,l.rule_id,l.entitlement_id,l.expires_at,l.bytes,l.used,l.multiplier,COALESCE(r.closed,0) FROM commerce_leases l LEFT JOIN commerce_lease_reservations r ON r.lease_id=l.id WHERE l.id=?"), u.LeaseID).Scan(&node, &rule, &ent, &expiry, &bytes, &used, &mult, &closed)
	if err != nil {
		return err
	}
	if closed != 0 {
		return errors.New("lease retired")
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
	r, err := queries.Exec(ctx, s.q("UPDATE commerce_leases SET used=used+? WHERE id=? AND used=?"), raw, u.LeaseID, used)
	if err != nil {
		return err
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return ErrConflict
	}
	_, err = queries.Exec(ctx, s.q("UPDATE commerce_entitlements SET used=used+? WHERE id=?"), charge.Int64(), ent)
	if err != nil {
		return err
	}
	_, err = queries.Exec(ctx, s.q("INSERT INTO commerce_usage(id,lease_id,payload,charged) VALUES(?,?,?,?)"), u.ID, u.LeaseID, string(payload), charge.Int64())
	return err
}
