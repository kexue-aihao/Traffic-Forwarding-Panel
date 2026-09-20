package commerce

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/payment"
)

type Channel struct {
	Adapter   payment.Adapter
	NotifyURL string
	ReturnURL string
	Method    string
}

// CreateAdapterOrder durably allocates an order before calling any gateway.
// An ambiguous creation is never retried as a new external order automatically.
func (s *Service) CreateAdapterOrder(ctx context.Context, user, channel, key string, amount int64, configured Channel, clientIP string) (Order, error) {
	var order Order
	if configured.Adapter == nil || amount <= 0 || amount > 100000000 || key == "" || len(key) > 128 {
		return order, errors.New("invalid or unavailable channel/amount/key")
	}
	fresh := false
	err := s.Write(ctx, func(tx *sql.Tx) error {
		if _, _, e := s.walletTx(ctx, tx, user); e != nil {
			return e
		}
		var created string
		e := tx.QueryRowContext(ctx, s.q("SELECT id,channel,amount,status,payment_url,created_at FROM commerce_orders WHERE user_id=? AND idempotency_key=?"), user, key).Scan(&order.ID, &order.Channel, &order.Amount, &order.Status, &order.PaymentURL, &created)
		if e == nil {
			if order.Channel != channel || order.Amount != amount {
				return ErrConflict
			}
			order.Currency = "CNY"
			order.CreatedAt = parse(created)
			return nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		order = Order{ID: id(), Channel: channel, Amount: amount, Currency: "CNY", Status: "pending", CreatedAt: s.Now().UTC()}
		if _, e = tx.ExecContext(ctx, s.q("INSERT INTO commerce_orders(id,user_id,channel,amount,status,payment_url,created_at,idempotency_key) VALUES(?,?,?,?,'pending','',?,?)"), order.ID, user, channel, amount, stamp(order.CreatedAt), key); e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, s.q("INSERT INTO commerce_attempts(order_id,state,provider_id,updated_at) VALUES(?,'creating','',?)"), order.ID, stamp(s.Now()))
		fresh = e == nil
		return e
	})
	if err != nil {
		return order, err
	}
	if !fresh {
		return order, nil
	}
	created, callErr := configured.Adapter.Create(ctx, payment.CreateRequest{OrderID: order.ID, UserKey: user, Name: "钱包充值", Currency: "CNY", AmountCents: amount, NotifyURL: configured.NotifyURL, ReturnURL: configured.ReturnURL, ClientIP: clientIP, Method: configured.Method})
	if callErr == nil && (created.OrderID != order.ID || created.Currency != "CNY" || created.AmountCents != amount || created.PaymentURL == "") {
		callErr = payment.ErrProtocol
	}
	// Persist uncertainty even when the HTTP client disconnected during creation.
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	err = s.Write(persistCtx, func(tx *sql.Tx) error {
		state := "ready"
		provider := ""
		if callErr != nil {
			state = "uncertain"
		} else {
			provider = created.TransactionID
		}
		if _, e := tx.ExecContext(persistCtx, s.q("UPDATE commerce_attempts SET state=?,provider_id=?,updated_at=? WHERE order_id=?"), state, provider, stamp(s.Now()), order.ID); e != nil {
			return e
		}
		if callErr == nil {
			_, e := tx.ExecContext(persistCtx, s.q("UPDATE commerce_orders SET payment_url=? WHERE id=?"), created.PaymentURL, order.ID)
			return e
		}
		return nil
	})
	if err != nil {
		return order, err
	}
	if callErr != nil {
		return order, errors.New("payment creation uncertain; reconcile this order before retrying")
	}
	order.PaymentURL = created.PaymentURL
	return order, nil
}

// ReconcileOrder uses the channel's authenticated query and the same financial
// confirmation path as callbacks. It never infers payment from a return URL.
func (s *Service) ReconcileOrder(ctx context.Context, user, orderID string, channels map[string]Channel) (Order, error) {
	var order Order
	var provider, created string
	err := s.DB.QueryRowContext(ctx, s.q("SELECT o.id,o.channel,o.amount,o.status,o.payment_url,o.created_at,COALESCE(a.provider_id,'') FROM commerce_orders o LEFT JOIN commerce_attempts a ON a.order_id=o.id WHERE o.id=? AND o.user_id=?"), orderID, user).Scan(&order.ID, &order.Channel, &order.Amount, &order.Status, &order.PaymentURL, &created, &provider)
	if err != nil {
		return order, err
	}
	order.Currency = "CNY"
	order.CreatedAt = parse(created)
	c, ok := channels[order.Channel]
	if !ok || c.Adapter == nil || !c.Adapter.Capabilities().Query {
		return order, payment.ErrUnsupported
	}
	status, err := c.Adapter.Query(ctx, payment.QueryRequest{OrderID: orderID, TransactionID: provider})
	if err != nil {
		return order, err
	}
	if err = payment.ValidateExpected(status, order.ID, order.Amount, "CNY"); err != nil {
		return order, err
	}
	if status.State == payment.Paid {
		if err = s.ConfirmPayment(ctx, order.Channel, order.ID, status.TransactionID, status.AmountCents); err != nil {
			return order, err
		}
		order.Status = "paid"
	}
	// Pending/expired gateway states never discard a subsequently valid receipt.
	return order, nil
}

func (s *Service) ConfirmStatus(ctx context.Context, channel string, status payment.Status) error {
	if status.State != payment.Paid {
		return payment.ErrProtocol
	}
	var expected int64
	if err := s.DB.QueryRowContext(ctx, s.q("SELECT amount FROM commerce_orders WHERE id=? AND channel=?"), status.OrderID, channel).Scan(&expected); err != nil {
		return err
	}
	if err := payment.ValidateExpected(status, status.OrderID, expected, "CNY"); err != nil {
		return err
	}
	return s.ConfirmPayment(ctx, channel, status.OrderID, status.TransactionID, status.AmountCents)
}
