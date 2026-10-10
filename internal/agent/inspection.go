package agent

import (
	"crypto/tls"
	"errors"
	"sort"
	"strings"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/policy"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/policy/detect"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/tunnel"
)

func ruleLayers(v contract.Rule) []contract.InboundPolicy {
	var layers []contract.InboundPolicy
	if v.EffectivePolicy != nil {
		layers = append(layers, v.EffectivePolicy.InboundLayers...)
	}
	covered := map[string]bool{}
	apps := func(value string) []string {
		value = strings.TrimPrefix(value, "app:")
		if value == "socks" {
			return []string{"socks4", "socks5"}
		}
		return []string{value}
	}
	for _, layer := range layers {
		for _, value := range layer.BlockedApps {
			for _, app := range apps(value) {
				covered[app] = true
			}
		}
	}
	var extra []string
	for _, value := range v.BlockedProtocols {
		for _, app := range apps(value) {
			if !covered[app] {
				extra = append(extra, app)
				covered[app] = true
			}
		}
	}
	if len(extra) > 0 {
		layers = append(layers, contract.InboundPolicy{GroupID: "rule", BlockedApps: extra})
	}
	return layers
}

func inspectionVisibility(b *contract.BusinessInbound) string {
	if b != nil && b.WebSocket {
		return "ws-payload"
	}
	if b != nil && b.TLSProfile != "" {
		return "tls-plaintext"
	}
	return "raw"
}

func prepareRuleInspection(v contract.Rule, profiles detect.Profiles, businessProfiles map[string]BusinessProfile, generations ...string) (*detect.Plan, *preparedBusiness, error) {
	layers := ruleLayers(v)
	groupIDs := []string{v.GroupID}
	if v.ExitGroupID != "" {
		groupIDs = append(groupIDs, v.ExitGroupID)
	}
	for _, layer := range layers {
		if layer.GroupID != "" && layer.GroupID != "rule" {
			groupIDs = append(groupIDs, layer.GroupID)
		}
	}
	generation := ""
	if len(generations) > 0 {
		generation = generations[0]
	}
	plan, err := detect.Prepare(layers, profiles, detect.Scope{RuleID: v.ID, Target: v.Target, GroupIDs: groupIDs, Visibility: inspectionVisibility(v.Business), Network: v.Network, SOCKSAssociation: v.Network == "udp", Generation: generation})
	if err != nil {
		return nil, nil, err
	}
	if v.Business != nil && v.Network != "tcp" {
		return nil, nil, errors.New("business adaptation supports TCP streams only")
	}
	if v.Business != nil && v.Business.TLSProfile != "" && (v.Transport == "direct-tls" || v.Transport == "secure-direct") {
		return nil, nil, errors.New("business TLS adaptation conflicts with transport TLS")
	}
	if v.Business != nil && v.Business.UpstreamTLSProfile != "" && v.Transport == "direct-tls" {
		return nil, nil, errors.New("duplicate upstream business TLS")
	}
	business, err := prepareBusiness(v.Business, businessProfiles, v.Listen, v.Target, true)
	if err != nil {
		return nil, nil, err
	}
	return plan, business, nil
}

func (r *Runtime) inspectionInputs() (detect.Profiles, map[string]BusinessProfile, error) {
	p := r.InspectionProfiles
	b := r.BusinessProfiles
	var err error
	if r.InspectionProfilePath != "" {
		p, err = detect.LoadProfiles(r.InspectionProfilePath)
		if err != nil {
			return nil, nil, errors.New("inspection profile reload failed")
		}
	}
	if r.BusinessProfilePath != "" {
		b, err = LoadBusinessProfiles(r.BusinessProfilePath)
		if err != nil {
			return nil, nil, err
		}
	}
	return p, b, nil
}

func (r *Runtime) InspectionProfileStatuses() []contract.InspectionProfileStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	statuses := r.InspectionProfiles.Statuses()
	return append(statuses, businessProfileStatuses(r.BusinessProfiles)...)
}

func validateDetectorName(value string) error {
	if _, ok := policy.NormalizeApplication(value); !ok {
		return errors.New("unsupported application detector")
	}
	return nil
}

func inspectionGeneration(profiles detect.Profiles, business map[string]*preparedBusiness, rule contract.Rule, routes map[string]route) string {
	parts := []string{profiles.Generation()}
	if p := business[rule.ID]; p != nil {
		parts = append(parts, rule.ID+":"+p.generation)
	}
	for _, r := range routes {
		if p := business[r.rule.ID]; p != nil {
			parts = append(parts, r.rule.ID+":"+p.generation)
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "|")
}

func prepareServiceInspection(v contract.ServiceConfig, profiles detect.Profiles, businessProfiles map[string]BusinessProfile) (map[string]*detect.Plan, map[string]*tls.Config, string, error) {
	plans := map[string]*detect.Plan{}
	origins := map[string]*tls.Config{}
	generation := []string{profiles.Generation()}
	var layers []contract.InboundPolicy
	if v.Policy != nil {
		layers = v.Policy.InboundLayers
	}
	for _, grant := range v.Grants {
		for _, target := range grant.Targets {
			if target.Business != nil && target.Business.TLSProfile != "" {
				for _, layer := range layers {
					if layer.TLSRequired || layer.RejectEmptySNI {
						return nil, nil, "", errors.New("exit business plaintext cannot independently verify entrance TLSRequired or RejectEmptySNI; enforce these at the TLS entrance")
					}
				}
			}
			key := tunnel.InspectionKey(target.RuleID, target.Network, target.Target)
			plan, err := detect.Prepare(layers, profiles, detect.Scope{RuleID: target.RuleID, Target: target.Target, GroupIDs: []string{v.GroupID}, Visibility: inspectionVisibility(target.Business), Network: target.Network})
			if err != nil {
				return nil, nil, "", err
			}
			business, err := prepareBusiness(target.Business, businessProfiles, "", target.Target, false)
			if err != nil {
				return nil, nil, "", err
			}
			plans[key] = plan
			if business != nil {
				origins[key] = business.upstream
				generation = append(generation, key+":"+business.generation)
			}
		}
	}
	sort.Strings(generation)
	return plans, origins, strings.Join(generation, "|"), nil
}
