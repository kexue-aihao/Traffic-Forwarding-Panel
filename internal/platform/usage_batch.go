package platform

import (
	"context"
	"database/sql"
	"errors"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
	"math"
	"time"
)

func (s *Server) persistUsageBatch(ctx context.Context, tx *sql.Tx, node string, records []contract.UsageRecord) error {
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
		rule, node, ent                       string
		budget, before, used, expiry, retired int64
	}
	leases := map[string]*lease{}
	rows, e = tx.QueryContext(ctx, s.q("SELECT l.id,l.rule_id,l.node_id,l.entitlement_id,l.bytes_allocated,l.bytes_used,l.expires_at,COALESCE(ret.used_bytes,-1) FROM cp_rule_leases l LEFT JOIN cp_lease_retirements ret ON ret.id=l.id WHERE l.id IN("+storage.Placeholders(len(args))+")"), args...)
	if e != nil {
		return e
	}
	for rows.Next() {
		var id string
		l := &lease{}
		if e = rows.Scan(&id, &l.rule, &l.node, &l.ent, &l.budget, &l.before, &l.expiry, &l.retired); e != nil {
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
	commercial := []contract.UsageRecord{}
	inserts := [][]any{}
	for _, u := range fresh {
		l, ok := leases[u.LeaseID]
		if !ok || l.retired >= 0 || l.rule != u.RuleID || l.node != node || l.ent != u.EntitlementID || u.EndedAt.Unix() > l.expiry {
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
