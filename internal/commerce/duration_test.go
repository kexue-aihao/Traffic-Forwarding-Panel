package commerce

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestPlanDurationEntitlementPaths(t *testing.T) {
	for _, tc := range []struct {
		unit                  string
		count                 int
		start, first, renewal string
	}{
		{"day", 2, "2026-01-31T23:45:00+08:00", "2026-02-02T23:45:00+08:00", "2026-02-04T23:45:00+08:00"},
		{"week", 2, "2026-01-31T23:45:00+08:00", "2026-02-14T23:45:00+08:00", "2026-02-28T23:45:00+08:00"},
		{"month", 1, "2028-01-31T23:45:00+08:00", "2028-02-29T23:45:00+08:00", "2028-03-29T23:45:00+08:00"},
		{"year", 1, "2028-02-29T23:45:00+08:00", "2029-02-28T23:45:00+08:00", "2030-02-28T23:45:00+08:00"},
	} {
		t.Run(tc.unit, func(t *testing.T) {
			s, ctx := fixture(t), context.Background()
			now := parse(tc.start)
			s.Now = func() time.Time { return now }
			p, err := s.CreatePlan(ctx, Plan{Name: tc.unit, Price: 100, Quota: 1000, DurationUnit: tc.unit, DurationValue: tc.count})
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Write(ctx, func(tx *sql.Tx) error { return s.post(ctx, tx, "buyer", 1000, "test", "fund") }); err != nil {
				t.Fatal(err)
			}
			ent, err := s.PurchaseQuote(ctx, "buyer", p.ID, "buy", 0, p.Version)
			if err != nil || !ent.ExpiresAt.Equal(parse(tc.first)) {
				t.Fatal("purchase expiry", ent, err)
			}
			_, code, err := s.CreateRedeemCode(ctx, 0, p.ID, 1, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.RedeemCode(ctx, "recipient", code); err != nil {
				t.Fatal(err)
			}
			redeemed, err := s.Entitlement(ctx, "recipient")
			if err != nil || !redeemed.ExpiresAt.Equal(ent.ExpiresAt) {
				t.Fatal("redeem expiry", redeemed, err)
			}
			if err = s.SetAutoRenew(ctx, "buyer", p.ID, true); err != nil {
				t.Fatal(err)
			}
			now = ent.ExpiresAt.Add(-time.Second)
			if n, err := s.RunAutoRenew(ctx, 10); err != nil || n != 0 {
				t.Fatal("early renewal", n, err)
			}
			now = ent.ExpiresAt
			if n, err := s.RunAutoRenew(ctx, 10); err != nil || n != 1 {
				t.Fatal("renewal", n, err)
			}
			renewed, err := s.Entitlement(ctx, "buyer")
			if err != nil || renewed.Version != 2 || !renewed.ExpiresAt.Equal(parse(tc.renewal)) {
				t.Fatal("renewal expiry", renewed, err)
			}
			if n, err := s.RunAutoRenew(ctx, 10); err != nil || n != 0 {
				t.Fatal("duplicate renewal", n, err)
			}
			wallet, err := s.Wallet(ctx, "buyer")
			if err != nil || wallet.Balance != 800 {
				t.Fatal("deduction", wallet, err)
			}
			for _, paged := range []bool{false, true} {
				var plans []Plan
				if paged {
					plans, _, err = s.PlansPage(ctx, 1, 20)
				} else {
					plans, err = s.Plans(ctx)
				}
				if err != nil || len(plans) != 1 || plans[0].DurationUnit != tc.unit || plans[0].DurationValue != tc.count {
					t.Fatal("read duration", plans, err)
				}
			}
			// Changing the selling period preserves prior entitlements and snapshots.
			p.DurationUnit, p.DurationValue, p.Months = "week", 3, 0
			updated, err := s.UpdatePlan(ctx, p)
			if err != nil || updated.Version != 2 {
				t.Fatal(updated, err)
			}
			if _, err = s.PurchaseQuote(ctx, "buyer", p.ID, "stale", 2, 1); !errors.Is(err, ErrConflict) {
				t.Fatal("stale quote accepted", err)
			}
			replay, err := s.PurchaseQuote(ctx, "buyer", p.ID, "buy", 0, 1)
			if err != nil || !replay.ExpiresAt.Equal(parse(tc.first)) {
				t.Fatal("changed old entitlement", replay, err)
			}
			history, err := s.PurchaseHistory(ctx, "buyer")
			if err != nil || len(history) != 2 {
				t.Fatal(history, err)
			}
			for _, h := range history {
				snapshot := h["plan"].(Plan)
				if snapshot.DurationUnit != tc.unit || snapshot.DurationValue != tc.count {
					t.Fatal("changed snapshot", snapshot)
				}
			}
			fresh, err := s.PurchaseQuote(ctx, "buyer", p.ID, "changed", 2, 2)
			if err != nil || !fresh.ExpiresAt.Equal(now.Add(21*24*time.Hour)) {
				t.Fatal("updated duration", fresh, err)
			}
		})
	}
}

func TestPlanDurationValidation(t *testing.T) {
	s, ctx := fixture(t), context.Background()
	for _, tc := range []struct {
		unit string
		max  int
	}{{"day", 3650}, {"week", 520}, {"month", 120}, {"year", 10}} {
		for _, count := range []int{-1, 0, 1, tc.max, tc.max + 1} {
			_, err := s.CreatePlan(ctx, Plan{Name: "p", Price: 100, Quota: 1, DurationUnit: tc.unit, DurationValue: count})
			wantValid := count >= 1 && count <= tc.max
			if (err == nil) != wantValid {
				t.Fatalf("%s %d: %v", tc.unit, count, err)
			}
		}
	}
	for _, p := range []Plan{
		{DurationUnit: "decade", DurationValue: 1},
		{DurationValue: 1}, {Months: 1, DurationValue: 1},
		{Months: 1, DurationUnit: "week", DurationValue: 1},
		{Months: 1, DurationUnit: "month", DurationValue: 2},
		{Months: 1, DurationUnit: "year", DurationValue: 1},
		{Kind: "addon", DurationUnit: "week", DurationValue: 1},
		{Kind: "addon", DurationValue: 1}, {Kind: "addon", Months: 1},
	} {
		p.Name, p.Price, p.Quota = "invalid", 100, 1
		if _, err := s.CreatePlan(ctx, p); err == nil {
			t.Fatal("invalid duration accepted", p)
		}
	}
	legacy, err := s.CreatePlan(ctx, Plan{Name: "legacy", Price: 100, Quota: 1, Months: 3})
	if err != nil || legacy.DurationUnit != "month" || legacy.DurationValue != 3 {
		t.Fatal(legacy, err)
	}
	legacy.DurationUnit, legacy.DurationValue, legacy.Months = "", 0, 6
	updated, err := s.UpdatePlan(ctx, legacy)
	if err != nil || updated.DurationUnit != "month" || updated.DurationValue != 6 {
		t.Fatal("legacy monthly edit", updated, err)
	}
	yearly, err := s.CreatePlan(ctx, Plan{Name: "annual", Price: 100, Quota: 1, DurationUnit: "year", DurationValue: 1})
	if err != nil || yearly.Months != 12 {
		t.Fatal(yearly, err)
	}
	yearly.DurationUnit, yearly.DurationValue = "", 0
	if _, err := s.UpdatePlan(ctx, yearly); err == nil {
		t.Fatal("legacy client silently changed annual plan")
	}
}

func TestPlanDurationV7MigrationAndLegacySnapshot(t *testing.T) {
	s, ctx := fixture(t), context.Background()
	p, err := s.CreatePlan(ctx, Plan{Name: "old monthly", Price: 100, Quota: 1000, Months: 3})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreatePlan(ctx, Plan{Name: "old addon", Price: 10, Quota: 100, Kind: "addon"}); err != nil {
		t.Fatal(err)
	}
	// Recreate the previous schema, then apply the real upgrade twice.
	for _, q := range []string{
		"ALTER TABLE commerce_plans DROP COLUMN duration_unit",
		"ALTER TABLE commerce_plans DROP COLUMN duration_value",
		"DELETE FROM commerce_schema WHERE version>=7",
		"INSERT INTO commerce_schema(version) VALUES(7)",
	} {
		if _, err := s.DB.ExecContext(ctx, q); err != nil {
			t.Fatal(q, err)
		}
	}
	for i := 0; i < 2; i++ {
		if err = s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Write(ctx, func(tx *sql.Tx) error { return s.post(ctx, tx, "legacy", 1000, "test", "fund") }); err != nil {
		t.Fatal(err)
	}
	ent, err := s.Purchase(ctx, "legacy", p.ID, "old", 0)
	if err != nil || !ent.ExpiresAt.Equal(AddMonths(ent.StartsAt, 3)) {
		t.Fatal("legacy expiry changed", ent, err)
	}
	// An archived purchase from an old release still reports a monthly duration.
	raw, err := json.Marshal(map[string]any{"id": p.ID, "name": p.Name, "price_cents": "100", "quota_bytes": "1000", "months": 3, "kind": "period", "version": 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.ExecContext(ctx, s.q("UPDATE commerce_purchase_snapshots SET payload=? WHERE entitlement_id=?"), string(raw), ent.ID); err != nil {
		t.Fatal(err)
	}
	history, err := s.PurchaseHistory(ctx, "legacy")
	if err != nil || len(history) != 1 || history[0]["plan"].(Plan).DurationUnit != "month" || history[0]["plan"].(Plan).DurationValue != 3 {
		t.Fatal(history, err)
	}
}
