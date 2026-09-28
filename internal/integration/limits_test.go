package integration

import (
	"context"
	"encoding/json"
	"sort"
	"testing"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/commerce"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/tunnel"
)

func TestPlanRuleLimitIsControlPlaneOnlyAndSnapshotted(t *testing.T) {
	f := newFixture(t, tunnel.Client{})
	f.plan.Limits = contract.PlanLimits{MaxRules: 2}
	f.plan = decode[commerce.Plan](t, f.request("PUT", "/plans/"+f.plan.ID, f.plan, f.admin, 200))
	f.ent = f.purchase("limited", 1)
	tcp, udp := targets(t)
	a := f.rule("tcp", "direct", tcp, nil)
	b := f.rule("udp", "direct", udp, nil)
	f.sync()
	var legacyPayload string
	if err := f.db.DB.QueryRow(f.db.Rebind("SELECT payload FROM cp_rules WHERE id=?"), a.ID).Scan(&legacyPayload); err != nil {
		t.Fatal(err)
	}
	var legacyRule contract.Rule
	if err := json.Unmarshal([]byte(legacyPayload), &legacyRule); err != nil {
		t.Fatal(err)
	}
	legacyRule.Lease.Limits = contract.ResourceLimits{MaxRules: 2, MaxConnectionsPerNode: 2, MaxIPsPerNode: 1, BytesPerSecondPerNode: 65536}
	legacyPayloadBytes, err := json.Marshal(legacyRule)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.DB.Exec(f.db.Rebind("UPDATE cp_rules SET payload=? WHERE id=?"), string(legacyPayloadBytes), a.ID); err != nil {
		t.Fatal(err)
	}
	f.sync()
	for _, rule := range f.store.Config().Rules {
		if rule.Lease.Limits != (contract.ResourceLimits{}) {
			t.Fatal("plan-only rule count leaked into Agent resource controls", rule)
		}
	}
	transfer(t, a, []byte("limited TCP"))
	transfer(t, b, []byte("limited UDP"))
	third := contract.Rule{Name: "third", NodeID: f.store.Identity().NodeID, GroupID: f.group.ID, Network: "tcp", Transport: "direct", Listen: freeAddress(t, "tcp"), Target: tcp, Enabled: true}
	f.request("POST", "/rules", third, f.user, 409)

	// Editing a plan only affects the next purchase; the entitlement is a snapshot.
	f.plan.Limits.MaxRules = 1
	f.plan = decode[commerce.Plan](t, f.request("PUT", "/plans/"+f.plan.ID, f.plan, f.admin, 200))
	f.sync()
	if len(f.store.Config().Rules) != 2 {
		t.Fatal("plan edit retroactively changed the current entitlement")
	}
	f.ent = f.purchase("downgrade", 2)
	f.sync()
	ids := []string{a.ID, b.ID}
	sort.Strings(ids)
	cfg := f.store.Config()
	if len(cfg.Rules) != 1 || cfg.Rules[0].ID != ids[0] {
		t.Fatal("new purchase rule limit was not enforced", cfg)
	}
	if _, err := f.app.Commerce.Entitlement(context.Background(), f.owner.ID); err != nil {
		t.Fatal(err)
	}
}

func TestAdminCanAdjustOneAccountBalanceAndRuleLimit(t *testing.T) {
	f := newFixture(t, tunnel.Client{})
	path := "/users/" + f.owner.ID
	initial := decode[commerce.UserAccount](t, f.request("GET", path+"/account", nil, f.admin, 200))
	if initial.Balance != 9000 || initial.RuleCount != 0 {
		t.Fatal("unexpected initial account", initial)
	}
	// User credentials must not expose administrative account data.
	f.request("GET", path+"/account", nil, f.user, 403)
	for range 2 {
		f.request("POST", path+"/balance-adjustments", map[string]string{
			"amount_cents": "250", "idempotency_key": "account-credit", "reason": "support credit",
		}, f.admin, 200)
	}
	wallet := decode[commerce.Wallet](t, f.request("GET", "/wallet", nil, f.user, 200))
	if wallet.Balance != 9250 {
		t.Fatal("balance adjustment replay posted twice", wallet)
	}
	limit := 1
	f.request("PUT", path+"/rule-limit", map[string]any{"max_rules": limit, "reason": "temporary cap"}, f.admin, 200)
	tcp, _ := targets(t)
	f.rule("tcp", "direct", tcp, nil)
	second := contract.Rule{Name: "second", NodeID: f.store.Identity().NodeID, GroupID: f.group.ID, Network: "tcp", Transport: "direct", Listen: freeAddress(t, "tcp"), Target: tcp, Enabled: true}
	f.request("POST", "/rules", second, f.user, 409)
	f.request("PUT", path+"/rule-limit", map[string]any{"max_rules": nil, "reason": "follow package"}, f.admin, 200)
	f.request("POST", "/rules", second, f.user, 201)
	f.sync()
	account := decode[commerce.UserAccount](t, f.request("GET", path+"/account", nil, f.admin, 200))
	if account.RuleCount != 2 || account.MaxRules != 0 || account.RuleLimitOverride != nil {
		t.Fatal("clearing account cap did not restore package behavior", account)
	}
}
