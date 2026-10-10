package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"slices"
	"strings"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/policy"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/policy/association"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/policy/detect"
)

// A local generation is not a credential or a remotely reported fingerprint.
// Rotating a control rule's forwarding authority invalidates UDP evidence;
// replenishing its finite accounting lease leaves the live association intact.
func associationGeneration(rule contract.Rule, profiles detect.Profiles, rules []contract.Rule) string {
	selected := map[string]bool{rule.ID: true}
	for _, p := range profiles {
		if p.Protocol == "socks5" && p.UDPMode == "associated" && slices.Contains(p.RuleIDs, rule.ID) && slices.Contains(p.Targets, rule.Target) {
			for _, id := range p.ControlRuleIDs {
				selected[id] = true
			}
		}
	}
	var scope []contract.Rule
	for _, r := range rules {
		if selected[r.ID] {
			r.Lease, r.StandbyLease, r.LeasePipeline, r.Version = nil, nil, false, 0
			scope = append(scope, r)
		}
	}
	slices.SortFunc(scope, func(a, b contract.Rule) int { return strings.Compare(a.ID, b.ID) })
	b, _ := json.Marshal(scope)
	sum := sha256.Sum256(append([]byte(profiles.Generation()), b...))
	return hex.EncodeToString(sum[:])
}

func prepareAssociations(rules []contract.Rule, plans map[string]*detect.Plan) ([]association.Binding, map[string][]association.Binding, error) {
	byID := map[string]contract.Rule{}
	for _, rule := range rules {
		if rule.Enabled {
			byID[rule.ID] = rule
		}
	}
	seen := map[association.Binding]bool{}
	var all []association.Binding
	controls := map[string][]association.Binding{}
	ids := make([]string, 0, len(plans))
	for id := range plans {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b string) int {
		if plans[a].RequiresAssociation() != plans[b].RequiresAssociation() {
			if plans[a].RequiresAssociation() {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	})
	for _, id := range ids {
		plan := plans[id]
		for _, binding := range plan.AssociationBindings() {
			if err := validateAssociationBinding(byID, id, binding); err != nil {
				if !plan.RequiresAssociation() {
					// Observation does not grant authority. Keep this profile in the
					// detector, but it can only report unassociated without proof.
					continue
				}
				return nil, nil, err
			}
			if !seen[binding] {
				if len(all) == association.MaxBindings && !plan.RequiresAssociation() {
					continue
				}
				seen[binding] = true
				all = append(all, binding)
				controls[binding.ControlRuleID] = append(controls[binding.ControlRuleID], binding)
			}
		}
	}
	// Validate before persistence and listener changes. Configure at commit is
	// then guaranteed to succeed, without revoking the running registry early.
	if err := association.New(0, 0).Configure(all); err != nil {
		return nil, nil, err
	}
	for id := range controls {
		slices.SortFunc(controls[id], func(a, b association.Binding) int {
			return strings.Compare(a.UDPRuleID+"|"+a.Generation, b.UDPRuleID+"|"+b.Generation)
		})
	}
	return all, controls, nil
}

func validateAssociationBinding(byID map[string]contract.Rule, id string, binding association.Binding) error {
	udp, ok := byID[id]
	control, controlOK := byID[binding.ControlRuleID]
	if !ok || udp.Network != "udp" || !controlOK || control.Network != "tcp" || udp.UserID != control.UserID {
		return errors.New("SOCKS association requires enabled same-owner control TCP and UDP rules on this Agent")
	}
	endpoint, err := netip.ParseAddrPort(udp.Listen)
	if err != nil || endpoint.Addr().IsUnspecified() || netip.AddrPortFrom(endpoint.Addr().Unmap(), endpoint.Port()) != binding.RelayEndpoint {
		return errors.New("SOCKS association relay endpoint must exactly match a numeric, non-wildcard UDP listener")
	}
	if control.SharedTLS != nil || control.Business != nil || control.ProxyProtocol != nil || len(control.Backends) != 0 || len(control.RouteCandidates) != 0 || len(udp.Backends) != 0 || len(udp.RouteCandidates) != 0 {
		return errors.New("SOCKS association requires fixed visible control and UDP forwarding without business adaptation, PROXY, shared TLS or failover")
	}
	tcpHost, _, tcpErr := net.SplitHostPort(control.Target)
	udpHost, _, udpErr := net.SplitHostPort(udp.Target)
	if tcpErr != nil || udpErr != nil || !strings.EqualFold(tcpHost, udpHost) {
		return errors.New("SOCKS association control and UDP targets must address the same explicitly configured relay host")
	}
	for _, layer := range ruleLayers(control) {
		if layer.Inspection != nil && layer.Inspection.Mode == "observe" {
			continue
		}
		for _, app := range layer.BlockedApps {
			if app == "socks" || app == "socks5" || app == "app:socks" || app == "app:socks5" {
				return errors.New("SOCKS association control rule must permit SOCKS5 negotiation; its strict control policy would prevent association")
			}
		}
	}
	return nil
}

func associatedDatagramDecision(payload []byte, layers []contract.InboundPolicy, plan *detect.Plan, proof association.Proof) (detect.Detection, bool) {
	d := plan.FeedAssociated(payload, true, "udp", proof)
	if plan.Decision(d) != nil {
		return d, true
	}
	if d.Status == detect.Match && d.Evidence == detect.Authenticated {
		return d, false
	}
	// The associated detector owns SOCKS5 UDP evidence. A coincidental RFC1928
	// header without a live control association must not invoke legacy blocking.
	for _, layer := range layers {
		if layer.Inspection != nil && layer.Inspection.Mode == "observe" {
			continue
		}
		var other []string
		for _, app := range layer.BlockedApps {
			if app != "socks" && app != "socks5" && app != "app:socks" && app != "app:socks5" {
				other = append(other, app)
			}
		}
		if policy.BlockedDatagram(payload, other) {
			return d, true
		}
	}
	return d, false
}
