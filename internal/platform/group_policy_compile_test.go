package platform

import (
	"strings"
	"testing"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

func TestManagedRouteCapabilityAndMigrationGating(t *testing.T) {
	g := contract.Group{ID: "exit-group", Advanced: &contract.GroupAdvanced{PolicyVersion: 2}}
	top := managedTopology{groups: map[string]contract.Group{g.ID: g}, nodes: map[string]contract.Node{"exit": {Capabilities: []string{"managed-services-v1"}}}}
	r := contract.Rule{ID: "rule", NodeID: "entry", GroupID: "entry-group", ExitGroupID: g.ID, Network: "tcp"}
	x := contract.Exit{ID: "x", GroupID: g.ID, NodeID: "exit", Enabled: true}
	if _, err := top.route(r, x, false); err == nil || err.Error() != "advanced_exit_requires_managed_service" {
		t.Fatal("activated exit silently used legacy service", err)
	}
	x.Managed = true
	if _, err := top.route(r, x, false); err == nil || !strings.Contains(err.Error(), "group-policy-v2") {
		t.Fatal("managed flag bypassed missing policy capability", err)
	}
	top.nodes["exit"] = contract.Node{Capabilities: []string{"managed-services-v1", "group-policy-v2", "inbound-inspection-v1", "http-stream-filter-v1", "peer-address-policy-v1", "route-failover-v1"}}
	if _, err := top.route(r, x, false); err != nil {
		t.Fatal(err)
	}
	g.Advanced.ReverseGroup = []string{r.GroupID}
	g.Advanced.Protocol = "tls_simple"
	top.groups[g.ID] = g
	if _, err := top.route(r, x, false); err == nil || !strings.Contains(err.Error(), "reverse-group-v1") {
		t.Fatal("reverse carrier capability not gated", err)
	}
}

func TestSharedReverseHubRejectsConflictingTLS(t *testing.T) {
	hub := contract.Exit{ID: "hub", GroupID: "entry", Transport: "tls"}
	group := contract.Group{ID: "a", Advanced: &contract.GroupAdvanced{PolicyVersion: 2, ReverseGroup: []string{hub.GroupID}, TLS: map[string]any{"server_name": "one.example"}}}
	other := contract.Group{ID: "b", Advanced: &contract.GroupAdvanced{PolicyVersion: 2, ReverseGroup: []string{hub.GroupID}, TLS: map[string]any{"server_name": "two.example"}}}
	top := managedTopology{groups: map[string]contract.Group{group.ID: group, other.ID: other}, exits: []contract.Exit{{GroupID: group.ID, Enabled: true, Managed: true}, {GroupID: other.ID, Enabled: true, Managed: true}}}
	if _, err := top.hubTLS(group, hub); err == nil || err.Error() != "reverse_hub_tls_policy_conflict" {
		t.Fatal("one listener silently selected conflicting TLS settings", err)
	}
	other.Advanced.TLS["server_name"] = "ONE.EXAMPLE."
	top.groups[other.ID] = other
	if _, err := top.hubTLS(group, hub); err != nil {
		t.Fatal("normalized equal TLS settings rejected", err)
	}
}
