package platform

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
	"strings"
	"testing"
	"time"
)

func TestUDPExitSelectionPolicyCapabilitiesAndRedaction(t *testing.T) {
	f := setup(t)
	entry, node := f.node()
	group, exitNode := f.node()
	group.Type = contract.GroupExit
	group = read[contract.Group](t, f.req("PUT", "/groups/"+group.ID, group, ""), 200)
	for _, n := range []contract.Registered{node, exitNode} {
		current := read[contract.Config](t, f.req("GET", "/agent/config", nil, n.Token), 200)
		if r := f.req("POST", "/agent/ack", contract.Ack{Version: current.Version, AppliedVersion: current.Version, Capabilities: []string{"udp-datagram-v1", "udp-credit-v1"}}, n.Token); r.Code != 204 {
			t.Fatal(r.Code, r.Body.String())
		}
		if e := f.s.Store.Write(context.Background(), storage.Normal, func(tx *sql.Tx) error {
			_, e := tx.Exec(f.s.q("UPDATE cp_nodes SET last_seen=? WHERE id=?"), time.Now().Unix(), n.NodeID)
			return e
		}); e != nil {
			t.Fatal(e)
		}
	}
	exit := read[contract.Exit](t, f.req("POST", "/exits", contract.Exit{Name: "native", GroupID: group.ID, NodeID: exitNode.NodeID, Transport: "tls", Tunnel: contract.Tunnel{Endpoint: "exit.example:9443", ServerName: "exit.example", Token: "tcp-secret-token-123456"}, UDP: &contract.UDPExit{Endpoint: "exit.example:9443", ServerName: "exit.example", Token: "udp-secret-token-123456"}, Weight: 1, Enabled: true}, ""), 201)
	if exit.UDP == nil || exit.UDP.Token != "" {
		t.Fatal("UDP token disclosed")
	}
	input := ruleFor(entry, node)
	input.Network = "udp"
	input.ExitGroupID = group.ID
	rule := read[contract.Rule](t, f.req("POST", "/rules", input, ""), 201)
	if rule.Transport != "quic" || rule.Tunnel != nil {
		t.Fatal("native rule not resolved/redacted", rule)
	}
	cfg := read[contract.Config](t, f.req("GET", "/agent/config", nil, node.Token), 200)
	if len(cfg.Rules) != 1 || cfg.Rules[0].Transport != "quic" || cfg.Rules[0].Tunnel.Token != "udp-secret-token-123456" || cfg.Rules[0].UDP == nil {
		t.Fatal("capability-negotiated native config missing")
	}
	page := f.req("GET", "/exits", nil, "")
	if strings.Contains(page.Body.String(), "secret-token") {
		t.Fatal("exit list leaked token")
	}
	// An old entry cannot silently inherit a required native endpoint.
	if r := f.req("POST", "/agent/ack", contract.Ack{Version: cfg.Version, AppliedVersion: cfg.Version, Capabilities: []string{"udp"}}, node.Token); r.Code != 204 {
		t.Fatal(r.Code, r.Body.String())
	}
	if e := f.s.refreshExits(context.Background(), node.NodeID); e != nil {
		t.Fatal(e)
	}
	var raw string
	f.s.Store.DB.QueryRow(f.s.q("SELECT payload FROM cp_rules WHERE id=?"), rule.ID).Scan(&raw)
	var updated contract.Rule
	json.Unmarshal([]byte(raw), &updated)
	if !updated.ExitUnavailable {
		t.Fatal("old node silently fell back")
	}
	entry.Advanced = &contract.GroupAdvanced{UDPOverTCP: true}
	read[contract.Group](t, f.req("PUT", "/groups/"+entry.ID, entry, ""), 200)
	if e := f.s.refreshExits(context.Background(), node.NodeID); e != nil {
		t.Fatal(e)
	}
	f.s.Store.DB.QueryRow(f.s.q("SELECT payload FROM cp_rules WHERE id=?"), rule.ID).Scan(&raw)
	updated = contract.Rule{}
	json.Unmarshal([]byte(raw), &updated)
	if updated.Transport != "tls" || updated.ExitUnavailable {
		t.Fatal("administrator UDP over TCP was ignored", updated)
	}
	// TCP and UDP may share a port; another UDP rule on the exit cannot.
	conflict := ruleFor(group, exitNode)
	conflict.Network = "udp"
	conflict.Listen = ":9443"
	if response := f.req("POST", "/rules", conflict, ""); response.Code != 409 || !strings.Contains(response.Body.String(), "physical port reserved") {
		t.Fatal("exit UDP port not reserved", response.Code)
	}
}

func TestRecoveryUsageAuditMetadataAndReplay(t *testing.T) {
	f := setup(t)
	g, n := f.node()
	rule := read[contract.Rule](t, f.req("POST", "/rules", ruleFor(g, n), ""), 201)
	cfg := read[contract.Config](t, f.req("GET", "/agent/config", nil, n.Token), 200)
	lease := cfg.Rules[0].Lease
	window := strings.Repeat("a", 32)
	u := contract.UsageRecord{ID: window + ":1", NodeID: n.NodeID, RuleID: rule.ID, LeaseID: lease.ID, EntitlementID: lease.EntitlementID, StartedAt: time.Now().Add(-time.Second), EndedAt: time.Now(), UploadBytes: 25, Kind: "recovery", WindowID: window, WindowSequence: 1}
	for range 2 {
		read[map[string]any](t, f.req("POST", "/agent/usage", contract.UsageBatch{Records: []contract.UsageRecord{u}}, n.Token), 200)
	}
	audit := read[struct {
		Items []contract.UsageRecord
		Total int
	}](t, f.req("GET", "/usage-audit?rule_id="+rule.ID, nil, ""), 200)
	if audit.Total != 1 || audit.Items[0].Kind != "recovery" {
		t.Fatal("recovery facts hidden or repeated", audit)
	}
	u.Kind = "normal"
	if r := f.req("POST", "/agent/usage", contract.UsageBatch{Records: []contract.UsageRecord{u}}, n.Token); r.Code != 409 {
		t.Fatal("same ID changed audit classification")
	}
	u.ID = "invalid-window-id"
	if r := f.req("POST", "/agent/usage", contract.UsageBatch{Records: []contract.UsageRecord{u}}, n.Token); r.Code != 409 {
		t.Fatal("unstable window identity accepted")
	}
}
