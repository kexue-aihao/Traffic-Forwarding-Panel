package detect

import (
	"errors"
	"strings"
	"testing"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/policy/association"
)

func TestObserveUnavailablePrerequisitesPreserveStrictLayers(t *testing.T) {
	tests := []struct {
		name, protocol, reason string
		profile                Profile
		scope                  Scope
	}{
		{"rule_scope", "vmess", "profile_scope_denied", Profile{Protocol: "vmess", UUID: "0581b063-cc43-4719-bb46-736235a2c3e9", RuleIDs: []string{"other-rule"}}, Scope{RuleID: "this-rule", Network: "tcp"}},
		{"group_scope", "vmess", "profile_scope_denied", Profile{Protocol: "vmess", UUID: "0581b063-cc43-4719-bb46-736235a2c3e9", GroupIDs: []string{"other-group"}}, Scope{GroupIDs: []string{"this-group"}, Network: "tcp"}},
		{"target_scope", "vmess", "profile_scope_denied", Profile{Protocol: "vmess", UUID: "0581b063-cc43-4719-bb46-736235a2c3e9", Targets: []string{"127.0.0.1:4000"}}, Scope{Target: "127.0.0.1:5000", Network: "tcp"}},
		{"invalid_credential", "vmess", "profile_invalid", Profile{Protocol: "vmess", UUID: "sensitive-invalid-value"}, Scope{Network: "tcp"}},
		{"unsupported_network", "vmess", "profile_network_unavailable", Profile{Protocol: "vmess", UUID: "0581b063-cc43-4719-bb46-736235a2c3e9"}, Scope{Network: "udp"}},
		{"udp_association_not_observable", "socks5", "socks5_udp_association_not_observable", Profile{Protocol: "socks5", UDPMode: "associated", RuleIDs: []string{"udp"}, ControlRuleIDs: []string{"control"}, Targets: []string{"127.0.0.1:7000"}, RelayEndpoint: "127.0.0.1:6000"}, Scope{RuleID: "udp", Target: "127.0.0.1:7000", Network: "udp"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			profiles := Profiles{"optional-local": tc.profile}
			observe := blockedLayer(tc.protocol, "observe", "optional-local")
			// An unavailable observer must not prevent an unrelated enforced
			// policy from preparing, nor gain access to an out-of-scope key.
			layers := append([]contract.InboundPolicy{{BlockedApps: []string{"http"}}}, observe...)
			plan, err := Prepare(layers, profiles, tc.scope)
			if err != nil {
				t.Fatalf("optional detector stopped configuration: %v", err)
			}
			d := plan.Feed([]byte("ordinary-business"), true, tc.scope.Network)
			if d.Status != Unavailable || d.Reason != tc.reason || plan.Decision(d) != nil {
				t.Fatalf("optional prerequisite was not diagnostic: %+v", d)
			}
			if plan.HasAssociation() || plan.RequiresAssociation() || len(plan.AssociationBindings()) != 0 {
				t.Fatal("unavailable observer created association capability")
			}
			for _, candidate := range plan.candidates {
				if candidate.profile.protocol == "vmess" {
					t.Fatal("unavailable observer retained credential candidate")
				}
			}
			if tc.scope.Network == "tcp" {
				d = plan.Feed([]byte("GET / HTTP/1.1\r\nHost: example.test\r\n\r\n"), true, "tcp")
				if d.Protocol != "http" || !errors.Is(plan.Decision(d), ErrDenied) {
					t.Fatalf("observer weakened unrelated strict layer: %+v", d)
				}
			}
			strict := blockedLayer(tc.protocol, "strict", "optional-local")
			for _, ordered := range [][]contract.InboundPolicy{strict, append(append([]contract.InboundPolicy{}, observe...), strict...), append(append([]contract.InboundPolicy{}, strict...), observe...)} {
				if _, err = Prepare(ordered, profiles, tc.scope); err == nil {
					t.Fatal("same profile's strict use lost mandatory prerequisite")
				}
			}
		})
	}
}

func TestObserveAssociationRequiresRealProofAndStrictPreparation(t *testing.T) {
	profile := Profile{Protocol: "socks5", UDPMode: "associated", RuleIDs: []string{"udp"}, ControlRuleIDs: []string{"control"}, Targets: []string{"127.0.0.1:7000"}, RelayEndpoint: "127.0.0.1:6000"}
	scope := Scope{RuleID: "udp", Target: "127.0.0.1:7000", Network: "udp", SOCKSAssociation: true}
	profiles := Profiles{"optional-local": profile}
	plan, err := Prepare(blockedLayer("socks5", "observe", "optional-local"), profiles, scope)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.HasAssociation() || plan.RequiresAssociation() {
		t.Fatal("observe association incorrectly became required")
	}
	packet := []byte{0, 0, 0, 1, 127, 0, 0, 1, 0, 53, 'd'}
	d := plan.FeedAssociated(packet, true, "udp", association.Proof{})
	if d.Status != Unavailable || d.Reason != "socks5_udp_unassociated" || plan.Decision(d) != nil {
		t.Fatalf("capability fabricated proof or enforced observation: %+v", d)
	}
	strict, err := Prepare(blockedLayer("socks5", "strict", "optional-local"), profiles, scope)
	if err != nil || !strict.HasAssociation() || !strict.RequiresAssociation() {
		t.Fatalf("strict association did not require topology: %v", err)
	}
}

func TestObservePrerequisiteDiagnosticsAreBoundedAndSanitized(t *testing.T) {
	var layers []contract.InboundPolicy
	for i := 0; i < 1024; i++ {
		layers = append(layers, blockedLayer("vmess", "observe", "unavailable-local")...)
	}
	plan, err := Prepare(layers, nil, Scope{Network: "tcp"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.unavailable) > MaxCredentials || len(plan.unavailable) != 2 {
		t.Fatalf("unbounded or lost prerequisite diagnostics: %v", plan.unavailable)
	}
	for _, reason := range plan.unavailable {
		if strings.Contains(reason, "unavailable-local") {
			t.Fatal("local profile label leaked into payload diagnostic")
		}
	}
}
