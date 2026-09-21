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

func TestLimitsReachAgentAndDowngrade(t *testing.T) {
	f := newFixture(t, tunnel.Client{})
	f.plan.Limits = contract.ResourceLimits{MaxRules: 2, MaxConnectionsPerNode: 2, MaxIPsPerNode: 1, BytesPerSecondPerNode: 65536}
	f.plan = decode[commerce.Plan](t, f.request("PUT", "/plans/"+f.plan.ID, f.plan, f.admin, 200))
	f.ent = f.purchase("limited", 1)
	tcp, udp := targets(t)
	a := f.rule("tcp", "direct", tcp, nil)
	b := f.rule("udp", "direct", udp, nil)
	f.sync()
	for _, r := range f.store.Config().Rules {
		if r.Lease.Limits != f.plan.Limits {
			t.Fatal("missing Agent policy", r)
		}
	}
	transfer(t, a, []byte("limited TCP"))
	transfer(t, b, []byte("limited UDP"))
	// Old Agents omit the capability: publishing an unlimited interpretation
	// would be unsafe. Updating capabilities in a valid ACK restores eligibility.
	var raw string
	if err := f.db.DB.QueryRow(f.db.Rebind("SELECT payload FROM cp_nodes WHERE id=?"), f.store.Identity().NodeID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var node contract.Node
	if err := json.Unmarshal([]byte(raw), &node); err != nil {
		t.Fatal(err)
	}
	node.Capabilities = []string{"tcp", "udp", "direct"}
	encoded, _ := json.Marshal(node)
	if _, err := f.db.DB.Exec(f.db.Rebind("UPDATE cp_nodes SET payload=? WHERE id=?"), string(encoded), node.ID); err != nil {
		t.Fatal(err)
	}
	var diag struct {
		Checks []struct {
			Name string `json:"name"`
			OK   bool   `json:"ok"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(f.request("GET", "/rules/"+a.ID+"/diagnose", nil, f.user, 200), &diag); err != nil {
		t.Fatal(err)
	}
	denied := false
	for _, check := range diag.Checks {
		if check.Name == "agent_limits_capability" && !check.OK {
			denied = true
		}
	}
	if !denied {
		t.Fatal("old Agent limitation hidden in diagnostics")
	}
	f.request("PUT", "/rules/"+a.ID, a, f.user, 409)
	f.sync()
	if len(f.store.Config().Rules) != 0 {
		t.Fatal("limited config sent to old Agent")
	}
	f.sync()
	if len(f.store.Config().Rules) != 2 {
		t.Fatal("ACK capabilities did not restore config")
	}
	f.plan.Limits.MaxRules = 1
	f.plan = decode[commerce.Plan](t, f.request("PUT", "/plans/"+f.plan.ID, f.plan, f.admin, 200))
	f.sync()
	if len(f.store.Config().Rules) != 2 {
		t.Fatal("plan edit retroactively downgraded entitlement")
	}
	f.ent = f.purchase("downgrade", 2)
	f.sync()
	ids := []string{a.ID, b.ID}
	sort.Strings(ids)
	cfg := f.store.Config()
	if len(cfg.Rules) != 1 || cfg.Rules[0].ID != ids[0] || cfg.Rules[0].Lease.Limits.MaxRules != 1 {
		t.Fatal("downgrade not enforced", cfg)
	}
	if _, err := f.app.Commerce.Entitlement(context.Background(), f.owner.ID); err != nil {
		t.Fatal(err)
	}
}
