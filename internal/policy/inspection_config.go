package policy

import (
	"errors"
	"slices"
	"strings"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/policy/detect"
)

// NormalizeApplication retains the historical socks union and app: inputs.
// Callers interpret historical top-level bare http as a carrier separately.
func NormalizeApplication(value string) (string, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.TrimPrefix(value, "app:")
	if detect.SupportedProtocol(value) {
		return value, true
	}
	return "", false
}

func validProfileLabel(value string) bool {
	if len(value) == 0 || len(value) > 64 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func NormalizeInspection(p *contract.InspectionPolicy) error {
	if p == nil {
		return nil
	}
	if p.Version != contract.InspectionVersion {
		return errors.New("unsupported inspection version")
	}
	if p.Mode == "" {
		p.Mode = "strict"
	}
	if p.Mode != "strict" && p.Mode != "observe" {
		return errors.New("inspection mode must be strict or observe")
	}
	if p.Unknown == "" {
		p.Unknown = "allow"
	}
	if p.Unknown != "allow" && p.Unknown != "deny" {
		return errors.New("inspection unknown must be allow or deny")
	}
	if p.Mode == "observe" && p.Unknown == "deny" {
		return errors.New("observation mode cannot deny unknown applications")
	}
	if len(p.Profiles) > 16 {
		return errors.New("at most 16 inspection profile labels")
	}
	for _, label := range p.Profiles {
		if !validProfileLabel(label) {
			return errors.New("inspection profile must be a local label")
		}
	}
	slices.Sort(p.Profiles)
	p.Profiles = slices.Compact(p.Profiles)
	if b := p.Business; b != nil {
		if b.WebSocketEarlyData && !b.WebSocket {
			return errors.New("WebSocket early data requires WebSocket adaptation")
		}
		if b.TLSProfile == "" && !b.WebSocket && b.UpstreamTLSProfile == "" {
			return errors.New("empty business adapter")
		}
		for _, label := range []string{b.TLSProfile, b.UpstreamTLSProfile} {
			if label != "" && !validProfileLabel(label) {
				return errors.New("business TLS profile must be a local label")
			}
		}
	}
	return nil
}
