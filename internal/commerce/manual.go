package commerce

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

// CreateManualOrder creates a pending order for an operator supplied wallet
// address. It deliberately does not mark anything paid: a transfer to a plain
// address has to be checked by an administrator (or a chain indexer) first.
func (s *Service) CreateManualOrder(ctx context.Context, user, channel, key string, amount int64, address string) (Order, error) {
	return s.CreateManualOrderAtomic(ctx, user, channel, key, amount, address, nil)
}

func (s *Service) CreateManualOrderAtomic(ctx context.Context, user, channel, key string, amount int64, address string, after func(*sql.Tx, Order) error) (Order, error) {
	var order Order
	if user == "" || channel == "" || len(channel) > 32 || key == "" || len(key) > 128 || amount <= 0 || amount > contract.MaxAmountCents || strings.TrimSpace(address) == "" {
		return order, errors.New("invalid manual payment")
	}
	if s.PaymentAllowed != nil {
		if err := s.PaymentAllowed(ctx, amount); err != nil {
			return order, err
		}
	}
	err := s.Write(ctx, func(tx *sql.Tx) error {
		if s.CheckAccount != nil {
			if err := s.CheckAccount(ctx, tx, user); err != nil {
				return err
			}
		}
		if _, _, err := s.walletTx(ctx, tx, user); err != nil {
			return err
		}
		var created string
		err := tx.QueryRowContext(ctx, s.q("SELECT id,channel,amount,payable_cents,status,payment_url,created_at FROM commerce_orders WHERE user_id=? AND idempotency_key=?"), user, key).Scan(&order.ID, &order.Channel, &order.Amount, &order.Payable, &order.Status, &order.PaymentURL, &created)
		if err == nil {
			if order.Channel != channel || order.Amount != amount || order.PaymentURL != address {
				return ErrConflict
			}
			order.Currency = "CNY"
			order.CreatedAt = parse(created)
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		order = Order{ID: id(), Channel: channel, Amount: amount, Payable: amount, Currency: "CNY", Status: "pending", PaymentURL: address, CreatedAt: s.Now().UTC()}
		if _, err = tx.ExecContext(ctx, s.q("INSERT INTO commerce_orders(id,user_id,channel,amount,payable_cents,status,payment_url,created_at,idempotency_key) VALUES(?,?,?,?,?,?,?,?,?)"), order.ID, user, channel, amount, amount, order.Status, address, stamp(order.CreatedAt), key); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, s.q("INSERT INTO commerce_attempts(order_id,state,provider_id,updated_at) VALUES(?,'manual','',?)"), order.ID, stamp(s.Now()))
		if err != nil {
			return err
		}
		if after != nil {
			return after(tx, order)
		}
		return nil
	})
	return order, err
}
