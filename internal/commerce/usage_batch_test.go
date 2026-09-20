package commerce

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"testing"
	"time"
)

func TestBulkUsageRoundingReplayAndAtomicFailure(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	if e := s.Write(ctx, func(tx *sql.Tx) error { return s.post(ctx, tx, "batch-user", 10000, "seed", "seed") }); e != nil {
		t.Fatal(e)
	}
	plan, e := s.CreatePlan(ctx, Plan{Name: "batch", Price: 100, Quota: 10000, Months: 1})
	if e != nil {
		t.Fatal(e)
	}
	ent, e := s.Purchase(ctx, "batch-user", plan.ID, "purchase", 0)
	if e != nil {
		t.Fatal(e)
	}
	var lease *contract.Lease
	if e = s.Write(ctx, func(tx *sql.Tx) error {
		var err error
		lease, err = s.AllocateWithMultiplier(ctx, tx, "batch-user", "rule", "node", "1/3")
		return err
	}); e != nil {
		t.Fatal(e)
	}
	now := time.Now().UTC()
	mk := func(id string, amount int64) contract.UsageRecord {
		return contract.UsageRecord{ID: id, NodeID: "node", RuleID: "rule", LeaseID: lease.ID, EntitlementID: ent.ID, StartedAt: now.Add(-time.Second), EndedAt: now, UploadBytes: amount}
	}
	a, b, c := mk("a", 1), mk("b", 1), mk("c", 1)
	apply := func(rs ...contract.UsageRecord) error {
		return s.Write(ctx, func(tx *sql.Tx) error { return s.SettleUsageBatch(ctx, tx, rs) })
	}
	if e = apply(a, a, b, c); e != nil {
		t.Fatal(e)
	}
	if e = apply(c, b, a); e != nil {
		t.Fatal(e)
	}
	var used int64
	var facts int
	check := func(wantUsed int64, wantFacts int) {
		t.Helper()
		if e = s.DB.QueryRowContext(ctx, s.q(`SELECT used FROM commerce_entitlements WHERE id=?`), ent.ID).Scan(&used); e != nil {
			t.Fatal(e)
		}
		if e = s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM commerce_usage`).Scan(&facts); e != nil {
			t.Fatal(e)
		}
		if used != wantUsed || facts != wantFacts {
			t.Fatalf("used %d facts %d wanted %d/%d", used, facts, wantUsed, wantFacts)
		}
	}
	check(1, 3)
	bad := mk("bad", 1)
	bad.NodeID = "other-tenant"
	if e = apply(mk("valid-before-invalid", 1), bad); e == nil {
		t.Fatal("foreign lease accepted")
	}
	check(1, 3)
	bad = a
	bad.UploadBytes = 2
	if e = apply(mk("new-before-changed-duplicate", 1), bad); e == nil {
		t.Fatal("changed persisted ID accepted")
	}
	check(1, 3)
	dup := mk("dup", 1)
	changed := dup
	changed.UploadBytes = 2
	if e = apply(dup, changed); e == nil {
		t.Fatal("changed in-batch ID accepted")
	}
	check(1, 3)
	records := make([]contract.UsageRecord, 250)
	for i := range records {
		records[i] = mk(fmt.Sprintf("bulk-%03d", i), 1)
	}
	if e = apply(records...); e != nil {
		t.Fatal(e)
	}
	check(85, 253)
	if e = apply(records...); e != nil {
		t.Fatal(e)
	}
	check(85, 253)
	// Rounding is per cumulative lease total: 253 bytes at 1/3 costs ceil=85.
	var sum int64
	if e = s.DB.QueryRow(`SELECT SUM(charged) FROM commerce_usage`).Scan(&sum); e != nil || sum != 85 {
		t.Fatal(sum, e)
	}
	invalid := mk("expired-event", 1)
	invalid.EndedAt = lease.ExpiresAt.Add(time.Second)
	if e = apply(mk("rollback-good", 1), invalid); e == nil {
		t.Fatal("expired usage accepted")
	}
	check(85, 253)
}

func TestBulkUsageConcurrentReplay(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	if e := s.Write(ctx, func(tx *sql.Tx) error { return s.post(ctx, tx, "concurrent-user", 10000, "seed", "seed") }); e != nil {
		t.Fatal(e)
	}
	p, e := s.CreatePlan(ctx, Plan{Name: "c", Price: 100, Quota: 10000, Months: 1})
	if e != nil {
		t.Fatal(e)
	}
	ent, e := s.Purchase(ctx, "concurrent-user", p.ID, "purchase", 0)
	if e != nil {
		t.Fatal(e)
	}
	var lease *contract.Lease
	if e = s.Write(ctx, func(tx *sql.Tx) error {
		var err error
		lease, err = s.Allocate(ctx, tx, "concurrent-user", "r", "n")
		return err
	}); e != nil {
		t.Fatal(e)
	}
	now := time.Now()
	records := []contract.UsageRecord{}
	for i := 0; i < 20; i++ {
		records = append(records, contract.UsageRecord{ID: fmt.Sprintf("parallel-%d", i), NodeID: "n", RuleID: "r", LeaseID: lease.ID, EntitlementID: ent.ID, StartedAt: now.Add(-time.Second), EndedAt: now, UploadBytes: 10})
	}
	done := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			var err error
			for attempt := 0; attempt < 10; attempt++ {
				err = s.Write(ctx, func(tx *sql.Tx) error { return s.SettleUsageBatch(ctx, tx, records) })
				if err == nil {
					break
				}
				time.Sleep(time.Duration(attempt+1) * time.Millisecond)
			}
			done <- err
		}()
	}
	for i := 0; i < 8; i++ {
		if e = <-done; e != nil {
			t.Fatal(e)
		}
	}
	var used, facts int64
	if e = s.DB.QueryRowContext(ctx, s.q(`SELECT used FROM commerce_entitlements WHERE id=?`), ent.ID).Scan(&used); e != nil {
		t.Fatal(e)
	}
	s.DB.QueryRow(`SELECT COUNT(*) FROM commerce_usage`).Scan(&facts)
	if used != 200 || facts != 20 {
		t.Fatal("concurrent replay double charged", used, facts)
	}
}
