package agent

import (
	"net"
	"slices"
	"strings"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/policy/detect"
)

func (b *binding) policyRejected(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.policyStatus == nil {
		b.policyStatus = map[string]contract.RuleRuntimeStatus{}
	}
	v := b.policyStatus[id]
	v.RuleID = id
	v.InspectionLocation = "entry"
	v.Rejected++
	b.policyStatus[id] = v
}
func (b *binding) recordDial(rule contract.Rule, candidate string, conn net.Conn) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.policyStatus == nil {
		b.policyStatus = map[string]contract.RuleRuntimeStatus{}
	}
	v := b.policyStatus[rule.ID]
	v.RuleID = rule.ID
	v.InspectionLocation = "entry"
	v.CandidateID = candidate
	v.Carrier = rule.Transport
	if rule.EffectivePolicy != nil {
		v.PolicyHash = rule.EffectivePolicy.Hash
	}
	if conn != nil && conn.RemoteAddr() != nil {
		h, _, e := net.SplitHostPort(conn.RemoteAddr().String())
		if e == nil {
			if ip := net.ParseIP(h); ip != nil {
				v.AddressFamily = "ipv6"
				if ip.To4() != nil {
					v.AddressFamily = "ipv4"
				}
			}
		}
	}
	b.policyStatus[rule.ID] = v
}
func (b *binding) recordInspection(id string, d detect.Detection, visibility string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.policyStatus == nil {
		b.policyStatus = map[string]contract.RuleRuntimeStatus{}
	}
	v := b.policyStatus[id]
	v.RuleID = id
	v.InspectionLocation = "entry"
	v.Visibility = visibility
	v.DetectedProtocol = d.Protocol
	v.DetectedVariant = d.Variant
	v.Evidence = string(d.Evidence)
	v.InspectionReason = d.Reason
	if d.Status == detect.Unavailable {
		v.Unavailable++
	} else if d.Status == detect.NoMatch {
		v.Unknown++
	}
	b.policyStatus[id] = v
}
func (r *Runtime) PolicyStatuses() []contract.RuleRuntimeStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []contract.RuleRuntimeStatus{}
	for _, b := range r.listeners {
		b.mu.Lock()
		for _, v := range b.policyStatus {
			out = append(out, v)
		}
		b.mu.Unlock()
	}
	out = append(out, r.Services.RuleStatuses()...)
	slices.SortFunc(out, func(a, b contract.RuleRuntimeStatus) int { return strings.Compare(a.RuleID, b.RuleID) })
	return out
}
