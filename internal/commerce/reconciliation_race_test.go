package commerce

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/payment"
)

func TestReconciliationCallbackWhileGatewayBlocked(t *testing.T) {
	s := fixture(t)
	start := time.Now().UTC()
	s.Now = func() time.Time { return start }
	entered, release := make(chan struct{}), make(chan struct{})
	released := false
	channel, calls := reconciliationGateway(t, func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		gatewayStatus(w, r, "10.00", "1")
	})
	defer func() {
		if !released {
			close(release)
		}
	}()
	order := scheduledOrder(t, s, channel, "concurrent-callback")
	due := start.Add(time.Minute)
	s.Now = func() time.Time { return due }
	channels := map[string]Channel{"epay": channel}
	done := make(chan error, 1)
	go func() { _, err := s.ReconcilePending(context.Background(), channels, 1); done <- err }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("gateway was not queried")
	}
	if n, err := s.ReconcilePending(context.Background(), channels, 1); err != nil || n != 0 {
		t.Fatal("overlapping tick queued", n, err)
	}
	// Query I/O must not retain database locks: a real callback commits while
	// the gateway response is held, then both paths observe the same receipt.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := s.ConfirmStatus(ctx, "epay", payment.Status{OrderID: order.ID, TransactionID: "tx-" + order.ID, AmountCents: 1000, Currency: "CNY", State: payment.Paid})
	if err != nil {
		t.Fatal("gateway I/O blocked callback", err)
	}
	close(release)
	released = true
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	wallet, err := s.Wallet(context.Background(), "alice")
	ledger, ledgerErr := s.Ledger(context.Background(), "alice")
	if err != nil || ledgerErr != nil || wallet.Balance != 1000 || len(ledger) != 1 || calls.Load() != 1 {
		t.Fatal("duplicate callback/query receipt", wallet, ledger, err, ledgerErr)
	}
}

func TestReconciliationV1UpgradePreservesFinancialFacts(t *testing.T) {
	s := fixture(t)
	channel, _ := reconciliationGateway(t, func(w http.ResponseWriter, r *http.Request) { gatewayStatus(w, r, "10.00", "1") })
	order := scheduledOrder(t, s, channel, "upgrade")
	// Reproduce the released v1 schema by removing the v2/v3 additions in
	// this randomly allocated test database. Business rows remain untouched.
	for _, table := range []string{"commerce_reconciliation", "commerce_plan_states", "commerce_refunds", "commerce_redeem_codes", "commerce_redeem_claims", "commerce_auto_renew", "commerce_referral_codes", "commerce_referral_bindings", "commerce_commissions", "commerce_webhook_subscriptions", "commerce_events", "commerce_event_deliveries", "commerce_referral_policy", "commerce_commission_adjustments", "commerce_purchase_snapshots", "commerce_addon_purchases", "commerce_plan_limits", "commerce_entitlement_limits", "commerce_webhook_settings"} {
		if _, err := s.DB.Exec("DROP TABLE " + table); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DB.Exec("DELETE FROM commerce_schema WHERE version>=2"); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	attempts, next := retryState(t, s, order.ID)
	if attempts != 0 || next != 0 {
		t.Fatal("v1 pending order not backfilled", attempts, next)
	}
	if n, err := s.ReconcilePending(context.Background(), map[string]Channel{"epay": channel}, 1); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal("migration was not idempotent", err)
	}
	wallet, err := s.Wallet(context.Background(), "alice")
	if err != nil || wallet.Balance != 1000 {
		t.Fatal(wallet, err)
	}
}

func TestClosedOrderReconciliationCreditsLatePayment(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	channel, _ := reconciliationGateway(t, func(w http.ResponseWriter, r *http.Request) { gatewayStatus(w, r, "10.00", "1") })
	order := scheduledOrder(t, s, channel, "closed-query")
	if _, err := s.CloseOrder(ctx, "alice", order.ID); err != nil {
		t.Fatal(err)
	}
	now := s.Now().Add(time.Minute)
	s.Now = func() time.Time { return now }
	if n, err := s.ReconcilePending(ctx, map[string]Channel{"epay": channel}, 10); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	orders, err := s.Orders(ctx, "alice")
	wallet, _ := s.Wallet(ctx, "alice")
	if err != nil || orders[0].Status != "paid_late" || wallet.Balance != 1000 {
		t.Fatal(orders, wallet, err)
	}
}
