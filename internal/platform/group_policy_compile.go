package platform

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/policy"
)

func activeAdvanced(g contract.Group) bool {
	return g.Advanced != nil && g.Advanced.PolicyVersion == contract.GroupPolicyVersion
}
func groupLayer(g contract.Group) contract.InboundPolicy {
	p := contract.InboundPolicy{GroupID: g.ID, BlockedApps: applicationBlocks(nil, g.BlockedProtocols)}
	if activeAdvanced(g) {
		a := g.Advanced
		p.AllowedHosts = append([]string{}, a.AllowedHost...)
		p.BlockedHosts = append([]string{}, a.BlockedHost...)
		p.BlockedPaths = append([]string{}, a.BlockedPath...)
		// The advanced field is authoritative; the top-level values mirror it.
		p.BlockedApps = applicationBlocks(a.BlockedProtocol, nil)
		p.TLSRequired = a.TLSInboundPolicy > 0
		p.RejectEmptySNI = a.TLSRejectEmptySNI
		p.Inspection = a.Inspection
	}
	return p
}
func loadPolicyGroups(ctx context.Context, tx *sql.Tx) (map[string]contract.Group, error) {
	rows, e := tx.QueryContext(ctx, "SELECT payload FROM cp_groups ORDER BY id")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := map[string]contract.Group{}
	for rows.Next() {
		var raw string
		var g contract.Group
		if e = rows.Scan(&raw); e != nil {
			return nil, e
		}
		if e = json.Unmarshal([]byte(raw), &g); e != nil {
			return nil, e
		}
		out[g.ID] = g
	}
	return out, rows.Err()
}
func compileGroupPolicy(rule *contract.Rule, role string, groups map[string]contract.Group, caps []string, profiles ...[]contract.InspectionProfileStatus) error {
	entry, ok := groups[rule.GroupID]
	if !ok {
		return errors.New("entry_group_missing")
	}
	path := []contract.Group{entry}
	if rule.ExitGroupID != "" {
		g, ok := groups[rule.ExitGroupID]
		if !ok {
			return errors.New("exit_group_missing")
		}
		path = append(path, g)
		for _, id := range g.ChainGroupIDs {
			hop, ok := groups[id]
			if !ok {
				return errors.New("chain_group_missing")
			}
			path = append(path, hop)
		}
	}
	enabled := false
	for _, g := range path {
		enabled = enabled || activeAdvanced(g)
		if policyDenied(g, *rule) {
			return errors.New("group_policy_denied")
		}
	}
	if activeAdvanced(entry) {
		if managedTLS(*rule) {
			v := ingressSettings(entry)
			if v == nil || !v.Enabled {
				return errors.New("shared_tls_ingress_disabled")
			}
			if !contains(caps, "shared-tls-ingress-v1") {
				return errors.New("required_capability_missing:shared-tls-ingress-v1")
			}
		}
		if entry.Advanced.UDPOverTCP && rule.Network == "udp" && rule.Transport == "quic" && rule.ExitGroupID == "" {
			return errors.New("udp_over_tcp_requires_configured_tcp_exit")
		}
		a := entry.Advanced
		if a.TLSInboundPolicy > 0 && rule.Network != "tcp" {
			return errors.New("tls_inbound_requires_tcp")
		}
		if a.TLSInboundPolicy == 2 && role != "admin" && !sharesPort(*rule) {
			return errors.New("tls_independent_port_admin_only")
		}
	}
	if !enabled {
		for _, app := range applicationBlocks(rule.BlockedProtocols, nil) {
			if newApplication(app) {
				return errors.New("inspection_policy_required:" + app)
			}
		}
		for _, g := range path {
			rule.BlockedProtocols = applicationBlocks(rule.BlockedProtocols, g.BlockedProtocols)
		}
		return nil
	}
	for _, cap := range []string{"group-policy-v2", "inbound-inspection-v1", "http-stream-filter-v1", "peer-address-policy-v1", "route-failover-v1"} {
		if !contains(caps, cap) {
			return errors.New("required_capability_missing:" + cap)
		}
	}
	p := &contract.EffectivePolicy{Version: contract.GroupPolicyVersion}
	ruleApps := applicationBlocks(rule.BlockedProtocols, nil)
	inspectionNode := contract.Node{Capabilities: caps}
	if len(profiles) > 0 {
		inspectionNode.InspectionProfiles = profiles[0]
	}
	rule.Business = nil
	for _, g := range path {
		layer := groupLayer(g)
		if layer.Inspection != nil && layer.Inspection.Business != nil {
			business := layer.Inspection.Business
			if rule.Business != nil && !reflect.DeepEqual(rule.Business, business) {
				return errors.New("business_adapter_policy_conflict")
			}
			copy := *business
			rule.Business = &copy
		}
		p.InboundLayers = append(p.InboundLayers, layer)
	}
	for _, layer := range p.InboundLayers {
		if layer.Inspection != nil && layer.Inspection.Business == nil && rule.Business != nil {
			copy := *layer.Inspection
			copy.Business = rule.Business
			layer.Inspection = &copy
		}
		if e := inspectionCapabilities(layer, inspectionNode, rule.Network); e != nil {
			return e
		}
	}
	for _, g := range path {
		rule.BlockedProtocols = applicationBlocks(rule.BlockedProtocols, g.BlockedProtocols)
	}
	if len(ruleApps) > 0 {
		layer := contract.InboundPolicy{GroupID: "rule", BlockedApps: ruleApps}
		if activeAdvanced(entry) {
			if entry.Advanced.Inspection != nil {
				copy := *entry.Advanced.Inspection
				copy.Mode = "strict"
				if copy.Business == nil {
					copy.Business = rule.Business
				}
				layer.Inspection = &copy
			}
		}
		// Group layers already carry their local detection plan. Only validate
		// additional rule restrictions with the entry's selected profiles.
		if e := inspectionCapabilities(layer, inspectionNode, rule.Network); e != nil {
			return e
		}
		p.InboundLayers = append(p.InboundLayers, layer)
	}
	if activeAdvanced(entry) {
		a := entry.Advanced
		p.Failover = &contract.FailoverPolicy{MaxFail: a.MaxFail, CooldownSec: a.FailTimeoutSec}
		peer := rule.ExitGroupID
		if len(path) > 2 {
			peer = path[2].ID
		}
		p.PreferIPv6 = contains(a.IPv6Group, peer) && peer != ""
	}
	if rule.Tunnel != nil && len(rule.Tunnel.Chain) > 0 && len(path) > 2 {
		t := *rule.Tunnel
		t.Chain = append([]contract.TunnelHop{}, t.Chain...)
		for i := range t.Chain {
			from := path[i+2]
			if i+3 < len(path) && activeAdvanced(from) {
				t.Chain[i].PreferIPv6 = contains(from.Advanced.IPv6Group, path[i+3].ID)
			}
		}
		rule.Tunnel = &t
	}
	if e := policy.Seal(p); e != nil {
		return e
	}
	rule.EffectivePolicy = p
	return nil
}

// Publish to all enrolled nodes for an advanced dependency change. This is a
// deliberately conservative dependency superset: it also covers chain groups,
// removed reverse relationships and authorization revocation, without JSON SQL
// dialect differences or missing a transitive dependency.
func (s *Server) publishGroupDependencies(ctx context.Context, tx *sql.Tx) error {
	_, e := tx.ExecContext(ctx, "UPDATE cp_nodes SET desired_version=desired_version+1")
	return e
}
