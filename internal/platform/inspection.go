package platform

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/policy"
)

func newApplication(app string) bool {
	return app != "http" && app != "socks"
}

// Node reports contain local labels and supported scope, never credentials.
func validateInspectionProfiles(profiles []contract.InspectionProfileStatus) error {
	// Detection profiles (256) and business identities (128) share the report.
	if len(profiles) > 384 {
		return errors.New("too many inspection profiles")
	}
	seen := map[string]bool{}
	for _, p := range profiles {
		if !validInspectionLabel(p.Label) || !slices.Contains([]string{"", "profile_invalid", "business_certificate_unavailable", "business_ca_unavailable"}, p.Reason) || len(p.Variants) > 16 || len(p.Networks) > 2 {
			return errors.New("invalid inspection profile status")
		}
		app, ok := policy.NormalizeApplication(p.Protocol)
		if !ok && p.Protocol != "business-tls" || ok && app != p.Protocol {
			return errors.New("invalid inspection profile protocol")
		}
		key := p.Label + "/" + p.Protocol
		if seen[key] {
			return errors.New("duplicate inspection profile status")
		}
		seen[key] = true
		for _, v := range p.Variants {
			if len(v) == 0 || len(v) > 64 || strings.ContainsAny(v, "\r\n") {
				return errors.New("invalid inspection profile variant")
			}
		}
		for _, n := range p.Networks {
			if n != "tcp" && n != "udp" {
				return errors.New("invalid inspection profile network")
			}
		}
	}
	return nil
}

func validInspectionLabel(label string) bool {
	if len(label) == 0 || len(label) > 64 {
		return false
	}
	for _, c := range label {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func routeBusiness(rule contract.Rule, groups map[string]contract.Group) (*contract.BusinessInbound, error) {
	var business *contract.BusinessInbound
	ids := []string{rule.GroupID, rule.ExitGroupID}
	ids = append(ids, groups[rule.ExitGroupID].ChainGroupIDs...)
	for _, id := range ids {
		g := groups[id]
		if activeAdvanced(g) && g.Advanced.Inspection != nil && g.Advanced.Inspection.Business != nil {
			candidate := g.Advanced.Inspection.Business
			if business != nil && !reflect.DeepEqual(business, candidate) {
				return nil, errors.New("business_adapter_policy_conflict")
			}
			business = candidate
		}
	}
	if business == nil {
		business = rule.Business
	}
	return business, nil
}

func stagedHopRequired(rule contract.Rule, target contract.Group, business *contract.BusinessInbound) bool {
	// Entry inspection can precheck a legacy external tunnel. Only this hop's
	// own new inspector or authenticated business adapter needs the v4 gate.
	return rule.Network == "tcp" && (business != nil || activeAdvanced(target) && target.Advanced.Inspection != nil)
}

func stagedCapabilities(nodes ...contract.Node) error {
	for _, n := range nodes {
		if !contains(n.Capabilities, "staged-inspection-v1") {
			return errors.New("required_capability_missing:staged-inspection-v1")
		}
	}
	return nil
}

func validateInspectionRuntime(v contract.RuleRuntimeStatus) error {
	if !contains([]string{"", "entry", "exit", "chain", "reverse"}, v.InspectionLocation) || !contains([]string{"", "structural", "authenticated", "legacy_auth", "probable"}, v.Evidence) || !contains([]string{"", "raw", "tls-plaintext", "ws-payload", "opaque"}, v.Visibility) {
		return errors.New("invalid inspection status enum")
	}
	if v.DetectedProtocol != "" {
		if app, ok := policy.NormalizeApplication(v.DetectedProtocol); !ok || app != v.DetectedProtocol {
			return errors.New("invalid detected protocol")
		}
	}
	if len(v.DetectedVariant) > 128 || len(v.InspectionReason) > 128 || strings.ContainsAny(v.DetectedVariant+v.InspectionReason, "\x00\r\n") {
		return errors.New("invalid inspection status")
	}
	return nil
}

func profileAvailable(n contract.Node, labels []string, app, network string, variants ...string) bool {
	for _, p := range n.InspectionProfiles {
		if p.Ready && p.Protocol == app && slices.Contains(labels, p.Label) && slices.Contains(p.Networks, network) {
			if len(variants) == 0 || slices.ContainsFunc(p.Variants, func(v string) bool { return slices.Contains(variants, v) }) {
				return true
			}
		}
	}
	return false
}

func credentialProfileSupported(n contract.Node, labels []string, app, network string) bool {
	variants := []string{}
	switch app {
	case "shadowsocks":
		if contains(n.Capabilities, "inspect:ss-aead2017-v1") {
			variants = append(variants, "aead2017")
		}
		if contains(n.Capabilities, "inspect:ss-aead2022-v1") {
			variants = append(variants, "aead2022")
		}
		if contains(n.Capabilities, "inspect:ss-sip023-v1") {
			variants = append(variants, "aead2022-sip023")
		}
	case "vmess":
		if contains(n.Capabilities, "inspect:vmess-aead-v1") {
			variants = append(variants, "aead")
		}
		if contains(n.Capabilities, "inspect:vmess-legacy-v1") {
			variants = append(variants, "legacy")
		}
	case "trojan":
		if contains(n.Capabilities, "inspect:trojan-v1") {
			variants = append(variants, "trojan-sha224")
		}
	}
	return len(variants) > 0 && profileAvailable(n, labels, app, network, variants...)
}

func inspectionCapabilities(p contract.InboundPolicy, n contract.Node, network string) error {
	return inspectionCapabilitiesAt(p, n, network, true)
}

func inspectionCapabilitiesAt(p contract.InboundPolicy, n contract.Node, network string, incoming bool) error {
	if p.Inspection == nil {
		for _, app := range p.BlockedApps {
			if newApplication(app) {
				return errors.New("inspection_policy_required:" + app)
			}
		}
		return nil
	}
	if !contains(n.Capabilities, "application-inspection-v1") {
		return errors.New("required_capability_missing:application-inspection-v1")
	}
	strict := p.Inspection.Mode != "observe"
	for _, app := range p.BlockedApps {
		caps := []string{}
		switch app {
		case "http":
			caps = []string{"block:http"}
		case "socks", "socks4", "socks5":
			if network == "udp" {
				if app == "socks4" {
					if strict {
						return errors.New("unsupported_detector_network:socks4:udp")
					}
					continue
				}
				caps = []string{"inspect:socks5-udp-associated-v1"}
				if contains(n.Capabilities, "inspect:socks5-udp-structural-v1") && profileAvailable(n, p.Inspection.Profiles, "socks5", "udp", "socks5-udp-structural") {
					caps = []string{"inspect:socks5-udp-structural-v1"}
				} else if strict {
					if !incoming {
						return errors.New("inspection_not_observable:socks5:udp:control_association")
					}
					if contains(n.Capabilities, caps[0]) && !profileAvailable(n, p.Inspection.Profiles, "socks5", "udp", "socks5-udp-associated") {
						return errors.New("inspection_profile_not_ready:socks5:udp:associated")
					}
				}
			} else {
				if app != "socks5" {
					caps = append(caps, "inspect:socks4-tcp-v1")
				}
				if app != "socks4" {
					caps = append(caps, "inspect:socks5-tcp-v1")
				}
			}
		case "shadowsocks":
			if !contains(n.Capabilities, "inspect:ss-aead2017-v1") && !contains(n.Capabilities, "inspect:ss-aead2022-v1") {
				caps = []string{"inspect:ss-aead2017-v1|inspect:ss-aead2022-v1"}
			}
		case "vmess", "trojan":
			if network != "tcp" {
				if strict {
					return fmt.Errorf("unsupported_detector_network:%s:%s", app, network)
				}
				continue
			}
			if app == "vmess" {
				caps = []string{"inspect:vmess-aead-v1"}
			} else {
				caps = []string{"inspect:trojan-v1"}
			}
		}
		if strict {
			for _, cap := range caps {
				if !contains(n.Capabilities, cap) {
					return errors.New("required_capability_missing:" + cap)
				}
			}
			if app == "shadowsocks" || app == "trojan" || app == "vmess" {
				if !profileAvailable(n, p.Inspection.Profiles, app, network) {
					return fmt.Errorf("inspection_profile_not_ready:%s:%s", app, network)
				}
				if !credentialProfileSupported(n, p.Inspection.Profiles, app, network) {
					return fmt.Errorf("inspection_profile_scope_not_supported:%s:%s", app, network)
				}
			}
			if app == "trojan" && (p.Inspection.Business == nil || p.Inspection.Business.TLSProfile == "") {
				return errors.New("business_tls_termination_required:trojan")
			}
		}
	}
	if b := p.Inspection.Business; b != nil {
		if network != "tcp" {
			return errors.New("business_adapter_requires_tcp")
		}
		if b.TLSProfile != "" && incoming {
			if !contains(n.Capabilities, "business-tls-termination-v1") {
				return errors.New("required_capability_missing:business-tls-termination-v1")
			}
			if !profileAvailable(n, []string{b.TLSProfile}, "business-tls", "tcp", "server") {
				return errors.New("business_tls_profile_not_ready:" + b.TLSProfile)
			}
		}
		if b.UpstreamTLSProfile != "" {
			if !contains(n.Capabilities, "business-tls-termination-v1") {
				return errors.New("required_capability_missing:business-tls-termination-v1")
			}
			if !profileAvailable(n, []string{b.UpstreamTLSProfile}, "business-tls", "tcp", "upstream") {
				return errors.New("business_upstream_tls_profile_not_ready:" + b.UpstreamTLSProfile)
			}
		}
		if b.WebSocket && !contains(n.Capabilities, "staged-inspection-v1") {
			return errors.New("required_capability_missing:staged-inspection-v1")
		}
	}
	return nil
}
