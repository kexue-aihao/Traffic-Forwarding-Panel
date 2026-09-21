package commerce

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/payment"
)

func TestLatePaymentCloseRefundAndReplay(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	o, err := s.CreateOrder(ctx, "alice", "epay", "late", 1000, payment.EPay{Gateway: "https://example.invalid", PID: "m", Key: "k"})
	if err != nil {
		t.Fatal(err)
	}
	closed, err := s.CloseOrder(ctx, "alice", o.ID)
	if err != nil || closed.Status != "closed" {
		t.Fatal(closed, err)
	}
	if err = s.ConfirmPayment(ctx, "epay", o.ID, "late-tx", 1000); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.OrdersPage(ctx, "alice", 1, 10)
	if err != nil || len(got) != 1 || got[0].Status != "paid_late" {
		t.Fatal(got, err)
	}
	if err = s.ConfirmPayment(ctx, "epay", o.ID, "late-tx", 1000); err != nil {
		t.Fatal("replayed receipt should be idempotent", err)
	}
	refund, err := s.RequestRefund(ctx, "admin", o.ID, "refund1", "customer request", 1000)
	if err != nil || refund.Status != "pending_external" {
		t.Fatal(refund, err)
	}
	replay, err := s.RequestRefund(ctx, "admin", o.ID, "refund1", "customer request", 1000)
	if err != nil || replay.ID != refund.ID {
		t.Fatal(replay, err)
	}
	if _, err = s.ResolveRefund(ctx, "admin", refund.ID, "bank transfer reference R1", true); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ResolveRefund(ctx, "admin", refund.ID, "bank transfer reference R1", true); err != nil {
		t.Fatal(err)
	}
	if err = s.ConfirmPayment(ctx, "epay", o.ID, "late-tx", 1000); err != nil {
		t.Fatal("old callback after refund", err)
	}

	w, err := s.Wallet(ctx, "alice")
	if err != nil || w.Balance != 0 {
		t.Fatal(w, err)
	}
	ledger, err := s.Ledger(ctx, "alice")
	if err != nil || len(ledger) != 2 {
		t.Fatal(ledger, err)
	}
}

func TestRedeemCodeConcurrentLimit(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	_, plain, err := s.CreateRedeemCode(ctx, 100, "", 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, user := range []string{"a", "b"} {
		wg.Add(1)
		go func(user string) {
			defer wg.Done()
			_, e := s.RedeemCode(ctx, user, plain)
			results <- e
		}(user)
	}
	wg.Wait()
	close(results)
	ok := 0
	for err := range results {
		if err == nil {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("expected one successful redemption, got %d", ok)
	}
}

func TestReferralCommissionIsIdempotent(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	_, code, err := s.CreateReferralCode(ctx, "inviter")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.BindReferral(ctx, "invitee", code); err != nil {
		t.Fatal(err)
	}
	if err = s.BindReferral(ctx, "invitee", code); err == nil {
		t.Fatal("referral rebinding accepted")
	}
	if err = s.SetCommissionRate(ctx, 500); err != nil {
		t.Fatal(err)
	}
	p, err := s.CreatePlan(ctx, Plan{Name: "ref", Price: 1000, Quota: 1000, Months: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Write(ctx, func(tx *sql.Tx) error { return s.post(ctx, tx, "invitee", 1000, "test", "seed-invitee") }); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Purchase(ctx, "invitee", p.ID, "purchase", 0); err != nil {
		t.Fatal(err)
	}
	commissions, err := s.Commissions(ctx, "inviter")
	if err != nil || len(commissions) != 1 || commissions[0].Amount != 50 {
		t.Fatal(commissions, err)
	}
	if err = s.Write(ctx, func(tx *sql.Tx) error { return s.recordCommission(ctx, tx, "invitee", commissions[0].SourceID, 1000) }); err == nil {
		t.Fatal("duplicate commission accepted")
	}
}

func TestAutoRenewExpiryRetryConcurrencyAndDisable(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	now := time.Date(2026, 1, 31, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	p, err := s.CreatePlan(ctx, Plan{Name: "renew", Price: 1000, Quota: 1000, Months: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Write(ctx, func(tx *sql.Tx) error { return s.post(ctx, tx, "alice", 1000, "test", "seed") }); err != nil {
		t.Fatal(err)
	}
	ent, err := s.Purchase(ctx, "alice", p.ID, "first", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetAutoRenew(ctx, "alice", p.ID, true); err != nil {
		t.Fatal(err)
	}
	now = ent.ExpiresAt.Add(-time.Hour)
	if n, err := s.RunAutoRenew(ctx, 10); err != nil || n != 0 {
		t.Fatal("early renewal", n, err)
	}
	now = ent.ExpiresAt
	if n, err := s.RunAutoRenew(ctx, 10); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	status, err := s.AutoRenewStatus(ctx, "alice")
	if err != nil || status.LastError != "insufficient_funds" {
		t.Fatal(status, err)
	}
	if err = s.Write(ctx, func(tx *sql.Tx) error { return s.post(ctx, tx, "alice", 3000, "test", "fund-retry") }); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, e := s.RunAutoRenew(ctx, 10); errs <- e }()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	renewed, err := s.Entitlement(ctx, "alice")
	wallet, _ := s.Wallet(ctx, "alice")
	if err != nil || renewed.Version != 2 || wallet.Balance != 2000 || !renewed.StartsAt.Equal(now) {
		t.Fatal(renewed, wallet, err)
	}
	if err = s.SetAutoRenew(ctx, "alice", "", false); err != nil {
		t.Fatal(err)
	}
	now = renewed.ExpiresAt.Add(time.Hour)
	if n, e := s.RunAutoRenew(ctx, 10); e != nil || n != 0 {
		t.Fatal(n, e)
	}
	if err = s.SetAutoRenew(ctx, "alice", "missing", true); err == nil {
		t.Fatal("missing plan accepted")
	}
	if err = s.SetPlanActive(ctx, p.ID, false); err != nil {
		t.Fatal(err)
	}
	if err = s.SetAutoRenew(ctx, "alice", p.ID, true); err == nil {
		t.Fatal("inactive plan accepted")
	}
}

func TestWebhookOutboxRecordsSignedEvent(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	hook, secret, err := s.CreateWebhook(ctx, "alice", "https://hooks.example.test/events", []string{"wallet.recharge"})
	if err != nil || secret == "" || hook.ID == "" {
		t.Fatal(hook, secret, err)
	}
	if err = s.Write(ctx, func(tx *sql.Tx) error { return s.post(ctx, tx, "alice", 100, "recharge", "payment:fixture:event") }); err != nil {
		t.Fatal(err)
	}
	events, err := s.Events(ctx, "alice", 10)
	if err != nil || len(events) != 1 || events[0].Kind != "wallet.recharge" {
		t.Fatal(events, err)
	}
	var pending int
	if err = s.DB.QueryRow(s.q("SELECT COUNT(*) FROM commerce_event_deliveries WHERE event_id=? AND subscription_id=? AND delivered_at IS NULL"), events[0].ID, hook.ID).Scan(&pending); err != nil || pending != 1 {
		t.Fatal(pending, err)
	}
}

func TestRefundPartialCancellationAndInsufficientFunds(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	o, err := s.CreateOrder(ctx, "a", "epay", "fund", 1000, payment.EPay{Gateway: "https://example.invalid", PID: "m", Key: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ConfirmPayment(ctx, "epay", o.ID, "partial-tx", 1000); err != nil {
		t.Fatal(err)
	}
	r, err := s.RequestRefund(ctx, "admin", o.ID, "part", "part", 400)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.RequestRefund(ctx, "admin", o.ID, "over", "over", 700); err == nil {
		t.Fatal("over-refund")
	}
	if _, err = s.ResolveRefund(ctx, "admin", r.ID, "canceled externally", false); err != nil {
		t.Fatal(err)
	}
	w, _ := s.Wallet(ctx, "a")
	if w.Balance != 1000 {
		t.Fatal(w)
	}
	r, err = s.RequestRefund(ctx, "admin", o.ID, "retry", "part", 400)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ResolveRefund(ctx, "admin", r.ID, "reference P1", true); err != nil {
		t.Fatal(err)
	}
	orders, _ := s.Orders(ctx, "a")
	if orders[0].Status != "partially_refunded" {
		t.Fatal(orders)
	}
	if err = s.Write(ctx, func(tx *sql.Tx) error { return s.post(ctx, tx, "a", -600, "purchase", "spend") }); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RequestRefund(ctx, "admin", o.ID, "no-funds", "request", 600); !errors.Is(err, ErrFunds) {
		t.Fatal(err)
	}
}
func TestRedeemReplayAndConcurrentEntitlementVersions(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	p, err := s.CreatePlan(ctx, Plan{Name: "grant", Price: 1, Months: 1, Quota: 100})
	if err != nil {
		t.Fatal(err)
	}
	_, code, err := s.CreateRedeemCode(ctx, 1, p.ID, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.RedeemCode(ctx, "alice", code)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.RedeemCode(ctx, "alice", code)
	if err != nil || first["entitlement_id"] != again["entitlement_id"] || again["amount_cents"] != "1" {
		t.Fatal(first, again, err)
	}
	_, code2, err := s.CreateRedeemCode(ctx, 0, p.ID, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); _, e := s.RedeemCode(ctx, "alice", code2); errs <- e }()
	go func() { defer wg.Done(); _, e := s.Purchase(ctx, "alice", p.ID, "parallel", 1); errs <- e }()
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil && !errors.Is(e, ErrConflict) {
			t.Fatal(e)
		}
	}
	e, err := s.Entitlement(ctx, "alice")
	if err != nil || e.Version < 2 {
		t.Fatal(e, err)
	}
}
func TestReferralDefaultOffCycleAndPending(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	_, a, _ := s.CreateReferralCode(ctx, "a")
	_, b, _ := s.CreateReferralCode(ctx, "b")
	if err := s.BindReferral(ctx, "b", a); err != nil {
		t.Fatal(err)
	}
	if err := s.BindReferral(ctx, "a", b); err == nil {
		t.Fatal("cycle")
	}
	if _, _, err := s.CreateReferralCode(ctx, "a"); err == nil {
		t.Fatal("duplicate code owner")
	}
	p, _ := s.CreatePlan(ctx, Plan{Name: "p", Price: 100, Quota: 1, Months: 1})
	if err := s.Write(ctx, func(tx *sql.Tx) error { return s.post(ctx, tx, "b", 200, "test", "seed") }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Purchase(ctx, "b", p.ID, "one", 0); err != nil {
		t.Fatal(err)
	}
	c, _ := s.Commissions(ctx, "a")
	if len(c) != 0 {
		t.Fatal(c)
	}
	if err := s.SetCommissionRate(ctx, 500); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Purchase(ctx, "b", p.ID, "two", 1); err != nil {
		t.Fatal(err)
	}
	c, _ = s.Commissions(ctx, "a")
	w, _ := s.Wallet(ctx, "a")
	if len(c) != 1 || c[0].Status != "pending" || w.Balance != 0 {
		t.Fatal(c, w)
	}
}
func TestWebhookDeliverySignaturePrecisionRetryAndTerminal(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	s.Now = func() time.Time { return now }
	var received, signature, eventID string
	status := http.StatusServiceUnavailable
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		received = string(raw)
		signature = r.Header.Get("X-TFP-Signature")
		eventID = r.Header.Get("X-TFP-Event-ID")
		w.WriteHeader(status)
	}))
	defer server.Close()
	hook, secret, err := s.CreateWebhook(ctx, "alice", "https://hooks.example.test/events", []string{"wallet.recharge"})
	if err != nil {
		t.Fatal(err)
	}
	// Test-only trusted TLS gateway. Production uses the restricted dialer.
	if _, err = s.DB.Exec(s.q("UPDATE commerce_webhook_subscriptions SET url=? WHERE id=?"), server.URL, hook.ID); err != nil {
		t.Fatal(err)
	}
	s.webhookClient = server.Client()
	if err = s.Write(ctx, func(tx *sql.Tx) error { return s.post(ctx, tx, "alice", 9007199254740993, "recharge", "precise") }); err != nil {
		t.Fatal(err)
	}
	if n, err := s.RunWebhookDelivery(ctx, 10); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if !strings.Contains(received, `"amount_cents":"9007199254740993"`) {
		t.Fatal(received)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(received))
	if signature != "sha256="+hex.EncodeToString(mac.Sum(nil)) || eventID == "" {
		t.Fatal("invalid signature or event id")
	}
	if n, err := s.RunWebhookDelivery(ctx, 10); err != nil || n != 0 {
		t.Fatal("backoff ignored", n, err)
	}
	for i := 1; i < 12; i++ {
		now = now.Add(time.Hour)
		if n, e := s.RunWebhookDelivery(ctx, 10); e != nil || n != 1 {
			t.Fatal(i, n, e)
		}
	}
	now = now.Add(time.Hour)
	if n, e := s.RunWebhookDelivery(ctx, 10); e != nil || n != 0 {
		t.Fatal("terminal retry ignored", n, e)
	}
	deliveries, err := s.WebhookDeliveries(ctx, "alice")
	if err != nil || deliveries[0]["status"] != "failed" {
		t.Fatal(deliveries, err)
	}
}
func TestWebhookPrivateDestinationsRejected(t *testing.T) {
	for _, addr := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "100.64.0.1", "::1", "::ffff:127.0.0.1", "fc00::1", "64:ff9b::a00:1"} {
		if publicWebhookIP(netip.MustParseAddr(addr)) {
			t.Fatal(addr)
		}
	}
	if !publicWebhookIP(netip.MustParseAddr("1.1.1.1")) {
		t.Fatal("public address blocked")
	}
	client := newWebhookClient()
	if _, err := client.Post("https://localhost:443/", "application/json", strings.NewReader("{}")); err == nil {
		t.Fatal("private DNS allowed")
	}
}

func TestCommissionSettlementReversalAndReplay(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	_, code, err := s.CreateReferralCode(ctx, "inviter")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.BindReferral(ctx, "buyer", code); err != nil {
		t.Fatal(err)
	}
	if err = s.SetCommissionRate(ctx, 1000); err != nil {
		t.Fatal(err)
	}
	p, err := s.CreatePlan(ctx, Plan{Name: "commission", Price: 1000, Quota: 100, Months: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Write(ctx, func(tx *sql.Tx) error { return s.post(ctx, tx, "buyer", 1000, "test", "seed") }); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Purchase(ctx, "buyer", p.ID, "buy", 0); err != nil {
		t.Fatal(err)
	}
	commissions, err := s.Commissions(ctx, "inviter")
	if err != nil || len(commissions) != 1 {
		t.Fatal(commissions, err)
	}
	for i := 0; i < 2; i++ {
		if err = s.ResolveCommission(ctx, "admin", commissions[0].ID, "settle", "reviewed"); err != nil {
			t.Fatal(err)
		}
	}
	wallet, _ := s.Wallet(ctx, "inviter")
	if wallet.Balance != 100 {
		t.Fatal(wallet)
	}
	for i := 0; i < 2; i++ {
		if err = s.ResolveCommission(ctx, "admin", commissions[0].ID, "reverse", "purchase reversed"); err != nil {
			t.Fatal(err)
		}
	}
	wallet, _ = s.Wallet(ctx, "inviter")
	if wallet.Balance != 0 {
		t.Fatal(wallet)
	}
	if err = s.ResolveCommission(ctx, "admin", commissions[0].ID, "settle", "retry"); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
}

func TestWebhookRestartSuccessAndRetentionPreserveLedger(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	s.Now = func() time.Time { return now }
	calls := 0
	response := 503
	var receivedID string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if receivedID != "" && receivedID != r.Header.Get("X-TFP-Event-ID") {
			t.Error("event id changed on retry")
		}
		receivedID = r.Header.Get("X-TFP-Event-ID")
		w.WriteHeader(response)
	}))
	defer server.Close()
	hook, _, err := s.CreateWebhook(ctx, "a", "https://hooks.example.test/events", []string{"wallet.recharge"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(s.q("UPDATE commerce_webhook_subscriptions SET url=? WHERE id=?"), server.URL, hook.ID); err != nil {
		t.Fatal(err)
	}
	s.webhookClient = server.Client()
	if err = s.Write(ctx, func(tx *sql.Tx) error { return s.post(ctx, tx, "a", 100, "recharge", "restart-event") }); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RunWebhookDelivery(ctx, 10); err != nil {
		t.Fatal(err)
	}
	restarted := New(s.DB, s.Dialect, s.Write)
	restarted.Now = func() time.Time { return now }
	restarted.webhookClient = server.Client()
	if n, e := restarted.RunWebhookDelivery(ctx, 10); e != nil || n != 0 {
		t.Fatal(n, e)
	}
	now = now.Add(time.Minute)
	response = 200
	if n, e := restarted.RunWebhookDelivery(ctx, 10); e != nil || n != 1 {
		t.Fatal(n, e)
	}
	if n, e := restarted.RunWebhookDelivery(ctx, 10); e != nil || n != 0 || calls != 2 {
		t.Fatal(n, e, calls)
	}
	records, err := restarted.WebhookDeliveries(ctx, "a")
	if err != nil || records[0]["status"] != "delivered" {
		t.Fatal(records, err)
	}
	now = now.Add(31 * 24 * time.Hour)
	if err = restarted.PruneEvents(ctx); err != nil {
		t.Fatal(err)
	}
	events, _ := s.Events(ctx, "a", 100)
	ledger, _ := s.Ledger(ctx, "a")
	wallet, _ := s.Wallet(ctx, "a")
	if len(events) != 0 || len(ledger) != 1 || wallet.Balance != 100 {
		t.Fatal(events, ledger, wallet)
	}
}
