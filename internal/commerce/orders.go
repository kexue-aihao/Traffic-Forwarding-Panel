package commerce

import (
	"context"
	"database/sql"
	"errors"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/payment"
)

// CreateOrder 是环境变量配置的旧版 EPay 通道。它的手续费始终为 0 —— 旧配置
// 里没有这个字段，也不该在升级时凭空多出一笔费用。
func (s *Service) CreateOrder(ctx context.Context, user, channel, key string, amount int64, gateway payment.EPay) (Order, error) {
	if s.PaymentAllowed != nil {
		if err := s.PaymentAllowed(ctx, amount); err != nil {
			return Order{}, err
		}
	}
	var o Order
	if channel != "epay" || amount <= 0 || amount > contract.MaxAmountCents || len(key) == 0 || len(key) > 128 {
		return o, errors.New("invalid or unavailable channel/amount/key")
	}
	err := s.Write(ctx, func(tx *sql.Tx) error {
		if _, _, err := s.walletTx(ctx, tx, user); err != nil {
			return err
		}
		var created string
		e := tx.QueryRowContext(ctx, s.q("SELECT id,channel,amount,payable_cents,status,payment_url,created_at FROM commerce_orders WHERE user_id=? AND idempotency_key=?"), user, key).Scan(&o.ID, &o.Channel, &o.Amount, &o.Payable, &o.Status, &o.PaymentURL, &created)
		if e == nil {
			if o.Channel != channel || o.Amount != amount {
				return ErrConflict
			}
			o.Currency = contract.SettlementCurrency
			o.CreatedAt = parse(created)
			o.resolvePayable(Channel{})
			return nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		o = Order{ID: id(), Channel: channel, Amount: amount, Payable: amount, Currency: contract.SettlementCurrency, Status: "pending", CreatedAt: s.Now().UTC()}
		o.PaymentURL, e = gateway.Create(o.ID, amount)
		if e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, s.q("INSERT INTO commerce_orders(id,user_id,channel,amount,payable_cents,status,payment_url,created_at,idempotency_key) VALUES(?,?,?,?,?,?,?,?,?)"), o.ID, user, channel, amount, amount, o.Status, o.PaymentURL, stamp(o.CreatedAt), key)
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
		var credit, payable int64
		var prev sql.NullString
		query := "SELECT user_id,channel,amount,payable_cents,status,provider_tx FROM commerce_orders WHERE id=?"
		if s.Dialect != "sqlite" {
			query += " FOR UPDATE"
		}
		e := tx.QueryRowContext(ctx, s.q(query), order).Scan(&user, &c, &credit, &payable, &status, &prev)
		if e != nil {
			return e
		}
		// 网关报的是它实际收的钱（含手续费）；老订单没有 payable_cents，
		// 那时两者相等。核对用付款额，入账用充值额。
		if payable <= 0 {
			payable = credit
		}
		if c != channel || payable != amount {
			return errors.New("payment mismatch")
		}
		if status == "paid" || status == "paid_late" || status == "refunded" || status == "partially_refunded" {
			if prev.String != transaction {
				return ErrConflict
			}
			return nil
		}
		if status != "pending" && status != "closed" && status != "expired" {
			return errors.New("order is not payable")
		}
		var existing string
		e = tx.QueryRowContext(ctx, s.q("SELECT id FROM commerce_orders WHERE channel=? AND provider_tx=?"), channel, transaction).Scan(&existing)
		if e == nil {
			return ErrConflict
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if e = s.post(ctx, tx, user, credit, "recharge", "payment:"+order); e != nil {
			return e
		}
		nextStatus := "paid"
		if status != "pending" {
			nextStatus = "paid_late"
		}
		r, e := tx.ExecContext(ctx, s.q("UPDATE commerce_orders SET status=?,provider_tx=? WHERE id=? AND status IN ('pending','closed','expired')"), nextStatus, transaction, order)
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
