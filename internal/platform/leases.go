package platform

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
	"net/http"
	"time"
)

// refreshLeases reuses live allocations; it never allocates on every config read.
// Allocation and publication share one transaction. Failures preserve the old
// configuration and never create an unmetered rule.
func (s *Server) refreshLeases(ctx context.Context, node string) error {
	return s.Store.Write(ctx, storage.Critical, func(tx *sql.Tx) error {
		rows, e := tx.QueryContext(ctx, s.q(`SELECT r.payload,g.payload,u.disabled,u.role FROM cp_rules r JOIN cp_groups g ON g.id=r.group_id JOIN cp_users u ON u.id=r.user_id WHERE r.node_id=? AND r.deleted=0`), node)
		if e != nil {
			return e
		}
		type item struct {
			rule     contract.Rule
			group    contract.Group
			disabled int
			role     string
		}
		items := []item{}
		for rows.Next() {
			var it item
			var rp, gp string
			if e = rows.Scan(&rp, &gp, &it.disabled, &it.role); e != nil {
				rows.Close()
				return e
			}
			if e = json.Unmarshal([]byte(rp), &it.rule); e != nil {
				rows.Close()
				return e
			}
			if e = json.Unmarshal([]byte(gp), &it.group); e != nil {
				rows.Close()
				return e
			}
			items = append(items, it)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		changed := false
		for _, it := range items {
			rule := it.rule
			g := it.group
			if !rule.Enabled || it.disabled != 0 || policyDenied(g, rule) || (it.role != "admin" && !contains(g.UserIDs, rule.UserID)) {
				continue
			}
			valid := rule.Lease != nil && rule.Lease.ExpiresAt.After(time.Now())
			if valid {
				var retired int
				if e = tx.QueryRowContext(ctx, s.q(`SELECT COUNT(*) FROM cp_lease_retirements WHERE id=?`), rule.Lease.ID).Scan(&retired); e != nil {
					return e
				}
				valid = retired == 0
			}
			if valid && rule.Lease.EntitlementID != "admin-test" {
				if s.opts.LeaseCurrent == nil {
					return errors.New("lease current validation required")
				}
				valid, e = s.opts.LeaseCurrent(ctx, tx, rule.UserID, rule.Lease)
				if e != nil {
					return e
				}
			}
			if valid {
				continue
			}
			oldLease := rule.Lease
			rule.Lease = nil
			if s.opts.Entitlements != nil {
				if a, ok := s.opts.Entitlements.(interface {
					AllocateWithMultiplier(context.Context, *sql.Tx, string, string, string, string) (*contract.Lease, error)
				}); ok {
					rule.Lease, e = a.AllocateWithMultiplier(ctx, tx, rule.UserID, rule.ID, node, g.Multiplier)
				} else {
					if g.Multiplier != "1" {
						return errors.New("multiplier allocator required")
					}
					rule.Lease, e = s.opts.Entitlements.Allocate(ctx, tx, rule.UserID, rule.ID, node)
				}
				if e != nil {
					if !errors.Is(e, contract.ErrEntitlementUnavailable) {
						return e
					}
					rule.Lease = nil
					e = nil
				}
			}
			// Explicit test grants are finite and not silently replenished.
			if oldLease == nil && rule.Lease == nil {
				continue
			}
			if rule.Lease != nil {
				if rule.Lease.Bytes <= 0 || !rule.Lease.ExpiresAt.After(time.Now()) || rule.Lease.ExpiresAt.After(time.Now().Add(24*time.Hour+time.Second)) {
					return errors.New("invalid lease")
				}
				if _, e = tx.ExecContext(ctx, s.q(`INSERT INTO cp_rule_leases(id,rule_id,node_id,entitlement_id,bytes_allocated,bytes_used,expires_at) VALUES(?,?,?,?,?,0,?)`), rule.Lease.ID, rule.ID, node, rule.Lease.EntitlementID, rule.Lease.Bytes, rule.Lease.ExpiresAt.Unix()); e != nil {
					return e
				}
			}
			res, e := tx.ExecContext(ctx, s.q(`UPDATE cp_rules SET payload=? WHERE id=? AND version=? AND deleted=0`), strJSON(rule), rule.ID, rule.Version)
			if e != nil {
				return e
			}
			n, _ := res.RowsAffected()
			if n != 1 {
				return errConflict
			}
			changed = true
		}
		if changed {
			_, e = tx.ExecContext(ctx, s.q(`UPDATE cp_nodes SET desired_version=desired_version+1 WHERE id=?`), node)
		}
		return e
	})
}
func (s *Server) retireLease(w http.ResponseWriter, r *http.Request) {
	var in struct {
		LeaseID string `json:"lease_id"`
		Used    int64  `json:"used_bytes,string"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Used < 0 {
		fail(w, 400, "invalid usage")
		return
	}
	node := r.Context().Value(nodeKey{}).(string)
	e := s.Store.Write(r.Context(), storage.Critical, func(tx *sql.Tx) error {
		var owner, ent string
		var used int64
		query := `SELECT node_id,entitlement_id,bytes_used FROM cp_rule_leases WHERE id=?`
		if s.Store.Dialect != "sqlite" {
			query += " FOR UPDATE"
		}
		if e := tx.QueryRowContext(r.Context(), s.q(query), in.LeaseID).Scan(&owner, &ent, &used); e != nil {
			return e
		}
		if owner != node || used != in.Used {
			return errors.New("lease owner or persisted usage mismatch")
		}
		var previous int64
		e := tx.QueryRowContext(r.Context(), s.q(`SELECT used_bytes FROM cp_lease_retirements WHERE id=?`), in.LeaseID).Scan(&previous)
		if e == nil {
			if previous != in.Used {
				return errConflict
			}
			return nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if ent != "admin-test" {
			if s.opts.RetireLease == nil {
				return errors.New("retirement unavailable")
			}
			if e = s.opts.RetireLease(r.Context(), tx, node, in.LeaseID, in.Used); e != nil {
				return e
			}
		}
		if _, e = tx.ExecContext(r.Context(), s.q(`INSERT INTO cp_lease_retirements(id,used_bytes,retired_at) VALUES(?,?,?)`), in.LeaseID, in.Used, time.Now().Unix()); e != nil {
			return e
		}
		_, e = tx.ExecContext(r.Context(), s.q(`UPDATE cp_nodes SET desired_version=desired_version+1 WHERE id=?`), node)
		return e
	})
	if e != nil {
		fail(w, 409, "retirement requires matching persisted usage and valid lease")
		return
	}
	w.WriteHeader(204)
}
