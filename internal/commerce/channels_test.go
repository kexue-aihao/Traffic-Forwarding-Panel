package commerce

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/payment"
)

type countingAdapter struct {
	requests  atomic.Int32
	createErr error
	status    payment.Status
}

func (a *countingAdapter) Create(_ context.Context, r payment.CreateRequest) (payment.Created, error) {
	a.requests.Add(1)
	a.status = payment.Status{OrderID: r.OrderID, AmountCents: r.AmountCents, Currency: r.Currency, TransactionID: "remote-order", State: payment.Paid}
	return payment.Created{OrderID: r.OrderID, AmountCents: r.AmountCents, Currency: r.Currency, TransactionID: "remote-order", PaymentURL: "https://example.invalid/pay"}, a.createErr
}
func (a *countingAdapter) Query(context.Context, payment.QueryRequest) (payment.Status, error) {
	return a.status, nil
}
func (a *countingAdapter) VerifyNotify([]byte, string) (payment.Status, error) { return a.status, nil }
func (a *countingAdapter) Capabilities() payment.Capabilities {
	return payment.Capabilities{Create: true, Query: true, Notify: true, NotifyAck: "ok"}
}

func TestGatewayCreationAmbiguityAndReconciliation(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	adapter := &countingAdapter{createErr: errors.New("timeout after gateway accepted")}
	channel := Channel{Adapter: adapter, NotifyURL: "https://panel.invalid/notify", ReturnURL: "https://panel.invalid/"}
	o, e := s.CreateAdapterOrder(ctx, "alice", "tokenpay", "topup", 1000, channel, "127.0.0.1")
	if e == nil || o.ID == "" {
		t.Fatal("uncertain remote result not retained", e)
	}
	retry, e := s.CreateAdapterOrder(ctx, "alice", "tokenpay", "topup", 1000, channel, "127.0.0.1")
	if e != nil || retry.ID != o.ID || adapter.requests.Load() != 1 {
		t.Fatal("ambiguous create retried", retry, e)
	}
	if _, e = s.CreateAdapterOrder(ctx, "alice", "tokenpay", "topup", 2000, channel, "127.0.0.1"); !errors.Is(e, ErrConflict) {
		t.Fatal("idempotency payload conflict missing", e)
	}
	channels := map[string]Channel{"tokenpay": channel}
	if _, e = s.ReconcileOrder(ctx, "bob", o.ID, channels); e == nil {
		t.Fatal("cross-user reconciliation")
	}
	for range 2 {
		v, e := s.ReconcileOrder(ctx, "alice", o.ID, channels)
		if e != nil || v.Status != "paid" {
			t.Fatal(v, e)
		}
	}
	w, e := s.Wallet(ctx, "alice")
	if e != nil || w.Balance != 1000 {
		t.Fatal(w, e)
	}
	bad := adapter.status
	bad.Currency = "USD"
	if e = s.ConfirmStatus(ctx, "tokenpay", bad); e == nil {
		t.Fatal("currency mismatch accepted")
	}
}

func TestHasActivePaymentOrdersKeepsChannelCredentialsStable(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	_, err := s.DB.ExecContext(ctx, s.q(`INSERT INTO commerce_orders(id,user_id,channel,amount,payable_cents,status,payment_url,created_at,idempotency_key) VALUES(?,?,?,?,?,'pending','',?,?)`), "order-pending", "alice", "epay", 1000, 1000, stamp(time.Now()), "key-pending")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, s.q(`INSERT INTO commerce_attempts(order_id,state,provider_id,updated_at) VALUES(?,'ready','',?)`), "order-pending", stamp(time.Now())); err != nil {
		t.Fatal(err)
	}
	active, err := s.HasActivePaymentOrders(ctx, "epay")
	if err != nil || !active {
		t.Fatalf("pending order was not detected: active=%v err=%v", active, err)
	}
	if _, err := s.DB.ExecContext(ctx, s.q(`UPDATE commerce_orders SET status='paid' WHERE id=?`), "order-pending"); err != nil {
		t.Fatal(err)
	}
	active, err = s.HasActivePaymentOrders(ctx, "epay")
	if err != nil || active {
		t.Fatalf("terminal order still blocks channel: active=%v err=%v", active, err)
	}
	if _, err := s.DB.ExecContext(ctx, s.q(`UPDATE commerce_orders SET status='pending' WHERE id=?`), "order-pending"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, s.q(`UPDATE commerce_attempts SET state='uncertain' WHERE order_id=?`), "order-pending"); err != nil {
		t.Fatal(err)
	}
	active, err = s.HasActivePaymentOrders(ctx, "epay")
	if err != nil || !active {
		t.Fatalf("uncertain attempt was not detected: active=%v err=%v", active, err)
	}
}
