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
}
