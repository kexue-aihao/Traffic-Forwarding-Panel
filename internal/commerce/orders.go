package commerce

import (
	"context"
	"database/sql"
	"errors"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/payment"
)

func (s *Service) CreateOrder(ctx context.Context, user, channel, key string, amount int64, gateway payment.EPay) (Order, error) {
	var o Order
	if channel != "epay" || amount <= 0 || amount > 100000000 || len(key) == 0 || len(key) > 128 {
		return o, errors.New("invalid or unavailable channel/amount/key")
	}
	err := s.Write(ctx, func(tx *sql.Tx) error {
		if _, _, err := s.walletTx(ctx, tx, user); err != nil {
			return err
		}
		var created string
		e := tx.QueryRowContext(ctx, s.q("SELECT id,channel,amount,status,payment_url,created_at FROM commerce_orders WHERE user_id=? AND idempotency_key=?"), user, key).Scan(&o.ID, &o.Channel, &o.Amount, &o.Status, &o.PaymentURL, &created)
		if e == nil {
			if o.Channel != channel || o.Amount != amount {
				return ErrConflict
			}
			o.Currency = "CNY"
			o.CreatedAt = parse(created)
			return nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		o = Order{ID: id(), Channel: channel, Amount: amount, Currency: "CNY", Status: "pending", CreatedAt: s.Now().UTC()}
		o.PaymentURL, e = gateway.Create(o.ID, amount)
		if e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, s.q("INSERT INTO commerce_orders(id,user_id,channel,amount,status,payment_url,created_at,idempotency_key) VALUES(?,?,?,?,?,?,?,?)"), o.ID, user, channel, amount, o.Status, o.PaymentURL, stamp(o.CreatedAt), key)
		if e == nil {
			e = s.scheduleReconciliation(ctx, tx, o.ID)
		}
		return e
	})
	return o, err
}
func (s *Service) ConfirmPayment(ctx context.Context, channel, order, transaction string, amount int64) error {
	if transaction == "" || len(transaction) > 128 {
		return errors.New("invalid transaction")
	}
	return s.Write(ctx, func(tx *sql.Tx) error {
		var user, c, status string
		var actual int64
		var prev sql.NullString
		query := "SELECT user_id,channel,amount,status,provider_tx FROM commerce_orders WHERE id=?"
		if s.Dialect != "sqlite" {
			query += " FOR UPDATE"
		}
		e := tx.QueryRowContext(ctx, s.q(query), order).Scan(&user, &c, &actual, &status, &prev)
		if e != nil {
			return e
		}
		if c != channel || actual != amount {
			return errors.New("payment mismatch")
		}
		if status == "paid" {
			if prev.String != transaction {
				return ErrConflict
			}
			return nil
		}
		var existing string
		e = tx.QueryRowContext(ctx, s.q("SELECT id FROM commerce_orders WHERE channel=? AND provider_tx=?"), channel, transaction).Scan(&existing)
		if e == nil {
			return ErrConflict
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if e = s.post(ctx, tx, user, amount, "recharge", "payment:"+channel+":"+transaction); e != nil {
			return e
		}
		r, e := tx.ExecContext(ctx, s.q("UPDATE commerce_orders SET status='paid',provider_tx=? WHERE id=? AND status='pending'"), transaction, order)
		if e != nil {
			return e
		}
		n, _ := r.RowsAffected()
		if n != 1 {
			return ErrConflict
		}
		_, e = tx.ExecContext(ctx, s.q("DELETE FROM commerce_reconciliation WHERE order_id=?"), order)
		return e
	})
}
func (s *Service) Orders(ctx context.Context, user string) ([]Order, error) {
	rows, e := s.DB.QueryContext(ctx, s.q("SELECT id,channel,amount,status,payment_url,created_at FROM commerce_orders WHERE user_id=? ORDER BY created_at DESC LIMIT 100"), user)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Order{}
	for rows.Next() {
		var o Order
		var date string
		if e = rows.Scan(&o.ID, &o.Channel, &o.Amount, &o.Status, &o.PaymentURL, &date); e != nil {
			return nil, e
		}
		o.Currency = "CNY"
		o.CreatedAt = parse(date)
		out = append(out, o)
	}
	return out, rows.Err()
}
func (s *Service) Ledger(ctx context.Context, user string) ([]Ledger, error) {
	rows, e := s.DB.QueryContext(ctx, s.q("SELECT id,amount,balance,kind,reference_id,created_at FROM commerce_ledger WHERE user_id=? ORDER BY created_at DESC LIMIT 100"), user)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Ledger{}
	for rows.Next() {
		var v Ledger
		var date string
		if e = rows.Scan(&v.ID, &v.Amount, &v.Balance, &v.Kind, &v.Reference, &date); e != nil {
			return nil, e
		}
		v.CreatedAt = parse(date)
		out = append(out, v)
	}
	return out, rows.Err()
}
