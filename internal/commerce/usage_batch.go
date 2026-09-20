package commerce

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"math/big"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
)

func (s *Service) SettleUsageBatch(ctx context.Context, tx *sql.Tx, records []contract.UsageRecord) error {
	if len(records) > 500 {
		return errors.New("usage batch exceeds 500")
	}
	if len(records) == 0 {
		return nil
	}
	payloads := map[string]string{}
	unique := []contract.UsageRecord{}
	args := []any{}
	for _, u := range records {
		if u.ID == "" || len(u.ID) > 128 || u.UploadBytes < 0 || u.DownloadBytes < 0 || u.UploadBytes > math.MaxInt64-u.DownloadBytes || u.StartedAt.IsZero() || u.EndedAt.Before(u.StartedAt) {
			return errors.New("invalid usage")
		}
		b, _ := json.Marshal(u)
		p := string(b)
		if old, ok := payloads[u.ID]; ok {
			if old != p {
				return ErrConflict
			}
			continue
		}
		payloads[u.ID] = p
		unique = append(unique, u)
		args = append(args, u.ID)
	}
	rows, e := tx.QueryContext(ctx, s.q("SELECT id,payload FROM commerce_usage WHERE id IN("+storage.Placeholders(len(args))+")"), args...)
	if e != nil {
		return e
	}
	existing := map[string]bool{}
	for rows.Next() {
		var id, p string
		if e = rows.Scan(&id, &p); e != nil {
			rows.Close()
			return e
		}
		if payloads[id] != p {
			rows.Close()
			return ErrConflict
		}
		existing[id] = true
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	fresh := []contract.UsageRecord{}
	leaseIDs := map[string]bool{}
	args = nil
	for _, u := range unique {
		if existing[u.ID] {
			continue
		}
		fresh = append(fresh, u)
		if !leaseIDs[u.LeaseID] {
			leaseIDs[u.LeaseID] = true
			args = append(args, u.LeaseID)
		}
	}
	if len(fresh) == 0 {
		return nil
	}
	type lease struct {
		node, rule, ent, expiry string
		budget, before, used    int64
		closed                  int
		mult                    *big.Rat
	}
	leases := map[string]*lease{}
	// Lock every lease, including zero-byte facts, in the same order. Retirement
	// holds this row until its reservation closes, so settlement cannot race the
	// refund. Read reservations separately after acquiring these locks: a joined
	// snapshot can predate a concurrent retirement that we waited for.
	lock := ""
	if s.Dialect != "sqlite" {
		lock = " FOR UPDATE"
	}
	rows, e = tx.QueryContext(ctx, s.q("SELECT id,node_id,rule_id,entitlement_id,expires_at,bytes,used,multiplier FROM commerce_leases WHERE id IN("+storage.Placeholders(len(args))+") ORDER BY id"+lock), args...)
	if e != nil {
		return e
	}
	for rows.Next() {
		var id, m string
		l := &lease{}
		if e = rows.Scan(&id, &l.node, &l.rule, &l.ent, &l.expiry, &l.budget, &l.before, &m); e != nil {
			rows.Close()
			return e
		}
		var ok bool
		l.mult, ok = new(big.Rat).SetString(m)
		if !ok || l.mult.Sign() <= 0 {
			rows.Close()
			return errors.New("invalid multiplier")
		}
		l.used = l.before
		leases[id] = l
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	// FOR UPDATE also makes this a current read under MySQL REPEATABLE READ.
	rows, e = tx.QueryContext(ctx, s.q("SELECT lease_id,closed FROM commerce_lease_reservations WHERE lease_id IN("+storage.Placeholders(len(args))+") ORDER BY lease_id"+lock), args...)
	if e != nil {
		return e
	}
	for rows.Next() {
		var id string
		var closed int
		if e = rows.Scan(&id, &closed); e != nil {
			rows.Close()
			return e
		}
		if l := leases[id]; l != nil {
			l.closed = closed
		}
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	charges := map[string]int64{}
	inserts := [][]any{}
	ceil := func(v int64, m *big.Rat) *big.Int {
		n := new(big.Int).Mul(big.NewInt(v), m.Num())
		n.Add(n, new(big.Int).Sub(m.Denom(), big.NewInt(1)))
		return n.Quo(n, m.Denom())
	}
	for _, u := range fresh {
		l, ok := leases[u.LeaseID]
		if !ok || l.closed != 0 || l.node != u.NodeID || l.rule != u.RuleID || l.ent != u.EntitlementID || u.EndedAt.After(parse(l.expiry)) {
			return errors.New("usage outside lease")
		}
		raw := u.UploadBytes + u.DownloadBytes
		if l.used < 0 || l.used > l.budget || raw > l.budget-l.used {
			return errors.New("lease quota exceeded")
		}
		delta := new(big.Int).Sub(ceil(l.used+raw, l.mult), ceil(l.used, l.mult))
		if !delta.IsInt64() || delta.Sign() < 0 || charges[l.ent] > math.MaxInt64-delta.Int64() {
			return errors.New("charge overflow")
		}
		l.used += raw
		charges[l.ent] += delta.Int64()
		inserts = append(inserts, []any{u.ID, u.LeaseID, payloads[u.ID], delta.Int64()})
	}
	changes := []storage.CounterChange{}
	for id, l := range leases {
		if l.used != l.before {
			changes = append(changes, storage.CounterChange{ID: id, Before: l.before, Delta: l.used - l.before})
		}
	}
	if e = storage.BulkCounter(ctx, tx, s.q, "commerce_leases", "used", "NOT EXISTS(SELECT 1 FROM commerce_lease_reservations r WHERE r.lease_id=commerce_leases.id AND r.closed<>0)", changes, true); e != nil {
		return e
	}
	changes = nil
	for id, delta := range charges {
		if delta > 0 {
			changes = append(changes, storage.CounterChange{ID: id, Delta: delta})
		}
	}
	if e = storage.BulkCounter(ctx, tx, s.q, "commerce_entitlements", "used", "", changes, false); e != nil {
		return e
	}
	return storage.BulkInsert(ctx, tx, s.q, "commerce_usage", "id,lease_id,payload,charged", inserts)
}
