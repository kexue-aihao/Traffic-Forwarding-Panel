package commerce

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

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
