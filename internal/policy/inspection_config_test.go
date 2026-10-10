package policy

import (
	"encoding/json"
	"testing"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

func TestInspectionConfigCompatibility(t *testing.T) {
	for _, raw := range []string{"socks", "app:socks5", "Shadowsocks", "trojan", "vmess", "socks4"} {
		if _, ok := NormalizeApplication(raw); !ok {
			t.Fatalf("unsupported %q", raw)
		}
	}
	for _, raw := range []string{"quic", "ssh", "app:app:socks5"} {
		if _, ok := NormalizeApplication(raw); ok {
			t.Fatalf("accepted %q", raw)
		}
	}
	var a contract.GroupAdvanced
	if err := json.Unmarshal([]byte(`{"policy_version":2,"blocked_protocol":["socks"]}`), &a); err != nil {
		t.Fatal(err)
	}
	if a.Inspection != nil || contract.GroupPolicyVersion != 2 {
		t.Fatal("old policy changed")
	}
	for _, raw := range []string{
		`{"inspection":{"version":1,"password":"secret"}}`,
		`{"inspection":{"version":1,"key":"secret"}}`,
	} {
		if json.Unmarshal([]byte(raw), &a) == nil {
			t.Fatal("credential accepted into control-plane contract")
		}
	}
}

func TestInspectionSealIncludesSemanticFields(t *testing.T) {
	makePolicy := func() contract.EffectivePolicy {
		return contract.EffectivePolicy{Version: 2, InboundLayers: []contract.InboundPolicy{{BlockedApps: []string{"app:socks5"}, Inspection: &contract.InspectionPolicy{Version: 1, Profiles: []string{"local"}}}}}
	}
	p := makePolicy()
	if err := Seal(&p); err != nil {
		t.Fatal(err)
	}
	if p.InboundLayers[0].BlockedApps[0] != "socks5" || p.InboundLayers[0].Inspection.Unknown != "allow" {
		t.Fatal("normalization failed")
	}
	hash := p.Hash
	p.InboundLayers[0].Inspection.Unknown = "deny"
	if err := Seal(&p); err != nil || p.Hash == hash {
		t.Fatal("unknown policy omitted from hash", err)
	}
	for _, invalid := range []*contract.InspectionPolicy{
		{Version: 2}, {Version: 1, Mode: "silent"}, {Version: 1, Unknown: "block-proxy"}, {Version: 1, Profiles: []string{"../secret"}},
		{Version: 1, Business: &contract.BusinessInbound{TLSProfile: "C:/private.pem"}},
	} {
		p := makePolicy()
		p.InboundLayers[0].Inspection = invalid
		if Seal(&p) == nil {
			t.Fatalf("invalid policy accepted: %+v", invalid)
		}
	}
}
