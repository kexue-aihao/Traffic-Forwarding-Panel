package commerce

import (
	"context"
	"database/sql"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/payment"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func fixture(t *testing.T) *Service {
	t.Helper()
	st, e := storage.Open(context.Background(), "sqlite", filepath.Join(t.TempDir(), "commerce.db"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { st.Close() })
	s := New(st.DB, st.Dialect, func(c context.Context, f func(*sql.Tx) error) error { return st.Write(c, storage.Critical, f) })
	if e = s.Migrate(context.Background()); e != nil {
		t.Fatal(e)
	}
	return s
}
func TestPurchaseReplayRenewAndConcurrentPayment(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	g := payment.EPay{Gateway: "https://example.invalid", PID: "merchant", Key: "secret"}
	o, e := s.CreateOrder(ctx, "alice", "epay", "topup", 10000, g)
	if e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- s.ConfirmPayment(ctx, "epay", o.ID, "tx1", 10000) }()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	w, _ := s.Wallet(ctx, "alice")
	if w.Balance != 10000 {
		t.Fatal(w)
	}
	if e = s.ConfirmPayment(ctx, "epay", o.ID, "tx1", 9999); e == nil {
		t.Fatal("amount tampering accepted")
	}
	p, e := s.CreatePlan(ctx, Plan{Name: "monthly", Price: 1000, Quota: 100000000, Months: 1})
	if e != nil {
		t.Fatal(e)
	}
	now := time.Date(2026, 1, 31, 2, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	a, e := s.Purchase(ctx, "alice", p.ID, "purchase1", 0)
	if e != nil {
		t.Fatal(e)
	}
	again, e := s.Purchase(ctx, "alice", p.ID, "purchase1", 0)
	if e != nil || again.ID != a.ID {
		t.Fatal(again, e)
	}
	if a.ExpiresAt.Day() != 28 {
		t.Fatal(a.ExpiresAt)
	}
	now = now.Add(24 * time.Hour)
	b, e := s.Purchase(ctx, "alice", p.ID, "purchase2", 1)
	if e != nil {
		t.Fatal(e)
	}
	if !b.StartsAt.Equal(now) || b.ExpiresAt.Month() != time.March || b.Used != 0 {
		t.Fatal(b)
	}
	if _, e = s.Purchase(ctx, "alice", p.ID, "stale", 1); e == nil {
		t.Fatal("stale accepted")
	}
	w, _ = s.Wallet(ctx, "alice")
	if w.Balance != 8000 {
		t.Fatal(w)
	}
}
func TestLeaseOldCycleAndUsageReplay(t *testing.T) {
	s := fixture(t)
	c := context.Background()
	s.Write(c, func(tx *sql.Tx) error { return s.post(c, tx, "u", 10000, "test", "test") })
	p, _ := s.CreatePlan(c, Plan{Name: "p", Price: 100, Quota: 1000, Months: 1})
	a, _ := s.Purchase(c, "u", p.ID, "p1", 0)
	var lease *contract.Lease
	e := s.Write(c, func(tx *sql.Tx) error {
		var e error
		lease, e = s.AllocateWithMultiplier(c, tx, "u", "r", "n", "2")
		return e
	})
	if e != nil {
		t.Fatal(e)
	}
	if lease.Bytes != 500 {
		t.Fatal(lease)
	}
	if e = s.Write(c, func(tx *sql.Tx) error { _, e := s.Allocate(c, tx, "u", "r2", "n2"); return e }); e == nil {
		t.Fatal("double allocated quota")
	}
	u := contract.UsageRecord{ID: "usage1", NodeID: "n", RuleID: "r", LeaseID: lease.ID, EntitlementID: a.ID, StartedAt: s.Now(), EndedAt: s.Now(), UploadBytes: 10, DownloadBytes: 20}
	b, e := s.Purchase(c, "u", p.ID, "p2", 1)
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 2; i++ {
		if e = s.Write(c, func(tx *sql.Tx) error { return s.SettleUsage(c, tx, u) }); e != nil {
			t.Fatal(e)
		}
	}
	var oldUsed int64
	s.DB.QueryRow("SELECT used FROM commerce_entitlements WHERE id=?", a.ID).Scan(&oldUsed)
	current, _ := s.Entitlement(c, "u")
	if oldUsed != 60 || current.ID != b.ID || current.Used != 0 {
		t.Fatal(oldUsed, current)
	}
	u.UploadBytes++
	if e = s.Write(c, func(tx *sql.Tx) error { return s.SettleUsage(c, tx, u) }); e == nil {
		t.Fatal("changed replay accepted")
	}
}
