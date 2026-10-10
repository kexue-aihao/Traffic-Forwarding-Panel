package platform

import (
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/agent"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/policy/detect"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/tunnel"
)

func inspectionCaps() []string {
	return []string{"group-policy-v2", "inbound-inspection-v1", "http-stream-filter-v1", "peer-address-policy-v1", "route-failover-v1", "application-inspection-v1", "block:http", "inspect:socks4-tcp-v1", "inspect:socks5-tcp-v1", "inspect:ss-aead2017-v1", "inspect:vmess-aead-v1", "inspect:trojan-v1", "business-tls-termination-v1", "staged-inspection-v1"}
}

func TestInspectionNamesPreserveLegacyCarrierAndSOCKSUnion(t *testing.T) {
	g := splitGroupPolicy(contract.Group{BlockedProtocols: []string{"http", "app:http", "socks", "app:socks5", "app:shadowsocks", "app:trojan", "app:vmess"}})
	if !slices.Equal(g.DisabledTransports, []string{"http"}) || !slices.Equal(g.BlockedProtocols, []string{"app:http", "app:socks", "app:socks5", "app:shadowsocks", "app:trojan", "app:vmess"}) {
		t.Fatalf("migration changed historical meanings: %+v", g)
	}
	layer := groupLayer(contract.Group{Advanced: &contract.GroupAdvanced{PolicyVersion: 2, BlockedProtocol: []string{"http", "socks5"}}})
	if !slices.Equal(layer.BlockedApps, []string{"http", "socks5"}) {
		t.Fatal("advanced bare http was dropped", layer)
	}
}

func TestInspectionActivationAndSecretFieldsRejected(t *testing.T) {
	for _, app := range []string{"socks4", "socks5", "shadowsocks", "trojan", "vmess"} {
		if err := validateGroupAdvanced(&contract.GroupAdvanced{BlockedProtocol: []string{app}}); err == nil {
			t.Fatalf("%s accepted without explicit inspection activation", app)
		}
		if err := validateGroupAdvanced(&contract.GroupAdvanced{PolicyVersion: 2, BlockedProtocol: []string{"app:" + app}, Inspection: &contract.InspectionPolicy{Version: 1}}); err != nil {
			t.Fatal(app, err)
		}
	}
	var a contract.GroupAdvanced
	for _, raw := range []string{`{"policy_version":2,"inspection":{"version":1,"password":"secret"}}`, `{"policy_version":2,"inspection":{"version":1,"business":{"private_key":"secret"}}}`} {
		if err := json.Unmarshal([]byte(raw), &a); err == nil {
			t.Fatal("credential-bearing API field accepted")
		}
	}
}

func TestInspectionStrictCoverageAndObservationAreDifferent(t *testing.T) {
	profile := contract.InspectionProfileStatus{Label: "known-ss", Protocol: "shadowsocks", Variants: []string{"aead2017"}, Networks: []string{"tcp", "udp"}, Ready: true}
	for _, tc := range []struct {
		name, app, network, mode, want string
		caps                           []string
		profiles                       []contract.InspectionProfileStatus
		business                       *contract.BusinessInbound
	}{
		{name: "missing_engine", app: "socks5", network: "tcp", want: "application-inspection-v1"},
		{name: "socks_tcp", app: "socks5", network: "tcp", caps: inspectionCaps()},
		{name: "socks_udp_unassociated", app: "socks5", network: "udp", caps: inspectionCaps(), want: "inspect:socks5-udp-associated-v1"},
		{name: "socks_udp_observe", app: "socks5", network: "udp", mode: "observe", caps: inspectionCaps()},
		{name: "ss_missing_local_key", app: "shadowsocks", network: "tcp", caps: inspectionCaps(), want: "inspection_profile_not_ready"},
		{name: "ss_tcp_ready", app: "shadowsocks", network: "tcp", caps: inspectionCaps(), profiles: []contract.InspectionProfileStatus{profile}},
		{name: "ss_missing_key_observe", app: "shadowsocks", network: "tcp", mode: "observe", caps: inspectionCaps()},
		{name: "vmess_raw_udp", app: "vmess", network: "udp", caps: inspectionCaps(), want: "unsupported_detector_network"},
		{name: "trojan_no_business_tls", app: "trojan", network: "tcp", caps: inspectionCaps(), profiles: []contract.InspectionProfileStatus{{Label: "known-ss", Protocol: "trojan", Variants: []string{"trojan-sha224"}, Networks: []string{"tcp"}, Ready: true}}, want: "business_tls_termination_required"},
		{name: "business_tls_missing_local_identity", app: "socks5", network: "tcp", caps: inspectionCaps(), business: &contract.BusinessInbound{TLSProfile: "owned"}, want: "business_tls_profile_not_ready"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := contract.InboundPolicy{BlockedApps: []string{tc.app}, Inspection: &contract.InspectionPolicy{Version: 1, Mode: tc.mode, Profiles: []string{"known-ss"}, Business: tc.business}}
			err := inspectionCapabilities(p, contract.Node{Capabilities: tc.caps, InspectionProfiles: tc.profiles}, tc.network)
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("coverage result %v, wanted %q", err, tc.want)
			}
		})
	}
}

func TestInspectionCompileBindsBusinessAndRejectsConflicts(t *testing.T) {
	policy := func(label string) *contract.GroupAdvanced {
		return &contract.GroupAdvanced{PolicyVersion: 2, BlockedProtocol: []string{"socks5"}, Inspection: &contract.InspectionPolicy{Version: 1, Business: &contract.BusinessInbound{TLSProfile: label}}}
	}
	groups := map[string]contract.Group{"entry": {ID: "entry", Advanced: policy("owned")}}
	r := contract.Rule{ID: "r", GroupID: "entry", Network: "tcp"}
	statuses := []contract.InspectionProfileStatus{{Label: "owned", Protocol: "business-tls", Variants: []string{"server"}, Networks: []string{"tcp"}, Ready: true}, {Label: "different", Protocol: "business-tls", Variants: []string{"server"}, Networks: []string{"tcp"}, Ready: true}}
	if err := compileGroupPolicy(&r, "admin", groups, inspectionCaps(), statuses); err != nil {
		t.Fatal(err)
	}
	if r.Business == nil || r.Business.TLSProfile != "owned" || r.EffectivePolicy == nil || r.EffectivePolicy.InboundLayers[0].Inspection == nil {
		t.Fatal("adapter was not bound into compiled policy", r)
	}
	targets := ruleTargets(r)
	if len(targets) != 1 || targets[0].Business == nil || targets[0].Business.TLSProfile != "owned" {
		t.Fatal("grant omitted business adapter", targets)
	}
	groups["exit"] = contract.Group{ID: "exit", Advanced: policy("different")}
	r.ExitGroupID = "exit"
	if err := compileGroupPolicy(&r, "admin", groups, inspectionCaps(), statuses); err == nil || err.Error() != "business_adapter_policy_conflict" {
		t.Fatal("conflicting adapters accepted", err)
	}
}

func TestInspectionObserveGroupMirrorNeverCreatesStrictRuleLayer(t *testing.T) {
	g := contract.Group{ID: "entry", BlockedProtocols: []string{"app:socks5"}, Advanced: &contract.GroupAdvanced{PolicyVersion: 2, BlockedProtocol: []string{"socks5"}, Inspection: &contract.InspectionPolicy{Version: 1, Mode: "observe"}}}
	r := contract.Rule{ID: "rule", GroupID: g.ID, Network: "tcp"}
	if err := compileGroupPolicy(&r, "admin", map[string]contract.Group{g.ID: g}, inspectionCaps()); err != nil {
		t.Fatal(err)
	}
	if len(r.EffectivePolicy.InboundLayers) != 1 || r.EffectivePolicy.InboundLayers[0].Inspection.Mode != "observe" {
		t.Fatal("group mirror enabled an extra strict rule policy", r.EffectivePolicy)
	}
	r = contract.Rule{ID: "rule", GroupID: g.ID, Network: "tcp", BlockedProtocols: []string{"socks5"}}
	if err := compileGroupPolicy(&r, "admin", map[string]contract.Group{g.ID: g}, inspectionCaps()); err != nil {
		t.Fatal(err)
	}
	if len(r.EffectivePolicy.InboundLayers) != 2 || r.EffectivePolicy.InboundLayers[1].GroupID != "rule" || r.EffectivePolicy.InboundLayers[1].Inspection.Mode != "strict" {
		t.Fatal("explicit rule restriction was lost", r.EffectivePolicy)
	}
}

func TestInspectionManagedStagedRouteAndLocalExitReadiness(t *testing.T) {
	entry := contract.Group{ID: "entry", Advanced: &contract.GroupAdvanced{PolicyVersion: 2, Inspection: &contract.InspectionPolicy{Version: 1, Business: &contract.BusinessInbound{TLSProfile: "owned"}}}}
	exit := contract.Group{ID: "exit", Advanced: &contract.GroupAdvanced{PolicyVersion: 2, BlockedProtocol: []string{"trojan"}, Inspection: &contract.InspectionPolicy{Version: 1, Profiles: []string{"known-trojan"}}}}
	now := time.Now()
	top := managedTopology{groups: map[string]contract.Group{entry.ID: entry, exit.ID: exit}, nodes: map[string]contract.Node{
		"entry-node": {Capabilities: inspectionCaps(), LastSeen: &now},
		"exit-node":  {Capabilities: append(inspectionCaps(), "managed-services-v1"), InspectionProfiles: []contract.InspectionProfileStatus{{Label: "known-trojan", Protocol: "trojan", Variants: []string{"trojan-sha224"}, Networks: []string{"tcp"}, Ready: true}}, LastSeen: &now},
	}}
	r := contract.Rule{ID: "r", NodeID: "entry-node", GroupID: entry.ID, ExitGroupID: exit.ID, Network: "tcp", Business: entry.Advanced.Inspection.Business}
	x := contract.Exit{ID: "x", NodeID: "exit-node", GroupID: exit.ID, Managed: true, Enabled: true}
	got, err := top.route(r, x, false)
	if err != nil {
		t.Fatal("exit must not require entry server certificate profile", err)
	}
	if !got.Tunnel.StagedInspection || !got.Tunnel.Inspect {
		t.Fatal("new inspector did not select staged target-before-dial route", got)
	}
	n := top.nodes["exit-node"]
	n.Capabilities = slices.DeleteFunc(n.Capabilities, func(v string) bool { return v == "staged-inspection-v1" })
	top.nodes["exit-node"] = n
	if _, err := top.route(r, x, false); err == nil || !strings.Contains(err.Error(), "staged-inspection-v1") {
		t.Fatal("old exit silently accepted new staged wire contract", err)
	}
}

func TestInspectionSOCKSUDPRequiresExplicitModeAndObservableAssociation(t *testing.T) {
	p := contract.InboundPolicy{BlockedApps: []string{"socks5"}, Inspection: &contract.InspectionPolicy{Version: 1, Profiles: []string{"udp-socks"}}}
	associated := contract.InspectionProfileStatus{Label: "udp-socks", Protocol: "socks5", Variants: []string{"socks5-udp-associated"}, Networks: []string{"tcp", "udp"}, Ready: true}
	structural := associated
	structural.Variants = []string{"socks5-udp-structural"}
	for _, tc := range []struct {
		name, want string
		caps       []string
		profiles   []contract.InspectionProfileStatus
		incoming   bool
	}{
		{"association-cap-without-profile", "inspection_profile_not_ready", append(inspectionCaps(), "inspect:socks5-udp-associated-v1"), nil, true},
		{"controlled-entry-association", "", append(inspectionCaps(), "inspect:socks5-udp-associated-v1"), []contract.InspectionProfileStatus{associated}, true},
		{"exit-cannot-prove-original-control-tuple", "inspection_not_observable", append(inspectionCaps(), "inspect:socks5-udp-associated-v1"), []contract.InspectionProfileStatus{associated}, false},
		{"explicit-structural-entry", "", append(inspectionCaps(), "inspect:socks5-udp-structural-v1"), []contract.InspectionProfileStatus{structural}, true},
		{"explicit-structural-exit", "", append(inspectionCaps(), "inspect:socks5-udp-structural-v1"), []contract.InspectionProfileStatus{structural}, false},
		{"structural-cap-without-consent", "inspect:socks5-udp-associated-v1", append(inspectionCaps(), "inspect:socks5-udp-structural-v1"), nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := inspectionCapabilitiesAt(p, contract.Node{Capabilities: tc.caps, InspectionProfiles: tc.profiles}, "udp", tc.incoming)
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("got %v, wanted %q", err, tc.want)
			}
		})
	}
}

func TestInspectionManualAndUnmanagedTunnelKeepEntryInspection(t *testing.T) {
	g := contract.Group{ID: "entry", Advanced: &contract.GroupAdvanced{PolicyVersion: 2, BlockedProtocol: []string{"socks5"}, Inspection: &contract.InspectionPolicy{Version: 1}}}
	r := contract.Rule{ID: "manual", NodeID: "entry-node", GroupID: g.ID, Network: "tcp", Transport: "tls", Tunnel: &contract.Tunnel{Endpoint: "127.0.0.1:443", Token: "test"}}
	groups := map[string]contract.Group{g.ID: g, "exit": {ID: "exit"}}
	if err := compileGroupPolicy(&r, "admin", groups, inspectionCaps()); err != nil {
		t.Fatal("manual tunnel cannot lose entry preinspection", err)
	}
	if r.EffectivePolicy == nil || r.Tunnel.StagedInspection {
		t.Fatal("manual exit has no advertised independent inspection", r)
	}
	top := managedTopology{groups: groups}
	r.ExitGroupID = "exit"
	x := contract.Exit{ID: "unmanaged", GroupID: "exit", Tunnel: *r.Tunnel}
	got, err := top.route(r, x, false)
	if err != nil || got.Tunnel.StagedInspection {
		t.Fatal("unmanaged exit must preserve entry-only inspection", got, err)
	}
}

func TestInspectionProfileMetadataBudgetAndSecretFreeReasons(t *testing.T) {
	profiles := make([]contract.InspectionProfileStatus, 384)
	for i := range profiles {
		profiles[i] = contract.InspectionProfileStatus{Label: fmt.Sprintf("p-%d", i), Protocol: "shadowsocks", Variants: []string{"aead2017"}, Networks: []string{"tcp", "udp"}, Ready: true}
	}
	if err := validateInspectionProfiles(profiles); err != nil {
		t.Fatal("combined detector and business metadata budget rejected", err)
	}
	if err := validateInspectionProfiles(append(profiles, contract.InspectionProfileStatus{})); err == nil {
		t.Fatal("excess metadata accepted")
	}
	profiles[0].Reason = "password-or-private-path"
	if err := validateInspectionProfiles(profiles); err == nil {
		t.Fatal("free-text credential-bearing readiness reason accepted")
	}
}

func TestInspectionAssociatedUDPFromAdvancedAPICompilesAndApplies(t *testing.T) {
	f := setup(t)
	newGroup := func(name string, advanced *contract.GroupAdvanced) contract.Group {
		return read[contract.Group](t, f.req("POST", "/groups", contract.Group{Name: name, PortMin: 1024, PortMax: 65535, Multiplier: "1", Advanced: advanced}, ""), 201)
	}
	controlGroup := newGroup("control", &contract.GroupAdvanced{PolicyVersion: 2})
	udpGroup := newGroup("associated-udp", &contract.GroupAdvanced{PolicyVersion: 2, BlockedProtocol: []string{"socks5"}, Inspection: &contract.InspectionPolicy{Version: 1, Profiles: []string{"association"}}})
	enrollment := read[map[string]string](t, f.req("POST", "/nodes/enrollment", map[string]any{"name": "associated-node", "group_ids": []string{controlGroup.ID, udpGroup.ID}}, ""), 201)
	profiles := []contract.InspectionProfileStatus{{Label: "association", Protocol: "socks5", Variants: []string{"socks5-udp-associated"}, Networks: []string{"tcp", "udp"}, Ready: true}}
	node := read[contract.Registered](t, f.req("POST", "/agent/register", contract.Registration{Token: enrollment["token"], Name: "associated-node", Capabilities: append(inspectionCaps(), "inspect:socks5-udp-associated-v1"), InspectionProfiles: profiles}, ""), 201)
	tcpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tcpListen := tcpListener.Addr().String()
	tcpListener.Close()
	udpListener, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	udpListen := udpListener.LocalAddr().String()
	udpListener.Close()
	control := ruleFor(controlGroup, node)
	control.Listen = tcpListen
	control = read[contract.Rule](t, f.req("POST", "/rules", control, ""), 201)
	udp := ruleFor(udpGroup, node)
	udp.Network, udp.Listen = "udp", udpListen
	udp = read[contract.Rule](t, f.req("POST", "/rules", udp, ""), 201)
	cfg := read[contract.Config](t, f.req("GET", "/agent/config", nil, node.Token), 200)
	if len(cfg.Rules) != 2 || len(cfg.BlockedRules) != 0 {
		t.Fatal("API did not compile controlled topology", cfg)
	}
	for _, rule := range cfg.Rules {
		if rule.EffectivePolicy == nil || rule.EffectivePolicy.Failover == nil {
			t.Fatal("test omitted the default advanced failover object", rule)
		}
	}
	store, err := agent.OpenStore(filepath.Join(t.TempDir(), "agent-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err = store.SetIdentity(node); err != nil {
		t.Fatal(err)
	}
	runtime := agent.NewRuntime(store, tunnel.Client{})
	t.Cleanup(runtime.Close)
	runtime.InspectionProfiles = detect.Profiles{"association": {Protocol: "socks5", UDPMode: "associated", RuleIDs: []string{udp.ID}, ControlRuleIDs: []string{control.ID}, Targets: []string{udp.Target}, RelayEndpoint: udpListen}}
	if err = runtime.Apply(cfg, true); err != nil {
		t.Fatal("default advanced policy incorrectly rejected a fixed association topology", err)
	}
}

func TestInspectionStageIsPerHopForEntryOnlyAndMixedPolicies(t *testing.T) {
	entry := contract.Group{ID: "entry", Advanced: &contract.GroupAdvanced{PolicyVersion: 2, BlockedProtocol: []string{"socks5"}, Inspection: &contract.InspectionPolicy{Version: 1}}}
	legacy := contract.Group{ID: "legacy"}
	inspected := contract.Group{ID: "inspected", Advanced: &contract.GroupAdvanced{PolicyVersion: 2, BlockedProtocol: []string{"socks5"}, Inspection: &contract.InspectionPolicy{Version: 1}}}
	top := managedTopology{groups: map[string]contract.Group{entry.ID: entry, legacy.ID: legacy, inspected.ID: inspected}, nodes: map[string]contract.Node{
		"entry-node": {Capabilities: inspectionCaps()}, "legacy-node": {Capabilities: []string{"managed-services-v1"}}, "inspected-node": {Capabilities: append(inspectionCaps(), "managed-services-v1")},
	}}
	r := contract.Rule{ID: "r", NodeID: "entry-node", GroupID: entry.ID, ExitGroupID: legacy.ID, Network: "tcp"}
	x := contract.Exit{ID: "legacy-x", GroupID: legacy.ID, NodeID: "legacy-node", Managed: true}
	first, err := top.route(r, x, false)
	if err != nil || first.Tunnel.StagedInspection {
		t.Fatal("entry-only inspection unnecessarily required a new exit", first, err)
	}
	r.ExitGroupID = inspected.ID
	x.GroupID, x.NodeID = inspected.ID, "inspected-node"
	second, err := top.route(r, x, false)
	if err != nil || !second.Tunnel.StagedInspection {
		t.Fatal("inspected hop omitted its local gate", second, err)
	}
	if err := stagedCapabilities(top.nodes["legacy-node"], top.nodes["inspected-node"]); err == nil {
		t.Fatal("an old preceding hop cannot send the authorized new wire contract")
	}
}

func TestInspectionServiceCompilerRebuildsStaleHopFlags(t *testing.T) {
	entry := contract.Group{ID: "entry", Advanced: &contract.GroupAdvanced{PolicyVersion: 2, BlockedProtocol: []string{"socks5"}, Inspection: &contract.InspectionPolicy{Version: 1}}}
	first := contract.Group{ID: "first"}
	last := contract.Group{ID: "last", Advanced: &contract.GroupAdvanced{PolicyVersion: 2, BlockedProtocol: []string{"socks5"}, Inspection: &contract.InspectionPolicy{Version: 1}}}
	chain := contract.Group{ID: "chain", Type: contract.GroupChainExit, ChainGroupIDs: []string{first.ID, last.ID}}
	top := managedTopology{groups: map[string]contract.Group{entry.ID: entry, first.ID: first, last.ID: last, chain.ID: chain}, nodes: map[string]contract.Node{
		"entry-node": {Capabilities: inspectionCaps()}, "first-node": {Capabilities: []string{"managed-services-v1", "staged-inspection-v1"}}, "last-node": {Capabilities: append(inspectionCaps(), "managed-services-v1")},
	}, exits: []contract.Exit{
		{ID: "first-x", GroupID: first.ID, NodeID: "first-node", Managed: true, Enabled: true, Tunnel: contract.Tunnel{Endpoint: "first.example:443"}},
		{ID: "last-x", GroupID: last.ID, NodeID: "last-node", Managed: true, Enabled: true, Tunnel: contract.Tunnel{Endpoint: "last.example:443"}},
	}}
	r := contract.Rule{ID: "r", NodeID: "entry-node", GroupID: entry.ID, ExitGroupID: chain.ID, Network: "tcp", SelectedExitID: "first-x", Tunnel: &contract.Tunnel{Endpoint: "first.example:443", Chain: []contract.TunnelHop{{Endpoint: "last.example:443"}}}}
	if err := top.refreshRouteInspection(&r); err != nil {
		t.Fatal(err)
	}
	if r.Tunnel.StagedInspection || !r.Tunnel.Chain[0].StagedInspection || !r.Tunnel.Chain[0].Inspect {
		t.Fatal("stale mixed-hop flags were not rebuilt from current local policy", r.Tunnel)
	}
	n := top.nodes["first-node"]
	n.Capabilities = []string{"managed-services-v1"}
	top.nodes["first-node"] = n
	if err := top.refreshRouteInspection(&r); err == nil || !strings.Contains(err.Error(), "staged-inspection-v1") {
		t.Fatal("old preceding Agent cannot enforce authorized successor flags", err)
	}
}

func TestInspectionProfileACKPublishesAndCanClearReadiness(t *testing.T) {
	f := setup(t)
	_, n := f.node()
	c := read[contract.Config](t, f.req("GET", "/agent/config", nil, n.Token), 200)
	profiles := []contract.InspectionProfileStatus{{Label: "local-ss", Protocol: "shadowsocks", Variants: []string{"aead2017"}, Networks: []string{"tcp"}, Ready: true}}
	ack := contract.Ack{Version: c.Version, AppliedVersion: c.Version, InspectionProfiles: profiles}
	if got := f.req("POST", "/agent/ack", ack, n.Token); got.Code != 204 {
		t.Fatal(got.Code, got.Body.String())
	}
	var raw string
	if err := f.s.Store.DB.QueryRow(f.s.q("SELECT payload FROM cp_nodes WHERE id=?"), n.NodeID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var stored contract.Node
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.InspectionProfiles) != 1 || !stored.InspectionProfiles[0].Ready {
		t.Fatal("profile readiness not persisted", stored)
	}
	next := read[contract.Config](t, f.req("GET", "/agent/config", nil, n.Token), 200)
	if next.Version <= c.Version {
		t.Fatal("readiness changes did not publish dependencies")
	}
	ack = contract.Ack{Version: next.Version, AppliedVersion: next.Version, InspectionProfiles: []contract.InspectionProfileStatus{}}
	if got := f.req("POST", "/agent/ack", ack, n.Token); got.Code != 204 {
		t.Fatal(got.Code, got.Body.String())
	}
	if err := f.s.Store.DB.QueryRow(f.s.q("SELECT payload FROM cp_nodes WHERE id=?"), n.NodeID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	stored = contract.Node{}
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.InspectionProfiles) != 0 {
		t.Fatal("empty readiness report left stale credentials available")
	}
}
