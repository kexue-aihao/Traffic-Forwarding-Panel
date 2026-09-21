package commerce

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/payment"
)

func TestResourceLimitsPurchaseRedeemSnapshotAndUpgrade(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	original := contract.ResourceLimits{MaxRules: 2, MaxConnectionsPerNode: 3, MaxIPsPerNode: 1, BytesPerSecondPerNode: 65536}
	p, err := s.CreatePlan(ctx, Plan{Name: "limited", Price: 100, Quota: 64 << 20, Months: 1, Limits: original})
	if err != nil {
		t.Fatal(err)
	}
	// Also exercise the maximum accepted provider transaction length on SQL.
	o, err := s.CreateOrder(ctx, "alice", "epay", "fund", 1000, payment.EPay{Gateway: "https://fixture.invalid", PID: "merchant", Key: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = s.ConfirmPayment(ctx, "epay", o.ID, strings.Repeat("x", 128), 1000); err != nil {
			t.Fatal(err)
		}
	}
	ent, err := s.Purchase(ctx, "alice", p.ID, "buy", 0)
	if err != nil || ent.Limits != original {
		t.Fatal(ent, err)
	}
	p.Limits.MaxRules = 1
	p, err = s.UpdatePlan(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Entitlement(ctx, "alice")
	if err != nil || got.Limits != original {
		t.Fatal("plan edit changed sold policy", got, err)
	}
	if err = s.Write(ctx, func(tx *sql.Tx) error {
		lease, err := s.Allocate(ctx, tx, "alice", "rule", "node")
		if err == nil && lease.Limits != original {
			t.Fatalf("lease lost snapshot: %+v", lease)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, code, err := s.CreateRedeemCode(ctx, 0, p.ID, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.RedeemCode(ctx, "bob", code); err != nil {
		t.Fatal(err)
	}
	got, err = s.Entitlement(ctx, "bob")
	if err != nil || got.Limits != p.Limits {
		t.Fatal("redeem lost limits", got, err)
	}
	history, err := s.PurchaseHistory(ctx, "bob")
	if err != nil || len(history) != 1 || history[0]["plan"].(Plan).Limits != p.Limits {
		t.Fatal(history, err)
	}
	// Simulate the v3 table boundary; an old grant stays unrestricted after upgrade.
	for _, table := range []string{"commerce_plan_limits", "commerce_entitlement_limits", "commerce_webhook_settings"} {
		if _, err = s.DB.Exec("DROP TABLE " + table); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.DB.Exec("DELETE FROM commerce_schema WHERE version>=4"); err != nil {
		t.Fatal(err)
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	got, err = s.Entitlement(ctx, "alice")
	if err != nil || got.Limits != (contract.ResourceLimits{}) || got.ID != ent.ID {
		t.Fatal(got, err)
	}
}
