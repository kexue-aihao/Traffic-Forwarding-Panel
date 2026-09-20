package platform

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
)

func (s *Server) persistUsageBatch(ctx context.Context, tx *sql.Tx, node string, records []contract.UsageRecord) error {
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
		if u.NodeID != node || u.ID == "" || len(u.ID) > 128 || u.UploadBytes < 0 || u.DownloadBytes < 0 || u.UploadBytes > math.MaxInt64-u.DownloadBytes || u.StartedAt.IsZero() || u.EndedAt.Before(u.StartedAt) || u.EndedAt.After(time.Now().Add(time.Minute)) {
			return errors.New("invalid usage record")
		}
		payload := strJSON(u)
		if old, ok := payloads[u.ID]; ok {
			if old != payload {
				return errConflict
			}
			continue
		}
		payloads[u.ID] = payload
		unique = append(unique, u)
		args = append(args, u.ID)
	}
	rows, e := tx.QueryContext(ctx, s.q("SELECT id,payload FROM cp_usage WHERE id IN("+storage.Placeholders(len(args))+")"), args...)
	if e != nil {
		return e
	}
	exists := map[string]bool{}
	for rows.Next() {
		var id, p string
		if e = rows.Scan(&id, &p); e != nil {
			rows.Close()
			return e
		}
		if payloads[id] != p {
			rows.Close()
			return errConflict
		}
		exists[id] = true
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	fresh := []contract.UsageRecord{}
	seenLeases := map[string]bool{}
	args = nil
	for _, u := range unique {
		if exists[u.ID] {
			continue
		}
		fresh = append(fresh, u)
		if !seenLeases[u.LeaseID] {
			seenLeases[u.LeaseID] = true
			args = append(args, u.LeaseID)
		}
	}
	if len(fresh) == 0 {
		return nil
	}
	type lease struct {
		rule, node, ent              string
		budget, before, used, expiry int64
		retired                      bool
	}
	leases := map[string]*lease{}
	// Share the retirement lock order: control-plane leases, then commercial
	// leases. Lock zero-byte facts too, before checking retirement in a fresh read.
	lock := ""
	if s.Store.Dialect != "sqlite" {
		lock = " FOR UPDATE"
	}
	rows, e = tx.QueryContext(ctx, s.q("SELECT id,rule_id,node_id,entitlement_id,bytes_allocated,bytes_used,expires_at FROM cp_rule_leases WHERE id IN("+storage.Placeholders(len(args))+") ORDER BY id"+lock), args...)
	if e != nil {
		return e
	}
	for rows.Next() {
		var id string
		l := &lease{}
		if e = rows.Scan(&id, &l.rule, &l.node, &l.ent, &l.budget, &l.before, &l.expiry); e != nil {
			rows.Close()
			return e
		}
		l.used = l.before
		leases[id] = l
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	rows, e = tx.QueryContext(ctx, s.q("SELECT id FROM cp_lease_retirements WHERE id IN("+storage.Placeholders(len(args))+") ORDER BY id"+lock), args...)
	if e != nil {
		return e
	}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return e
		}
		if l := leases[id]; l != nil {
			l.retired = true
		}
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	commercial := []contract.UsageRecord{}
	inserts := [][]any{}
	for _, u := range fresh {
		l, ok := leases[u.LeaseID]
		if !ok || l.retired || l.rule != u.RuleID || l.node != node || l.ent != u.EntitlementID || u.EndedAt.Unix() > l.expiry {
			return errors.New("usage outside lease")
		}
		amount := u.UploadBytes + u.DownloadBytes
		if l.used < 0 || l.used > l.budget || amount > l.budget-l.used {
			return errors.New("lease quota exceeded")
		}
		l.used += amount
		if l.ent != "admin-test" {
			commercial = append(commercial, u)
		}
		inserts = append(inserts, []any{u.ID, node, u.RuleID, u.LeaseID, payloads[u.ID], time.Now().Unix(), 1})
	}
	changes := []storage.CounterChange{}
	for id, l := range leases {
		if l.used != l.before {
			changes = append(changes, storage.CounterChange{ID: id, Before: l.before, Delta: l.used - l.before})
		}
	}
	if e = storage.BulkCounter(ctx, tx, s.q, "cp_rule_leases", "bytes_used", "NOT EXISTS(SELECT 1 FROM cp_lease_retirements r WHERE r.id=cp_rule_leases.id)", changes, true); e != nil {
		return e
	}
	if len(commercial) > 0 {
		if settle, ok := s.opts.Entitlements.(interface {
			SettleUsageBatch(context.Context, *sql.Tx, []contract.UsageRecord) error
		}); ok {
			e = settle.SettleUsageBatch(ctx, tx, commercial)
		} else if settle, ok := s.opts.Entitlements.(interface {
			SettleUsage(context.Context, *sql.Tx, contract.UsageRecord) error
		}); ok {
			for _, u := range commercial {
				if e = settle.SettleUsage(ctx, tx, u); e != nil {
					break
				}
			}
		} else {
			return errors.New("commercial settlement unavailable")
		}
		if e != nil {
			return e
		}
	}
	return storage.BulkInsert(ctx, tx, s.q, "cp_usage", "id,node_id,rule_id,lease_id,payload,received_at,settled", inserts)
}
