package commerce

import (
	"context"
	"database/sql"
	"sync"
	"testing"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/payment"
)

func TestPurchaseRefundFundingCommissionAndExternalRefund(t *testing.T) {
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
	gateway := payment.EPay{Gateway: "https://example.invalid", PID: "m", Key: "k"}
	orders := []Order{}
	for i, amount := range []int64{600, 400} {
		key := []string{"one", "two"}[i]
		o, e := s.CreateOrder(ctx, "buyer", "epay", key, amount, gateway)
		if e != nil {
			t.Fatal(e)
		}
		if e = s.ConfirmPayment(ctx, "epay", o.ID, key, amount); e != nil {
			t.Fatal(e)
		}
		orders = append(orders, o)
	}
	plan, err := s.CreatePlan(ctx, Plan{Name: "monthly", Price: 1000, Quota: 1000, Months: 1})
	if err != nil {
		t.Fatal(err)
	}
	ent, err := s.Purchase(ctx, "buyer", plan.ID, "buy", 0)
	if err != nil {
		t.Fatal(err)
	}
	commissions, err := s.Commissions(ctx, "inviter")
	if err != nil || len(commissions) != 1 {
		t.Fatal(commissions, err)
	}
	if err = s.ResolveCommission(ctx, "admin", commissions[0].ID, "settle", "reviewed"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RequestRefund(ctx, "admin", orders[0].ID, "early", "spent", 600); err == nil {
		t.Fatal("spent recharge refunded")
	}
	first, err := s.RefundPurchase(ctx, "admin", ent.ID, "part1", "customer", 333)
	if err != nil || first.QuotaRemoved != 333 {
		t.Fatal(first, err)
	}
	again, err := s.RefundPurchase(ctx, "admin", ent.ID, "part1", "customer", 333)
	if err != nil || again.ID != first.ID {
		t.Fatal("replay", again, err)
	}
	inviter, err := s.Wallet(ctx, "inviter")
	if err != nil || inviter.Balance != 67 {
		t.Fatal(inviter, err)
	}
	second, err := s.RefundPurchase(ctx, "admin", ent.ID, "part2", "customer", 667)
	if err != nil || second.QuotaRemoved != 667 {
		t.Fatal(second, err)
	}
	funds, err := s.PurchaseFunding(ctx, "buyer", ent.ID, false)
	if err != nil || len(funds) != 2 {
		t.Fatal(funds, err)
	}
	for _, o := range orders {
		refund, e := s.RequestRefund(ctx, "admin", o.ID, "external", "customer", o.Amount)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = s.ResolveRefund(ctx, "admin", refund.ID, "provider evidence", true); e != nil {
			t.Fatal(e)
		}
	}
	for _, user := range []string{"buyer", "inviter"} {
		w, e := s.Wallet(ctx, user)
		if e != nil || w.Balance != 0 {
			t.Fatal(user, w, e)
		}
	}
	current, err := s.Entitlement(ctx, "buyer")
	if err != nil || current.Quota != 0 {
		t.Fatal(current, err)
	}
	if _, err = s.PurchaseFunding(ctx, "intruder", ent.ID, false); err == nil {
		t.Fatal("funding privacy bypass")
	}
}
func TestPurchaseRefundRejectsReservedQuotaAndConcurrentOverRefund(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	if err := s.Write(ctx, func(tx *sql.Tx) error { return s.post(ctx, tx, "user", 1000, "adjustment", "credit") }); err != nil {
		t.Fatal(err)
	}
	p, err := s.CreatePlan(ctx, Plan{Name: "plan", Price: 1000, Quota: 1000, Months: 1})
	if err != nil {
		t.Fatal(err)
	}
	ent, err := s.Purchase(ctx, "user", p.ID, "purchase", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Write(ctx, func(tx *sql.Tx) error { _, e := s.Allocate(ctx, tx, "user", "rule", "node"); return e }); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RefundPurchase(ctx, "admin", ent.ID, "blocked", "reserved", 1); err == nil {
		t.Fatal("reserved quota refunded")
	}
	// Release the test allocation through the real retirement path.
	var lease string
	if err = s.DB.QueryRowContext(ctx, "SELECT id FROM commerce_leases").Scan(&lease); err != nil {
		t.Fatal(err)
	}
	if err = s.Write(ctx, func(tx *sql.Tx) error { return s.RetireLease(ctx, tx, "node", lease, 0) }); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, key := range []string{"a", "b"} {
		wg.Go(func() { _, e := s.RefundPurchase(ctx, "admin", ent.ID, key, "concurrent", 800); results <- e })
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatal("over-refund race", success)
	}
	w, err := s.Wallet(ctx, "user")
	if err != nil || w.Balance != 800 {
		t.Fatal(w, err)
	}
}
