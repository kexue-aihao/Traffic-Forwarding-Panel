package platform

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
)

var ingressTestCaps = []string{"tcp", "direct", "advanced-routing-v1", "shared-tls-ingress-v1", "group-policy-v2", "inbound-inspection-v1", "http-stream-filter-v1", "peer-address-policy-v1", "route-failover-v1"}

func checkIngressStatus(t *testing.T, f *fixture, method, path string, input any, bearer string, status int) {
	t.Helper()
	rr := f.req(method, path, input, bearer)
	if rr.Code != status {
		t.Fatalf("%s %s: got %d want %d: %s", method, path, rr.Code, status, rr.Body.String())
	}
}

func enableTestIngress(t *testing.T, f *fixture, g contract.Group, n contract.Registered) contract.Group {
	t.Helper()
	cfg := read[contract.Config](t, f.req("GET", "/agent/config", nil, n.Token), 200)
	checkIngressStatus(t, f, "POST", "/agent/ack", contract.Ack{Version: cfg.Version, AppliedVersion: cfg.Version, Capabilities: ingressTestCaps}, n.Token, 204)
	g.Advanced = &contract.GroupAdvanced{PolicyVersion: 2, SharedTLSIngress: &contract.SharedTLSIngressSettings{Enabled: true, ListenIP: "0.0.0.0", Port: 443}}
	return read[contract.Group](t, f.req("PUT", "/groups/"+g.ID, g, ""), 200)
}
func sharedTestRule(g contract.Group, n contract.Registered, name string) contract.Rule {
	v := ruleFor(g, n)
	v.Listen = ""
	v.SharedTLS = &contract.SharedTLS{IngressID: "group", ServerName: name}
	return v
}

func TestGroupTLSIngressLifecycleAndReservations(t *testing.T) {
	f := setup(t)
	g, n := f.node()
	g = enableTestIngress(t, f, g, n)
	cfg := read[contract.Config](t, f.req("GET", "/agent/config", nil, n.Token), 200)
	if len(cfg.TLSIngresses) != 1 || len(cfg.Rules) != 0 || cfg.TLSIngresses[0].Listen != "0.0.0.0:443" {
		t.Fatal(cfg)
	}
	v := read[contract.Rule](t, f.req("POST", "/rules", sharedTestRule(g, n, "A.Example.COM."), ""), 201)
	if v.Listen != "0.0.0.0:443" || v.SharedTLS.ServerName != "a.example.com" || v.SharedTLS.IngressID != cfg.TLSIngresses[0].ID {
		t.Fatal(v)
	}
	// Editing a shared rule requires no listener or parent ID.
	v.Listen = ""
	v.Target = "127.0.0.1:9090"
	v = read[contract.Rule](t, f.req("PUT", "/rules/"+v.ID, v, ""), 200)
	if rr := f.req("POST", "/rules", sharedTestRule(g, n, "a.example.com"), ""); rr.Code != 409 {
		t.Fatal("duplicate SNI", rr.Code)
	}
	wrong := sharedTestRule(g, n, "b.example.com")
	wrong.Listen = "0.0.0.0:444"
	if rr := f.req("POST", "/rules", wrong, ""); rr.Code != 409 {
		t.Fatal("client supplied listener accepted")
	}
	var ports int
	if e := f.s.Store.DB.QueryRow("SELECT COUNT(*) FROM cp_ports").Scan(&ports); e != nil || ports != 0 {
		t.Fatal("rule reserved a second port", ports, e)
	}
	checkIngressStatus(t, f, "DELETE", fmt.Sprintf("/rules/%s?version=%d", v.ID, v.Version), nil, "", 204)
	cfg = read[contract.Config](t, f.req("GET", "/agent/config", nil, n.Token), 200)
	if len(cfg.TLSIngresses) != 1 || len(cfg.Rules) != 0 {
		t.Fatal("listener depended on last rule", cfg)
	}
	g.Advanced.SharedTLSIngress.Port = 444
	g = read[contract.Group](t, f.req("PUT", "/groups/"+g.ID, g, ""), 200)
	if e := f.s.Store.DB.QueryRow("SELECT COUNT(*) FROM cp_ingress_ports").Scan(&ports); e != nil || ports != 2 {
		t.Fatal("old port released before ACK", ports, e)
	}
	cfg = read[contract.Config](t, f.req("GET", "/agent/config", nil, n.Token), 200)
	if cfg.TLSIngresses[0].Listen != "0.0.0.0:444" {
		t.Fatal(cfg)
	}
	checkIngressStatus(t, f, "POST", "/agent/ack", contract.Ack{Version: cfg.Version, AppliedVersion: 1, Error: "listener bind failed"}, n.Token, 204)
	if e := f.s.Store.DB.QueryRow("SELECT COUNT(*) FROM cp_ingress_ports").Scan(&ports); e != nil || ports != 2 {
		t.Fatal("failed ACK released old port", ports, e)
	}
	checkIngressStatus(t, f, "POST", "/agent/ack", contract.Ack{Version: cfg.Version, AppliedVersion: cfg.Version}, n.Token, 204)
	if e := f.s.Store.DB.QueryRow("SELECT COUNT(*) FROM cp_ingress_ports").Scan(&ports); e != nil || ports != 1 {
		t.Fatal(ports, e)
	}
	g.Advanced.SharedTLSIngress.Enabled = false
	g = read[contract.Group](t, f.req("PUT", "/groups/"+g.ID, g, ""), 200)
	if rr := f.req("DELETE", fmt.Sprintf("/groups/%s?version=%d", g.ID, g.Version), nil, ""); rr.Code != 409 {
		t.Fatal("group released unacknowledged port", rr.Code)
	}
	cfg = read[contract.Config](t, f.req("GET", "/agent/config", nil, n.Token), 200)
	if len(cfg.TLSIngresses) != 0 {
		t.Fatal("disabled ingress still emitted")
	}
	checkIngressStatus(t, f, "POST", "/agent/ack", contract.Ack{Version: cfg.Version, AppliedVersion: cfg.Version}, n.Token, 204)
	checkIngressStatus(t, f, "DELETE", fmt.Sprintf("/groups/%s?version=%d", g.ID, g.Version), nil, "", 204)
}

func TestGroupTLSIngressCrossAccountPolicyAndConcurrentSNI(t *testing.T) {
	f := setup(t)
	g, n := f.node()
	g = enableTestIngress(t, f, g, n)
	adminCookie := f.cookie
	identity := read[contract.IdentityGroup](t, f.req("POST", "/identity-groups", map[string]any{"id": "shared-customers", "name": "shared-customers"}, ""), 201)
	alice := read[contract.UserCreated](t, f.req("POST", "/users", map[string]any{"username": "ingress-alice", "role": "user", "identity_group_id": identity.ID}, ""), 201)
	bob := read[contract.UserCreated](t, f.req("POST", "/users", map[string]any{"username": "ingress-bob", "role": "user", "identity_group_id": identity.ID}, ""), 201)
	g.IdentityGroupIDs = []string{identity.ID}
	g.Advanced.TLSInboundPolicy = 2
	g = read[contract.Group](t, f.req("PUT", "/groups/"+g.ID, g, ""), 200)
	f.s.opts.Entitlements = &multiplierRecorder{}
	for i, u := range []contract.UserCreated{alice, bob} {
		login := f.req("POST", "/auth/login", map[string]any{"username": u.Username, "password": u.InitialPassword}, "")
		f.cookie = login.Result().Cookies()[0]
		read[contract.Rule](t, f.req("POST", "/rules", sharedTestRule(g, n, fmt.Sprintf("user%d.example.com", i)), ""), 201)
		if rr := f.req("POST", "/rules", ruleFor(g, n), ""); rr.Code != 409 {
			t.Fatal("user acquired independent port under TLS policy 2")
		}
	}
	f.cookie = adminCookie
	cfg := read[contract.Config](t, f.req("GET", "/agent/config", nil, n.Token), 200)
	if len(cfg.Rules) != 2 {
		t.Fatal("cross-account routing missing", cfg)
	}
	g.IdentityGroupIDs = nil
	g = read[contract.Group](t, f.req("PUT", "/groups/"+g.ID, g, ""), 200)
	cfg = read[contract.Config](t, f.req("GET", "/agent/config", nil, n.Token), 200)
	if len(cfg.Rules) != 0 || len(cfg.TLSIngresses) != 1 {
		t.Fatal("authorization revocation failed", cfg)
	}
	f.s.opts.Entitlements = nil
	var wg sync.WaitGroup
	statuses := make(chan int, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses <- f.req("POST", "/rules", sharedTestRule(g, n, "race.example.com"), "").Code
		}()
	}
	wg.Wait()
	close(statuses)
	successes := 0
	for code := range statuses {
		if code == 201 {
			successes++
		} else if code != 409 {
			t.Fatal(code)
		}
	}
	if successes != 1 {
		t.Fatal("duplicate concurrent reservation", successes)
	}
}

func TestTLSIngressPortConflictsCapabilitiesAndImport(t *testing.T) {
	f := setup(t)
	g, n := f.node()
	g = enableTestIngress(t, f, g, n)
	input := ruleFor(g, n)
	input.Listen = "0.0.0.0:20001"
	normal := read[contract.Rule](t, f.req("POST", "/rules", input, ""), 201)
	g.Advanced.SharedTLSIngress.Port = 20001
	if rr := f.req("PUT", "/groups/"+g.ID, g, ""); rr.Code != 409 {
		t.Fatal("shared ingress took independent rule port")
	}
	g.Advanced.SharedTLSIngress.Port = 443
	// Imported managed routes retain SNI and revalidate group placement.
	err := f.s.Store.Write(context.Background(), storage.Critical, func(tx *sql.Tx) error {
		_, e := f.s.importRuleTx(context.Background(), tx, normal.UserID, true, sharedTestRule(g, n, "import.example.com"))
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := read[contract.Config](t, f.req("GET", "/agent/config", nil, n.Token), 200)
	oldCaps := append([]string{}, ingressTestCaps...)
	oldCaps = oldCaps[:3]
	checkIngressStatus(t, f, "POST", "/agent/ack", contract.Ack{Version: cfg.Version, AppliedVersion: cfg.Version, Capabilities: oldCaps}, n.Token, 204)
	if rr := f.req("POST", "/rules", sharedTestRule(g, n, "unsupported.example.com"), ""); rr.Code != 409 {
		t.Fatal("unsupported Agent accepted")
	}
	cfg = read[contract.Config](t, f.req("GET", "/agent/config", nil, n.Token), 200)
	if len(cfg.TLSIngresses) != 0 {
		t.Fatal("old Agent got managed ingress")
	}
	for _, r := range cfg.Rules {
		if managedTLS(r) {
			t.Fatal("old Agent got shared route")
		}
	}
}
