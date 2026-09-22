package platform

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
)

func TestProtocolLayersDoNotLeakCarriersToApplicationDetectors(t *testing.T) {
	f := setup(t)
	g, n := f.node()
	g.BlockedProtocols = []string{"app:http"}
	g.DisabledNetworks = []string{"udp"}
	g.DisabledTransports = []string{"ws"}
	g = read[contract.Group](t, f.req("PUT", "/groups/"+g.ID, g, ""), 200)
	rule := ruleFor(g, n)
	rule.Transport = "http"
	rule.Tunnel = &contract.Tunnel{Endpoint: "exit.example:8080", Token: "http-tunnel-test-token"}
	rule.BlockedProtocols = []string{"socks"}
	r := read[contract.Rule](t, f.req("POST", "/rules", rule, ""), 201)
	c := read[contract.Config](t, f.req("GET", "/agent/config", nil, n.Token), 200)
	if len(c.Rules) != 1 || len(c.Rules[0].BlockedProtocols) != 2 || !contains(c.Rules[0].BlockedProtocols, "socks") || !contains(c.Rules[0].BlockedProtocols, "http") {
		t.Fatal(c)
	}
	g.DisabledTransports = []string{"http"}
	g = read[contract.Group](t, f.req("PUT", "/groups/"+g.ID, g, ""), 200)
	c = read[contract.Config](t, f.req("GET", "/agent/config", nil, n.Token), 200)
	if len(c.Rules) != 0 {
		t.Fatalf("parent transport deny bypassed: %s", r.ID)
	}
	g.DisabledTransports = nil
	g = read[contract.Group](t, f.req("PUT", "/groups/"+g.ID, g, ""), 200)
	c = read[contract.Config](t, f.req("GET", "/agent/config", nil, n.Token), 200)
	if len(c.Rules) != 1 || !contains(c.Rules[0].BlockedProtocols, "http") {
		t.Fatal("restoring HTTP forwarding lost the application block", c)
	}
	g.BlockedProtocols = nil
	g.DisabledTransports = []string{"ws"}
	read[contract.Group](t, f.req("PUT", "/groups/"+g.ID, g, ""), 200)
	c = read[contract.Config](t, f.req("GET", "/agent/config", nil, n.Token), 200)
	if len(c.Rules) != 1 || !slices.Equal(c.Rules[0].BlockedProtocols, []string{"socks"}) {
		t.Fatal("removing group application block changed rule policy", c)
	}
}

func TestLegacyGroupPoliciesSplitOnReadAndSave(t *testing.T) {
	f := setup(t)
	g, n := f.node()
	g.BlockedProtocols = []string{"tcp", "network:udp", "http", "transport:ws", "transport:direct-tls", "app:http", "socks"}
	// Seed the old on-disk representation, before any API normalization.
	if _, err := f.s.Store.DB.Exec(f.s.q("UPDATE cp_groups SET payload=? WHERE id=?"), strJSON(g), g.ID); err != nil {
		t.Fatal(err)
	}
	if res := f.req("POST", "/rules", ruleFor(g, n), ""); res.Code != 409 {
		t.Fatal("legacy restrictions stopped applying before migration", res.Body.String())
	}
	page := read[struct{ Items []contract.Group }](t, f.req("GET", "/groups", nil, ""), 200)
	if len(page.Items) != 1 {
		t.Fatal(page)
	}
	g = page.Items[0]
	check := func(g contract.Group) {
		t.Helper()
		if !slices.Equal(g.BlockedProtocols, []string{"app:http", "app:socks"}) || !slices.Equal(g.DisabledNetworks, []string{"tcp", "udp"}) || !slices.Equal(g.DisabledTransports, []string{"http", "ws", "direct-tls"}) {
			t.Fatal("legacy policy changed meaning", g)
		}
	}
	check(g)
	g = read[contract.Group](t, f.req("PUT", "/groups/"+g.ID, g, ""), 200)
	check(g)
	var payload string
	if err := f.s.Store.DB.QueryRow(f.s.q("SELECT payload FROM cp_groups WHERE id=?"), g.ID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var stored contract.Group
	if err := json.Unmarshal([]byte(payload), &stored); err != nil {
		t.Fatal(err)
	}
	check(stored)
	// Old API clients can still submit their namespaced representation.
	g.BlockedProtocols = []string{"network:tcp", "network:udp", "transport:http", "transport:ws", "transport:direct-tls", "app:http", "app:socks"}
	g.DisabledNetworks, g.DisabledTransports = nil, nil
	check(read[contract.Group](t, f.req("PUT", "/groups/"+g.ID, g, ""), 200))
}

func TestGroupPolicyRejectsValuesInWrongFields(t *testing.T) {
	f := setup(t)
	g, _ := f.node()
	for _, tc := range []struct{ field, value string }{
		{"blocked_protocols", "fet"},
		{"disabled_networks", "http"},
		{"disabled_transports", "udp"},
		{"disabled_transports", "app:http"},
		{"blocked_protocols", "network:http"},
		{"blocked_protocols", "transport:udp"},
	} {
		t.Run(tc.field+"/"+tc.value, func(t *testing.T) {
			body := map[string]any{"name": "invalid", "port_min": g.PortMin, "port_max": g.PortMax, tc.field: []string{tc.value}}
			if res := f.req("POST", "/groups", body, ""); res.Code != 400 {
				t.Fatal("invalid policy accepted", res.Body.String())
			}
		})
	}
}

func TestAdvancedPolicyUsesReferenceNamesAndAppliesUDPBlock(t *testing.T) {
	f := setup(t)
	g, n := f.node()
	g.Advanced = &contract.GroupAdvanced{BlockedProtocol: []string{"http"}, DisableUDP: true, FailTimeoutSec: 12}
	saved := read[contract.Group](t, f.req("PUT", "/groups/"+g.ID, g, ""), 200)
	if saved.Advanced == nil || !slices.Equal(saved.Advanced.BlockedProtocol, []string{"http"}) || saved.Advanced.FailTimeoutSec != 12 {
		t.Fatalf("advanced settings were not normalized: %+v", saved.Advanced)
	}
	udp := ruleFor(g, n)
	udp.Network = "udp"
	if res := f.req("POST", "/rules", udp, ""); res.Code != 409 {
		t.Fatal("disable_udp did not reject UDP rule", res.Code, res.Body.String())
	}
	tcp := ruleFor(g, n)
	read[contract.Rule](t, f.req("POST", "/rules", tcp, ""), 201)
	cfg := read[contract.Config](t, f.req("GET", "/agent/config", nil, n.Token), 200)
	if len(cfg.Rules) != 1 {
		t.Fatal("disable_udp unexpectedly blocked TCP")
	}
	if !contains(cfg.Rules[0].BlockedProtocols, "http") {
		t.Fatal("advanced bare http was not delivered as application block")
	}
}

func TestAdvancedAllowedHostConflictsWithOtherInboundBlocks(t *testing.T) {
	f := setup(t)
	body := map[string]any{
		"name": "host-conflict",
		"advanced": map[string]any{
			"allowed_host": []string{"example.com"},
			"blocked_path": []string{"/private"},
		},
	}
	if res := f.req("POST", "/groups", body, ""); res.Code != 400 {
		t.Fatal("allowed_host conflict accepted", res.Code, res.Body.String())
	}
}

func TestAdvancedSettingsPreserveReferenceValuesOnReadAndSave(t *testing.T) {
	f := setup(t)
	g, _ := f.node()
	g.Advanced = &contract.GroupAdvanced{
		BlockedProtocol: []string{"app:http", "socks"},
		MaxFail:         0, FailTimeoutSec: 0,
		TLS: map[string]any{"server_name": "example.com", "enabled": false},
	}
	for range 2 {
		g = read[contract.Group](t, f.req("PUT", "/groups/"+g.ID, g, ""), 200)
		page := read[struct{ Items []contract.Group }](t, f.req("GET", "/groups", nil, ""), 200)
		g = page.Items[0]
		if !slices.Equal(g.Advanced.BlockedProtocol, []string{"http", "socks"}) || !slices.Equal(g.BlockedProtocols, []string{"app:http", "app:socks"}) {
			t.Fatal("reference protocol names changed", g)
		}
		var wire map[string]any
		if err := json.Unmarshal([]byte(strJSON(g.Advanced)), &wire); err != nil {
			t.Fatal(err)
		}
		if wire["max_fail"] != float64(0) || wire["fail_timout_sec"] != float64(0) {
			t.Fatal("explicit zero values omitted", wire)
		}
		if g.Advanced.TLS["server_name"] != "example.com" || g.Advanced.TLS["enabled"] != false {
			t.Fatal("TLS object changed", g.Advanced.TLS)
		}
	}
}

type unavailableAllocator struct{}

func (unavailableAllocator) Allocate(context.Context, *sql.Tx, string, string, string) (*contract.Lease, error) {
	return nil, contract.ErrEntitlementUnavailable
}

func TestExhaustedEntitlementStillPublishesConfiguration(t *testing.T) {
	f := setup(t)
	g, n := f.node()
	read[contract.Rule](t, f.req("POST", "/rules", ruleFor(g, n), ""), 201)
	c := read[contract.Config](t, f.req("GET", "/agent/config", nil, n.Token), 200)
	r := c.Rules[0]
	r.Lease.ExpiresAt = time.Now().Add(-time.Minute)
	if err := f.s.Store.Write(context.Background(), storage.Critical, func(tx *sql.Tx) error {
		_, e := tx.Exec(f.s.q("UPDATE cp_rules SET payload=? WHERE id=?"), strJSON(r), r.ID)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	f.s.opts.Entitlements = unavailableAllocator{}
	next := read[contract.Config](t, f.req("GET", "/agent/config", nil, n.Token), 200)
	if len(next.Rules) != 0 || next.Version <= c.Version {
		t.Fatal("expired rule blocked config revocation", next)
	}
}
