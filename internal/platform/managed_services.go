package platform

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/policy"
)

type managedTopology struct {
	exits  []contract.Exit
	nodes  map[string]contract.Node
	groups map[string]contract.Group
}

func (s *Server) managedTopology(ctx context.Context, tx *sql.Tx) (managedTopology, error) {
	t := managedTopology{nodes: map[string]contract.Node{}}
	g, e := loadPolicyGroups(ctx, tx)
	if e != nil {
		return t, e
	}
	t.groups = g
	rows, e := tx.QueryContext(ctx, "SELECT payload,last_seen,desired_version,applied_version FROM cp_nodes ORDER BY id")
	if e != nil {
		return t, e
	}
	for rows.Next() {
		var raw string
		var seen, d, a int64
		if e = rows.Scan(&raw, &seen, &d, &a); e != nil {
			rows.Close()
			return t, e
		}
		var n contract.Node
		if e = json.Unmarshal([]byte(raw), &n); e != nil {
			rows.Close()
			return t, e
		}
		at := time.Unix(seen, 0)
		n.LastSeen = &at
		n.DesiredVersion = d
		n.AppliedVersion = a
		t.nodes[n.ID] = n
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return t, e
	}
	rows, e = tx.QueryContext(ctx, s.q("SELECT e.payload FROM cp_exits e JOIN cp_node_groups ng ON ng.node_id=e.node_id AND ng.group_id=e.group_id WHERE NOT EXISTS(SELECT 1 FROM cp_node_operations o WHERE o.node_id=e.node_id AND o.kind='uninstall' AND (o.status IN ('running','succeeded') OR (o.status='pending' AND o.expires_at>?))) ORDER BY e.id"), time.Now().Unix())
	if e != nil {
		return t, e
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		var x contract.Exit
		if e = rows.Scan(&raw); e != nil {
			return t, e
		}
		if e = json.Unmarshal([]byte(raw), &x); e != nil {
			return t, e
		}
		t.exits = append(t.exits, x)
	}
	return t, rows.Err()
}
func serviceSecret(secret, scope string) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(scope))
	return hex.EncodeToString(h.Sum(nil))
}
func relationID(e, h contract.Exit, g contract.Group) string {
	return "reverse-" + e.ID + "-" + serviceSecret(e.Tunnel.Token, fmt.Sprintf("%s/%s/%d/%d/%d", e.ID, h.ID, g.Version, e.Version, h.Version))[:16]
}
func relationGrants(e, h contract.Exit, g contract.Group, rule contract.Rule) contract.ServiceGrant {
	i := relationID(e, h, g)
	grant := contract.ServiceGrant{Identity: i, Token: serviceSecret(e.Tunnel.Token, "carrier/"+i), StreamToken: serviceSecret(e.Tunnel.Token, "stream/"+i+"/"+rule.ID)}
	grant.Targets = ruleTargets(rule)
	return grant
}
func ruleTargets(r contract.Rule) []contract.ServiceTarget {
	out := []contract.ServiceTarget{{RuleID: r.ID, Network: r.Network, Target: r.Target}}
	for _, b := range r.Backends {
		if !b.Disabled {
			out = append(out, contract.ServiceTarget{RuleID: r.ID, Network: r.Network, Target: b.Target})
		}
	}
	return out
}
func readyService(n contract.Node, id string) bool {
	if n.LastSeen == nil || time.Since(*n.LastSeen) > 90*time.Second {
		return false
	}
	for _, s := range n.Services {
		if s.ID == id {
			return s.Ready
		}
	}
	return false
}
func (t managedTopology) reverseHub(rule contract.Rule, e contract.Exit) (contract.Exit, bool) {
	g := t.groups[e.GroupID]
	if !e.Managed || !activeAdvanced(g) || len(g.Advanced.ReverseGroup) == 0 {
		return contract.Exit{}, false
	}
	for _, h := range t.exits {
		if h.Managed && h.ReverseHub && h.Enabled && h.NodeID == rule.NodeID && h.GroupID == rule.GroupID && contains(g.Advanced.ReverseGroup, h.GroupID) {
			return h, true
		}
	}
	return contract.Exit{}, false
}
func (t managedTopology) route(rule contract.Rule, e contract.Exit, ready bool) (contract.Exit, error) {
	g := t.groups[e.GroupID]
	if !e.Managed {
		if activeAdvanced(g) {
			return e, errors.New("advanced_exit_requires_managed_service")
		}
		return e, nil
	}
	if err := serviceCapabilities(t.nodes[e.NodeID], g); err != nil {
		return e, err
	}
	layer := groupLayer(g)
	e.Tunnel.Inspect = rule.Network == "tcp" && policy.NeedsInspect([]contract.InboundPolicy{layer})
	if activeAdvanced(g) && len(g.Advanced.ReverseGroup) > 0 {
		h, ok := t.reverseHub(rule, e)
		if !ok {
			return e, errors.New("reverse_hub_missing")
		}
		tc, err := t.hubTLS(g, h)
		if err != nil {
			return e, err
		}
		if tc.Enabled != nil && !*tc.Enabled {
			return e, errors.New("reverse_tls_disabled")
		}
		if err := serviceCapabilities(t.nodes[h.NodeID], g); err != nil {
			return e, err
		}
		carrier := g.Advanced.Protocol
		if carrier == "" {
			carrier = "tls"
		}
		if h.Transport != carrier {
			return e, errors.New("reverse_hub_carrier_mismatch")
		}
		i := relationID(e, h, g)
		if ready && (!readyService(t.nodes[h.NodeID], i) || !readyService(t.nodes[e.NodeID], i)) {
			return e, errors.New("reverse_waiting_for_peer")
		}
		grant := relationGrants(e, h, g, rule)
		e.Transport = carrier
		e.Tunnel = contract.Tunnel{ServiceID: h.ID, Endpoint: h.Tunnel.Endpoint, ServerName: tc.ServerName, Token: grant.StreamToken, Reverse: i, Inspect: e.Tunnel.Inspect}
		if e.Tunnel.ServerName == "" {
			e.Tunnel.ServerName = h.Tunnel.ServerName
		}
		e.UDP = nil
	} else {
		if ready && !readyService(t.nodes[e.NodeID], e.ID) {
			return e, errors.New("managed_exit_waiting_for_ready")
		}
		e.Tunnel.Token = serviceSecret(e.Tunnel.Token, "rule/"+rule.ID)
	}
	return e, nil
}

func (t managedTopology) hubTLS(g contract.Group, h contract.Exit) (contract.ReverseTLS, error) {
	normalize := func(group contract.Group) (contract.ReverseTLS, error) {
		v, err := policy.ParseTLS(group.Advanced.TLS)
		if v.ServerName == "" {
			v.ServerName = h.Tunnel.ServerName
		}
		return v, err
	}
	tc, err := normalize(g)
	if err != nil {
		return tc, err
	}
	// One physical listener has one TLS policy. Refuse conflicting groups
	// instead of silently using whichever exit happened to compile first.
	for _, x := range t.exits {
		other := t.groups[x.GroupID]
		if !x.Managed || !x.Enabled || x.ReverseHub || !activeAdvanced(other) || !contains(other.Advanced.ReverseGroup, h.GroupID) {
			continue
		}
		candidate, err := normalize(other)
		if err != nil || candidate.Enabled != nil && !*candidate.Enabled {
			continue
		}
		carrier := other.Advanced.Protocol
		if carrier == "" {
			carrier = "tls"
		}
		if carrier == h.Transport && !reflect.DeepEqual(candidate, tc) {
			return tc, errors.New("reverse_hub_tls_policy_conflict")
		}
	}
	return tc, nil
}

func serviceCapabilities(n contract.Node, g contract.Group) error {
	caps := []string{"managed-services-v1"}
	if activeAdvanced(g) {
		caps = append(caps, "group-policy-v2", "inbound-inspection-v1", "http-stream-filter-v1", "peer-address-policy-v1", "route-failover-v1")
		if len(g.Advanced.ReverseGroup) > 0 {
			caps = append(caps, "reverse-group-v1")
			if g.Advanced.Protocol == "tls_simple" {
				caps = append(caps, "reverse:tls_simple")
			}
		}
	}
	for _, cap := range caps {
		if !contains(n.Capabilities, cap) {
			return errors.New("required_capability_missing:" + cap)
		}
	}
	return nil
}

func (s *Server) compileManagedServices(ctx context.Context, tx *sql.Tx, cfg *contract.Config, top managedTopology) error {
	// Scope grants to funded, enabled, authorized business rules. Merely knowing
	// a relationship credential never authorizes arbitrary targets or tenants.
	rows, e := tx.QueryContext(ctx, s.q(`SELECT r.payload,u.role,n.payload FROM cp_rules r JOIN cp_users u ON u.id=r.user_id JOIN cp_nodes n ON n.id=r.node_id WHERE r.deleted=0 AND u.disabled=0 AND (u.role='admin' OR EXISTS(SELECT 1 FROM cp_group_identity_groups gig WHERE gig.group_id=r.group_id AND gig.identity_group_id=u.identity_group_id)) ORDER BY r.id`))
	if e != nil {
		return e
	}
	rules := []contract.Rule{}
	roles := map[string]string{}
	for rows.Next() {
		var raw, role, np string
		if e = rows.Scan(&raw, &role, &np); e != nil {
			rows.Close()
			return e
		}
		var r contract.Rule
		var n contract.Node
		if e = json.Unmarshal([]byte(raw), &r); e != nil {
			rows.Close()
			return e
		}
		if e = json.Unmarshal([]byte(np), &n); e != nil {
			rows.Close()
			return e
		}
		if !r.Enabled || r.Lease == nil || !r.Lease.ExpiresAt.After(time.Now()) || r.ExitGroupID == "" {
			continue
		}
		entry := top.groups[r.GroupID]
		if entryPolicyDenied(entry, r) {
			continue
		}
		if compileGroupPolicy(&r, role, top.groups, n.Capabilities) != nil {
			continue
		}
		rules = append(rules, r)
		roles[r.UserID] = role
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	authorizedRules := rules[:0]
	for _, r := range rules {
		g := top.groups[r.ExitGroupID]
		if g.OwnerID != "" && g.OwnerID != r.UserID {
			continue
		}
		authorized := true
		if roles[r.UserID] != "admin" {
			for _, gid := range append([]string{r.ExitGroupID}, g.ChainGroupIDs...) {
				ok, err := s.groupAuthorized(ctx, tx, gid, r.UserID)
				if err != nil {
					return err
				}
				if !ok {
					authorized = false
					break
				}
			}
		}
		if authorized {
			authorizedRules = append(authorizedRules, r)
			if r.Lease.ExpiresAt.Before(cfg.ValidUntil) {
				cfg.ValidUntil = r.Lease.ExpiresAt
			}
		}
	}
	rules = authorizedRules
	services := map[string]*contract.ServiceConfig{}
	for _, x := range top.exits {
		if !x.Managed || !x.Enabled || x.ReverseHub {
			continue
		}
		g := top.groups[x.GroupID]
		if serviceCapabilities(top.nodes[x.NodeID], g) != nil {
			continue
		}
		layer := groupLayer(g)
		p := &contract.EffectivePolicy{Version: contract.GroupPolicyVersion, InboundLayers: []contract.InboundPolicy{layer}}
		if activeAdvanced(g) {
			p.Failover = &contract.FailoverPolicy{MaxFail: g.Advanced.MaxFail, CooldownSec: g.Advanced.FailTimeoutSec}
		}
		if e := policy.Seal(p); e != nil {
			return e
		}
		if activeAdvanced(g) && len(g.Advanced.ReverseGroup) > 0 {
			tc, e := policy.ParseTLS(g.Advanced.TLS)
			if e != nil {
				return e
			}
			if tc.Enabled != nil && !*tc.Enabled {
				continue
			}
			carrier := g.Advanced.Protocol
			if carrier == "" {
				carrier = "tls"
			}
			for _, h := range top.exits {
				if !h.Managed || !h.ReverseHub || !h.Enabled || !contains(g.Advanced.ReverseGroup, h.GroupID) || h.Transport != carrier {
					continue
				}
				if serviceCapabilities(top.nodes[h.NodeID], g) != nil {
					continue
				}
				tc, e := top.hubTLS(g, h)
				if e != nil {
					continue
				}
				identity := relationID(x, h, g)
				grants := []contract.ServiceGrant{}
				for _, r := range rules {
					if r.ExitGroupID != x.GroupID || r.GroupID != h.GroupID || r.NodeID != h.NodeID || r.ExitID != "" && r.ExitID != "auto" && r.ExitID != x.ID {
						continue
					}
					if r.Network == "udp" && g.Advanced.DisableUDP {
						continue
					}
					grants = append(grants, relationGrants(x, h, g, r))
				}
				// The relationship itself is valid before business rules are ready.
				if len(grants) == 0 {
					grants = []contract.ServiceGrant{{Identity: identity, Token: serviceSecret(x.Tunnel.Token, "carrier/"+identity), StreamToken: serviceSecret(x.Tunnel.Token, "empty/"+identity)}}
				}
				if h.NodeID == cfg.NodeID {
					hub := services[h.ID]
					if hub == nil {
						hub = &contract.ServiceConfig{ID: h.ID, Kind: "hub", GroupID: h.GroupID, Listen: h.Listen, Endpoint: h.Tunnel.Endpoint, Transport: carrier, Token: h.Tunnel.Token, Profile: h.LocalProfile, TLS: tc}
						services[h.ID] = hub
					}
					hub.Grants = append(hub.Grants, grants...)
				}
				if x.NodeID == cfg.NodeID {
					if !readyService(top.nodes[h.NodeID], h.ID) {
						continue
					}
					if !contains(top.nodes[h.NodeID].Capabilities, "reverse-group-v1") {
						continue
					}
					serverName := tc.ServerName
					if serverName == "" {
						serverName = h.Tunnel.ServerName
					}
					tc.ServerName = serverName
					services[identity] = &contract.ServiceConfig{ID: identity, Kind: "reverse", GroupID: x.GroupID, Endpoint: h.Tunnel.Endpoint, Transport: carrier, PreferIPv6: contains(g.Advanced.IPv6Group, h.GroupID), Identity: identity, Token: grants[0].Token, Profile: x.LocalProfile, TLS: tc, Grants: grants, Policy: p}
				}
			}
		} else if x.NodeID == cfg.NodeID {
			v := &contract.ServiceConfig{ID: x.ID, Kind: "exit", GroupID: x.GroupID, Listen: x.Listen, Transport: x.Transport, Token: x.Tunnel.Token, Profile: x.LocalProfile, Policy: p, UDP: x.UDP}
			if g.Advanced != nil && g.Advanced.DisableUDP {
				v.UDP = nil
			}
			for _, r := range rules {
				if r.ExitGroupID != x.GroupID && !contains(top.groups[r.ExitGroupID].ChainGroupIDs, x.GroupID) || r.NodeID == x.NodeID || r.ExitID != "" && r.ExitID != "auto" && r.ExitID != x.ID {
					continue
				}
				if policyDenied(g, r) {
					continue
				}
				if contains(top.groups[r.ExitGroupID].ChainGroupIDs, x.GroupID) && r.Tunnel != nil {
					hops := append([]contract.TunnelHop{{Transport: r.Transport, Endpoint: r.Tunnel.Endpoint, ServerName: r.Tunnel.ServerName, Token: r.Tunnel.Token, Inspect: r.Tunnel.Inspect}}, r.Tunnel.Chain...)
					for i, hop := range hops {
						if hop.Endpoint == x.Tunnel.Endpoint && i+1 < len(hops) {
							next := hops[i+1]
							chainGroups := top.groups[r.ExitGroupID].ChainGroupIDs
							if i+1 < len(chainGroups) && activeAdvanced(g) {
								next.PreferIPv6 = contains(g.Advanced.IPv6Group, chainGroups[i+1])
							}
							v.NextHops = append(v.NextHops, next)
						}
					}
				}
				v.Grants = append(v.Grants, contract.ServiceGrant{Identity: r.ID, StreamToken: serviceSecret(x.Tunnel.Token, "rule/"+r.ID), Targets: ruleTargets(r)})
			}
			services[x.ID] = v
		}
	}
	ids := []string{}
	for id := range services {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		cfg.Services = append(cfg.Services, *services[id])
	}
	if len(cfg.Services) > 0 {
		if until := time.Now().UTC().Add(5 * time.Minute); until.Before(cfg.ValidUntil) {
			cfg.ValidUntil = until
		}
	}
	return nil
}

// Candidate compilation is a read-only operation inside the config snapshot;
// do not reuse the FOR UPDATE authorization resolver used by rule writes.
func compileRouteCandidates(rule *contract.Rule, top managedTopology, caps []string) error {
	groups := top.groups
	if rule.EffectivePolicy == nil || rule.ExitGroupID == "" || len(rule.Backends) > 0 || groups[rule.ExitGroupID].Type == contract.GroupChainExit {
		return nil
	}
	group, entry := groups[rule.ExitGroupID], groups[rule.GroupID]
	multiplier, err := effectiveMultiplier(entry.Multiplier, group.Multiplier)
	if err != nil {
		return err
	}
	if multiplier != rule.BillingMultiplier {
		return errors.New("candidate billing policy changed")
	}
	for _, x := range top.exits {
		if len(rule.RouteCandidates) == 16 {
			break
		}
		n := top.nodes[x.NodeID]
		if x.GroupID != rule.ExitGroupID || x.ReverseHub || !x.Enabled || x.NodeID == rule.NodeID || n.LastSeen == nil || time.Since(*n.LastSeen) > 90*time.Second || rule.ExitID != "" && rule.ExitID != "auto" && x.ID != rule.ExitID {
			continue
		}
		e, err := top.route(*rule, x, true)
		if err != nil {
			continue
		}
		forced := entry.Advanced != nil && entry.Advanced.UDPOverTCP || group.Advanced != nil && group.Advanced.UDPOverTCP
		if rule.Network == "udp" && e.UDP != nil && !forced {
			if contains(caps, "udp-datagram-v1") && contains(n.Capabilities, "udp-datagram-v1") {
				token := e.UDP.Token
				if e.Managed {
					token = e.Tunnel.Token
				}
				e.Transport = "quic"
				e.Tunnel = contract.Tunnel{Endpoint: e.UDP.Endpoint, ServerName: e.UDP.ServerName, Token: token}
			} else if !e.UDP.AllowTCPFallback {
				continue
			}
		}
		v := *rule
		v.Transport = e.Transport
		v.Tunnel = &e.Tunnel
		if policyDenied(group, v) || entryPolicyDenied(entry, v) {
			continue
		}
		rule.RouteCandidates = append(rule.RouteCandidates, contract.RouteCandidate{ID: e.ID, ExitGroupID: rule.ExitGroupID, Target: rule.Target, Transport: e.Transport, Tunnel: &e.Tunnel, Weight: e.Weight, EffectivePolicy: rule.EffectivePolicy, BillingMultiplier: multiplier})
	}
	return nil
}
