package commerce

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/payment"
)

func reconciliationGateway(t *testing.T, query func(http.ResponseWriter, *http.Request)) (Channel, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api.php" || r.URL.Query().Get("pid") != "merchant" || r.URL.Query().Get("key") != "secret" {
			t.Error("unauthenticated or unexpected query")
			w.WriteHeader(400)
			return
		}
		calls.Add(1)
		query(w, r)
	}))
	t.Cleanup(server.Close)
	adapter, err := payment.NewAdapter(payment.Configuration{Kind: "epay", Gateway: server.URL, MerchantID: "merchant", Key: "secret", Client: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return Channel{Adapter: adapter, NotifyURL: "https://panel.invalid/notify", ReturnURL: "https://panel.invalid/"}, &calls
}

func gatewayStatus(w http.ResponseWriter, r *http.Request, money, state string) {
	w.Header().Set("Content-Type", "application/json")
	order := r.URL.Query().Get("out_trade_no")
	json.NewEncoder(w).Encode(map[string]any{"code": 1, "pid": "merchant", "out_trade_no": order, "trade_no": "tx-" + order, "money": money, "status": state})
}

func scheduledOrder(t *testing.T, s *Service, channel Channel, key string) Order {
	t.Helper()
	o, err := s.CreateAdapterOrder(context.Background(), "alice", "epay", key, 1000, channel, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func retryState(t *testing.T, s *Service, order string) (int64, int64) {
	t.Helper()
	var attempts, next int64
	if err := s.DB.QueryRow(s.q("SELECT attempts,next_at FROM commerce_reconciliation WHERE order_id=?"), order).Scan(&attempts, &next); err != nil {
		t.Fatal(err)
	}
	return attempts, next
}

func TestReconciliationLostCallbackAndPaidIdempotency(t *testing.T) {
	s := fixture(t)
	now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	channel, calls := reconciliationGateway(t, func(w http.ResponseWriter, r *http.Request) { gatewayStatus(w, r, "10.00", "1") })
	order := scheduledOrder(t, s, channel, "lost-callback")
	channels := map[string]Channel{"epay": channel}
	ctx := context.Background()
	if n, err := s.ReconcilePending(ctx, channels, 10); n != 0 || err != nil {
		t.Fatal("queried before initial delay", n, err)
	}
	now = now.Add(reconciliationDelay)
	if n, err := s.ReconcilePending(ctx, channels, 10); n != 1 || err != nil {
		t.Fatal(n, err)
	}
	status := payment.Status{OrderID: order.ID, TransactionID: "tx-" + order.ID, AmountCents: 1000, Currency: "CNY", State: payment.Paid}
	for range 3 {
		if err := s.ConfirmStatus(ctx, "epay", status); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ReconcileOrder(ctx, "alice", order.ID, channels); err != nil {
			t.Fatal(err)
		}
		if n, err := s.ReconcilePending(ctx, channels, 10); n != 0 || err != nil {
			t.Fatal(n, err)
		}
	}
	wallet, err := s.Wallet(ctx, "alice")
	ledger, ledgerErr := s.Ledger(ctx, "alice")
	if err != nil || ledgerErr != nil || wallet.Balance != 1000 || len(ledger) != 1 || calls.Load() != 1 {
		t.Fatal(wallet, ledger, calls.Load(), err, ledgerErr)
	}
}

func TestReconciliationFailureBackoffRestartAndUnknownResult(t *testing.T) {
	s := fixture(t)
	now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	var mode atomic.Int32
	channel, calls := reconciliationGateway(t, func(w http.ResponseWriter, r *http.Request) {
		switch mode.Load() {
		case 0:
			w.WriteHeader(http.StatusBadGateway)
		case 1:
			gatewayStatus(w, r, "10.01", "1")
		case 2:
			gatewayStatus(w, r, "10.00", "0")
		default:
			gatewayStatus(w, r, "10.00", "1")
		}
	})
	order := scheduledOrder(t, s, channel, "retry")
	channels := map[string]Channel{"epay": channel}
	now = now.Add(reconciliationDelay)
	for attempt := int64(1); attempt <= 10; attempt++ {
		if n, err := s.ReconcilePending(context.Background(), channels, 1); n != 1 || err == nil {
			t.Fatal("failure was not retained", n, err)
		}
		got, next := retryState(t, s, order.ID)
		if got != attempt || next != now.Add(reconciliationBackoff(attempt)).UnixMilli() || next > now.Add(time.Hour).UnixMilli() {
			t.Fatal("retry schedule", got, next, now)
		}
		// Recreate the service as on process restart: no in-memory retry state.
		s = New(s.DB, s.Dialect, s.Write)
		s.Now = func() time.Time { return now }
		if err := s.Migrate(context.Background()); err != nil {
			t.Fatal(err)
		}
		if n, err := s.ReconcilePending(context.Background(), channels, 1); n != 0 || err != nil {
			t.Fatal("restart bypassed backoff", n, err)
		}
		now = time.UnixMilli(next)
	}
	mode.Store(1)
	if n, err := s.ReconcilePending(context.Background(), channels, 1); n != 1 || err == nil {
		t.Fatal("wrong amount accepted", n, err)
	}
	_, next := retryState(t, s, order.ID)
	now = time.UnixMilli(next)
	mode.Store(2)
	if n, err := s.ReconcilePending(context.Background(), channels, 1); n != 1 || err != nil {
		t.Fatal(n, err)
	}
	wallet, err := s.Wallet(context.Background(), "alice")
	if err != nil || wallet.Balance != 0 {
		t.Fatal("unknown/mismatched query credited funds", wallet, err)
	}
	_, next = retryState(t, s, order.ID)
	now = time.UnixMilli(next)
	mode.Store(3)
	if n, err := s.ReconcilePending(context.Background(), channels, 1); n != 1 || err != nil {
		t.Fatal(n, err)
	}
	wallet, err = s.Wallet(context.Background(), "alice")
	if err != nil || wallet.Balance != 1000 || calls.Load() != 13 {
		t.Fatal(wallet, calls.Load(), err)
	}
}

func TestReconciliationRestoredOrdersAndUnsupported(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	channel, calls := reconciliationGateway(t, func(w http.ResponseWriter, r *http.Request) { gatewayStatus(w, r, "10.00", "1") })
	for i := 0; i < 3; i++ {
		order := scheduledOrder(t, s, channel, id())
		// An old backup restored into an already migrated v2 database has no
		// schedule rows, including attempts interrupted in creating/uncertain.
		if err := s.Write(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, s.q("UPDATE commerce_attempts SET state=?,provider_id='' WHERE order_id=?"), []string{"creating", "uncertain", "ready"}[i], order.ID); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, s.q("DELETE FROM commerce_reconciliation WHERE order_id=?"), order.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	unsupported, err := payment.NewAdapter(payment.Configuration{Kind: "epusdt", Gateway: "https://gateway.invalid", Key: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	channels := map[string]Channel{"epay": {Adapter: unsupported}}
	if n, err := s.ReconcilePending(ctx, channels, 1); n != 0 || err != nil || calls.Load() != 0 {
		t.Fatal("unsupported query attempted", n, err)
	}
	channels["epay"] = channel
	for range 3 {
		if n, err := s.ReconcilePending(ctx, channels, 1); n != 1 || err != nil {
			t.Fatal("restored order not recovered in bounded batch", n, err)
		}
	}
	wallet, err := s.Wallet(ctx, "alice")
	if err != nil || wallet.Balance != 3000 || calls.Load() != 3 {
		t.Fatal(wallet, calls.Load(), err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.RunReconciliation(canceled, channels); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
