package commerce

import (
	"context"
	"database/sql"
	"testing"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

func TestLeaseRetirementReturnsOnlyConfirmedUnusedBudget(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	if err := s.Write(ctx, func(tx *sql.Tx) error { return s.post(ctx, tx, "retire-user", 10000, "test", "retire-credit") }); err != nil {
		t.Fatal(err)
	}
	p, err := s.CreatePlan(ctx, Plan{Name: "limited", Price: 100, Quota: 1000, Months: 1})
	if err != nil {
		t.Fatal(err)
	}
	ent, err := s.Purchase(ctx, "retire-user", p.ID, "purchase", 0)
	if err != nil {
		t.Fatal(err)
	}
	var lease *contract.Lease
	if err = s.Write(ctx, func(tx *sql.Tx) error {
		var e error
		lease, e = s.AllocateWithMultiplier(ctx, tx, "retire-user", "r", "n", "3/2")
		return e
	}); err != nil {
		t.Fatal(err)
	}
	u := contract.UsageRecord{ID: "retire-usage", NodeID: "n", RuleID: "r", LeaseID: lease.ID, EntitlementID: ent.ID, StartedAt: s.Now(), EndedAt: s.Now(), UploadBytes: 100}
	retire := func(node string, bytes int64) error {
		return s.Write(ctx, func(tx *sql.Tx) error { return s.RetireLease(ctx, tx, node, lease.ID, bytes) })
	}
	if err = retire("n", 100); err == nil {
		t.Fatal("unacknowledged usage allowed retirement")
	}
	if err = s.Write(ctx, func(tx *sql.Tx) error { return s.SettleUsage(ctx, tx, u) }); err != nil {
		t.Fatal(err)
	}
	if err = retire("other-node", 100); err == nil {
		t.Fatal("another node retired lease")
	}
	for range 2 {
		if err = retire("n", 100); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Write(ctx, func(tx *sql.Tx) error { return s.SettleUsage(ctx, tx, u) }); err != nil {
		t.Fatal("replay after retirement", err)
	}
	u.ID = "new-after-retire"
	if err = s.Write(ctx, func(tx *sql.Tx) error { return s.SettleUsage(ctx, tx, u) }); err == nil {
		t.Fatal("retired lease admitted new usage")
	}
	var current bool
	if err = s.Write(ctx, func(tx *sql.Tx) error {
		var e error
		current, e = s.LeaseCurrent(ctx, tx, "retire-user", lease)
		return e
	}); err != nil || current {
		t.Fatal("retired lease remains current", err)
	}
	if err = s.Write(ctx, func(tx *sql.Tx) error { var e error; lease, e = s.Allocate(ctx, tx, "retire-user", "r", "n"); return e }); err != nil {
		t.Fatal(err)
	}
	if lease.Bytes != 850 {
		t.Fatalf("unused reservation returned incorrectly: %d", lease.Bytes)
	}
	u = contract.UsageRecord{ID: "exhausted-usage", NodeID: "n", RuleID: "r", LeaseID: lease.ID, EntitlementID: ent.ID, StartedAt: s.Now(), EndedAt: s.Now(), UploadBytes: lease.Bytes}
	if err = s.Write(ctx, func(tx *sql.Tx) error { return s.SettleUsage(ctx, tx, u) }); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = retire("n", lease.Bytes); err != nil {
			t.Fatal("exhausted lease retirement must succeed without a refund", err)
		}
	}
	var allocated, used int64
	if err = s.DB.QueryRowContext(ctx, s.q("SELECT allocated,used FROM commerce_entitlements WHERE id=?"), ent.ID).Scan(&allocated, &used); err != nil {
		t.Fatal(err)
	}
	if allocated != 1000 || used != 1000 {
		t.Fatalf("exhausted retirement changed consumed quota: allocated=%d used=%d", allocated, used)
	}
	var closed int
	if err = s.DB.QueryRowContext(ctx, s.q("SELECT closed FROM commerce_lease_reservations WHERE lease_id=?"), lease.ID).Scan(&closed); err != nil || closed != 1 {
		t.Fatal("exhausted reservation remained open", closed, err)
	}
}
