package commerce

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestPlanRevisionSnapshotAndAddonReplay(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	s.Now = func() time.Time { return now }
	period, err := s.CreatePlan(ctx, Plan{Name: "original", Price: 100, Quota: 1000, Months: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Write(ctx, func(tx *sql.Tx) error { return s.post(ctx, tx, "a", 1000, "test", "fund") }); err != nil {
		t.Fatal(err)
	}
	ent, err := s.PurchaseQuote(ctx, "a", period.ID, "initial", 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	period.Name = "updated"
	period.Price = 200
	updated, err := s.UpdatePlan(ctx, period)
	if err != nil || updated.Version != 2 {
		t.Fatal(updated, err)
	}
	if _, err = s.UpdatePlan(ctx, period); !errors.Is(err, ErrConflict) {
		t.Fatal("stale edit", err)
	}
	if _, err = s.PurchaseQuote(ctx, "a", period.ID, "stale-quote", 1, 1); !errors.Is(err, ErrConflict) {
		t.Fatal("stale quote", err)
	}
	history, err := s.PurchaseHistory(ctx, "a")
	if err != nil || len(history) != 1 || history[0]["plan"].(Plan).Name != "original" || history[0]["plan"].(Plan).Price != 100 {
		t.Fatal(history, err)
	}
	replay, err := s.PurchaseQuote(ctx, "a", period.ID, "initial", 0, 1)
	if err != nil || replay.ID != ent.ID {
		t.Fatal(replay, err)
	}
	addon, err := s.CreatePlan(ctx, Plan{Name: "extra", Price: 50, Quota: 100, Kind: "addon"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetAutoRenew(ctx, "a", addon.ID, true); err == nil {
		t.Fatal("addon as renewal")
	}
	if _, _, err = s.CreateRedeemCode(ctx, 0, addon.ID, 1, nil); err == nil {
		t.Fatal("addon as period redemption")
	}
	if _, err = s.Purchase(ctx, "a", addon.ID, "wrong-path", 1); err == nil {
		t.Fatal("addon created new period")
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, e := s.PurchaseAddon(ctx, "a", addon.ID, "same-addon", 1, 1); errs <- e }()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	got, err := s.Entitlement(ctx, "a")
	w, _ := s.Wallet(ctx, "a")
	if err != nil || got.ID != ent.ID || !got.ExpiresAt.Equal(ent.ExpiresAt) || got.Quota != 1100 || w.Balance != 850 {
		t.Fatal(got, w, err)
	}
	// New period replaces quota while the add-on replay still returns its original result.
	now = now.Add(time.Minute)
	renewed, err := s.PurchaseQuote(ctx, "a", period.ID, "renew", 1, 2)
	if err != nil || renewed.Quota != 1000 || renewed.Version != 2 {
		t.Fatal(renewed, err)
	}
	replay, err = s.PurchaseAddon(ctx, "a", addon.ID, "same-addon", 1, 1)
	if err != nil || replay.ID != ent.ID || replay.Quota != 1100 {
		t.Fatal(replay, err)
	}
	now = renewed.ExpiresAt
	if _, err = s.PurchaseAddon(ctx, "a", addon.ID, "expired", 2, 1); err == nil {
		t.Fatal("expired add-on accepted")
	}
}
