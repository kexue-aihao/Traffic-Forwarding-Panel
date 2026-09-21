package commerce

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

type Refund struct {
	ID         string    `json:"id"`
	OrderID    string    `json:"order_id"`
	Amount     int64     `json:"amount_cents,string"`
	Status     string    `json:"status"`
	Reason     string    `json:"reason"`
	ActorID    string    `json:"actor_id"`
	Evidence   string    `json:"evidence,omitempty"`
	ResolvedBy string    `json:"resolved_by,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

const refundFields = "id,order_id,amount,status,reason,actor_id,evidence,resolved_by,created_at,updated_at"

func scanRefund(row scanner) (Refund, error) {
	var v Refund
	var created, updated string
	err := row.Scan(&v.ID, &v.OrderID, &v.Amount, &v.Status, &v.Reason, &v.ActorID, &v.Evidence, &v.ResolvedBy, &created, &updated)
	v.CreatedAt = parse(created)
	v.UpdatedAt = parse(updated)
	return v, err
}

// RequestRefund reserves available wallet funds. It does not transfer funds at
// a provider or claim a completed refund. Partial reservations are supported.
func (s *Service) RequestRefund(ctx context.Context, actor, order, key, reason string, amount int64) (Refund, error) {
	var out Refund
	if actor == "" || len(key) < 1 || len(key) > 128 || amount <= 0 || strings.TrimSpace(reason) == "" || len(reason) > 500 {
		return out, errors.New("invalid refund request")
	}
	err := s.Write(ctx, func(tx *sql.Tx) error {
		q := "SELECT user_id,amount,status FROM commerce_orders WHERE id=?"
		if s.Dialect != "sqlite" {
			q += " FOR UPDATE"
		}
		var user, status string
		var paid int64
		if err := tx.QueryRowContext(ctx, s.q(q), order).Scan(&user, &paid, &status); err != nil {
			return err
		}
		previous, err := scanRefund(tx.QueryRowContext(ctx, s.q("SELECT "+refundFields+" FROM commerce_refunds WHERE order_id=? AND idempotency_key=?"), order, key))
		if err == nil {
			if previous.Amount != amount || previous.Reason != reason {
				return ErrConflict
			}
			out = previous
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if status != "paid" && status != "paid_late" && status != "partially_refunded" {
			return errors.New("order unavailable for refund")
		}
		var reserved int64
		if err := tx.QueryRowContext(ctx, s.q("SELECT COALESCE(SUM(amount),0) FROM commerce_refunds WHERE order_id=? AND status<>'canceled'"), order).Scan(&reserved); err != nil {
			return err
		}
		if amount > paid-reserved {
			return errors.New("refund exceeds unrefunded amount")
		}
		now := s.Now().UTC()
		out = Refund{ID: id(), OrderID: order, Amount: amount, Status: "pending_external", Reason: reason, ActorID: actor, CreatedAt: now, UpdatedAt: now}
		_, err = tx.ExecContext(ctx, s.q("INSERT INTO commerce_refunds(id,order_id,user_id,amount,status,reason,actor_id,idempotency_key,evidence,resolved_by,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,'','',?,?)"), out.ID, order, user, amount, out.Status, reason, actor, key, stamp(now), stamp(now))
		if err != nil {
			return err
		}
		return s.post(ctx, tx, user, -amount, "refund_reserve", "refund-reserve:"+out.ID)
	})
	return out, err
}

// ResolveRefund records an operator-verified external transfer reference, or
// releases the reservation after a confirmed cancellation. Both are replayable.
func (s *Service) ResolveRefund(ctx context.Context, actor, refundID, evidence string, completed bool) (Refund, error) {
	var out Refund
	if actor == "" || strings.TrimSpace(evidence) == "" || len(evidence) > 500 {
		return out, errors.New("resolution evidence required")
	}
	err := s.Write(ctx, func(tx *sql.Tx) error {
		var order string
		if err := tx.QueryRowContext(ctx, s.q("SELECT order_id FROM commerce_refunds WHERE id=?"), refundID).Scan(&order); err != nil {
			return err
		}
		q := "SELECT user_id,amount FROM commerce_orders WHERE id=?"
		if s.Dialect != "sqlite" {
			q += " FOR UPDATE"
		}
		var user string
		var paid int64
		if err := tx.QueryRowContext(ctx, s.q(q), order).Scan(&user, &paid); err != nil {
			return err
		}
		var err error
		out, err = scanRefund(tx.QueryRowContext(ctx, s.q("SELECT "+refundFields+" FROM commerce_refunds WHERE id=?"), refundID))
		if err != nil {
			return err
		}
		status := "canceled"
		if completed {
			status = "completed"
		}
		if out.Status != "pending_external" {
			if out.Status == status && out.Evidence == evidence {
				return nil
			}
			return ErrConflict
		}
		if !completed {
			if err = s.post(ctx, tx, user, out.Amount, "refund_release", "refund-release:"+out.ID); err != nil {
				return err
			}
		}
		out.Status = status
		out.Evidence = evidence
		out.ResolvedBy = actor
		out.UpdatedAt = s.Now().UTC()
		if _, err = tx.ExecContext(ctx, s.q("UPDATE commerce_refunds SET status=?,evidence=?,resolved_by=?,updated_at=? WHERE id=?"), status, evidence, actor, stamp(out.UpdatedAt), out.ID); err != nil {
			return err
		}
		if completed {
			var total int64
			if err = tx.QueryRowContext(ctx, s.q("SELECT COALESCE(SUM(amount),0) FROM commerce_refunds WHERE order_id=? AND status='completed'"), order).Scan(&total); err != nil {
				return err
			}
			orderStatus := "partially_refunded"
			if total == paid {
				orderStatus = "refunded"
			}
			if _, err = tx.ExecContext(ctx, s.q("UPDATE commerce_orders SET status=? WHERE id=?"), orderStatus, order); err != nil {
				return err
			}
		}
		return s.emitEventTx(ctx, tx, user, "refund."+status, map[string]any{"refund_id": out.ID, "order_id": order})
	})
	return out, err
}
func (s *Service) Refunds(ctx context.Context, user, order string, admin bool) ([]Refund, error) {
	var owner string
	if err := s.DB.QueryRowContext(ctx, s.q("SELECT user_id FROM commerce_orders WHERE id=?"), order).Scan(&owner); err != nil {
		return nil, err
	}
	if !admin && owner != user {
		return nil, sql.ErrNoRows
	}
	rows, err := s.DB.QueryContext(ctx, s.q("SELECT "+refundFields+" FROM commerce_refunds WHERE order_id=? ORDER BY created_at,id"), order)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Refund{}
	for rows.Next() {
		v, err := scanRefund(rows)
		if err != nil {
			return nil, err
		}
		if !admin {
			v.ActorID = ""
			v.ResolvedBy = ""
			v.Evidence = ""
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
