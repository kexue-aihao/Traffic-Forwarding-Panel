package platform

import (
	"testing"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

func TestGroupTypeControlsEnrollmentRulesAndExitRoles(t *testing.T) {
	f := setup(t)
	monitor := read[contract.Group](t, f.req("POST", "/groups", map[string]any{
		"name": "monitor", "type": contract.GroupMonitor, "port_min": 20000, "port_max": 21000,
	}, ""), 201)
	if monitor.Type != contract.GroupMonitor || monitor.CanEnter() {
		t.Fatal("monitor role unexpectedly accepts entry rules", monitor)
	}
	if res := f.req("POST", "/nodes/enrollment", map[string]any{"name": "monitor-node", "group_ids": []string{monitor.ID}}, ""); res.Code != 201 {
		t.Fatal("monitor groups should still accept probe agents", res.Code, res.Body.String())
	}
	entry := read[contract.Group](t, f.req("POST", "/groups", map[string]any{
		"name": "entry", "type": contract.GroupEntry, "direct_policy": "allow", "port_min": 20000, "port_max": 21000,
	}, ""), 201)
	exit := read[contract.Group](t, f.req("POST", "/groups", map[string]any{
		"name": "exit", "type": contract.GroupExit, "port_min": 20000, "port_max": 21000,
	}, ""), 201)
	if !entry.CanEnter() || entry.CanHostExit() || !exit.CanHostExit() {
		t.Fatal("entry/exit role capabilities are wrong", entry, exit)
	}
	if res := f.req("PUT", "/groups/"+entry.ID, contract.Group{ID: entry.ID, Version: entry.Version, Name: entry.Name, Type: contract.GroupExit, PortMin: entry.PortMin, PortMax: entry.PortMax, Multiplier: "1"}, ""); res.Code != 409 {
		t.Fatal("group type changed after creation", res.Code, res.Body.String())
	}
	if res := f.req("POST", "/exits", contract.Exit{Name: "invalid", GroupID: entry.ID, NodeID: "missing", Transport: "tls", Tunnel: contract.Tunnel{Endpoint: "exit.example:443", Token: "exit-token-long-enough", ServerName: "exit.example"}, Weight: 1, Enabled: true}, ""); res.Code != 409 {
		t.Fatal("entry group accepted a managed exit", res.Code, res.Body.String())
	}
	if res := f.req("POST", "/rules", contract.Rule{Name: "monitor-rule", NodeID: "missing", GroupID: monitor.ID, Network: "tcp", Transport: "direct", Listen: ":20001", Target: "127.0.0.1:80", Enabled: true}, ""); res.Code != 409 {
		t.Fatal("monitor group accepted a forwarding rule", res.Code, res.Body.String())
	}
}

func TestChainExitRequiresDistinctPhysicalExitGroups(t *testing.T) {
	f := setup(t)
	exit := func(name string) contract.Group {
		return read[contract.Group](t, f.req("POST", "/groups", map[string]any{
			"name": name, "type": contract.GroupExit, "port_min": 20000, "port_max": 21000,
		}, ""), 201)
	}
	one, two := exit("one"), exit("two")
	chain := read[contract.Group](t, f.req("POST", "/groups", map[string]any{
		"name": "chain", "type": contract.GroupChainExit, "chain_group_ids": []string{one.ID, two.ID}, "port_min": 20000, "port_max": 21000,
	}, ""), 201)
	if chain.Type != contract.GroupChainExit || len(chain.ChainGroupIDs) != 2 {
		t.Fatal("chain exit was not persisted", chain)
	}
	for _, ids := range [][]string{{one.ID}, {one.ID, one.ID}, {one.ID, two.ID, one.ID, two.ID}} {
		if res := f.req("POST", "/groups", map[string]any{
			"name": "bad-chain", "type": contract.GroupChainExit, "chain_group_ids": ids, "port_min": 20000, "port_max": 21000,
		}, ""); res.Code != 400 && res.Code != 409 {
			t.Fatal("invalid chain was accepted", ids, res.Code, res.Body.String())
		}
	}
	if res := f.req("POST", "/nodes/enrollment", map[string]any{"name": "chain-node", "group_ids": []string{chain.ID}}, ""); res.Code != 400 && res.Code != 409 {
		t.Fatal("chain group accepted direct device enrollment", res.Code, res.Body.String())
	}
}
