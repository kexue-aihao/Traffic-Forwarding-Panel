package detect

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/policy/association"
)

type Status string

const (
	NeedMore    Status = "need_more"
	NoMatch     Status = "no_match"
	Match       Status = "match"
	Unavailable Status = "unavailable"
)

type Evidence string

const (
	StructuralEvidence Evidence = "structural"
	Authenticated      Evidence = "authenticated"
	LegacyAuth         Evidence = "legacy_auth"
	Probable           Evidence = "probable"
)

var ErrDenied = errors.New("inspection policy denied")

type Detection struct {
	Protocol string
	Variant  string
	Status   Status
	Evidence Evidence
	Need     int
	Reason   string
}
type Scope struct {
	RuleID           string
	Target           string
	GroupIDs         []string
	Visibility       string
	Network          string
	SOCKSAssociation bool
	Generation       string
}
type compiledProfile struct {
	protocol, variant, method string
	keys                      [][]byte
	trojanHash                []byte
	vmessKey                  [16]byte
	legacy                    bool
	networks                  []string
	udpStructural             bool
	udpAssociated             bool
	controlRules              []string
	relayEndpoint             netip.AddrPort
}
type candidate struct {
	profile compiledProfile
	strict  bool
}

// Plan is immutable and Feed is safe for concurrent use. Callers own each
// flow's bounded prefix and invoke Feed only after the returned Need is met.
type Plan struct {
	candidates          []candidate
	blocked             map[string]bool
	strict              map[string]bool
	unknownDeny         bool
	visibility          string
	scope               Scope
	unavailable         []string
	now                 func() time.Time
	generation          string
	hasAssociation      bool
	requiresAssociation bool
}

func SupportedProtocol(s string) bool {
	switch s {
	case "http", "socks", "socks4", "socks5", "shadowsocks", "vmess", "trojan":
		return true
	}
	return false
}
func expanded(s string) []string {
	if s == "socks" {
		return []string{"socks4", "socks5"}
	}
	return []string{s}
}

func Prepare(layers []contract.InboundPolicy, profiles Profiles, scope Scope) (*Plan, error) {
	if scope.Visibility == "" {
		scope.Visibility = "raw"
	}
	if scope.Visibility != "raw" && scope.Visibility != "tls-plaintext" && scope.Visibility != "ws-payload" {
		return nil, errors.New("unsupported inspection visibility")
	}
	if scope.Network != "" && scope.Network != "tcp" && scope.Network != "udp" {
		return nil, errors.New("unsupported inspection network")
	}
	p := &Plan{blocked: map[string]bool{}, strict: map[string]bool{}, visibility: scope.Visibility, scope: scope, now: time.Now}
	// Prerequisite diagnostics contain fixed reason codes, never credentials or
	// labels, and remain bounded even when several layers repeat a failure.
	noteUnavailable := func(reason string) {
		if len(p.unavailable) < MaxCredentials && !slices.Contains(p.unavailable, reason) {
			p.unavailable = append(p.unavailable, reason)
		}
	}
	p.generation = scope.Generation
	if p.generation == "" {
		p.generation = profiles.Generation()
	}
	selected := map[string]bool{}
	versionedStrictUDP := false
	legacyUDP := false
	for _, layer := range layers {
		ip := layer.Inspection
		observe := ip != nil && ip.Mode == "observe"
		if ip != nil {
			if ip.Version != contract.InspectionVersion || ip.Mode != "" && ip.Mode != "strict" && ip.Mode != "observe" || ip.Unknown != "" && ip.Unknown != "allow" && ip.Unknown != "deny" {
				return nil, errors.New("invalid inspection policy")
			}
			p.unknownDeny = p.unknownDeny || !observe && ip.Unknown == "deny"
			for _, label := range ip.Profiles {
				if !validLabel(label) {
					return nil, errors.New("invalid inspection profile label")
				}
				if _, ok := profiles[label]; !ok {
					if !observe {
						return nil, fmt.Errorf("inspection_profile_missing:%s", label)
					}
					noteUnavailable("profile_missing")
					continue
				}
				// The same label can be selected in several policy layers. Any
				// strict use keeps its prerequisites mandatory.
				selected[label] = selected[label] || !observe
			}
		}
		for _, app := range layer.BlockedApps {
			app = strings.TrimPrefix(app, "app:")
			if !SupportedProtocol(app) {
				return nil, errors.New("unsupported application detector")
			}
			for _, a := range expanded(app) {
				p.blocked[a] = true
				p.strict[a] = p.strict[a] || !observe
				if a == "socks5" && !observe {
					versionedStrictUDP = versionedStrictUDP || ip != nil
					legacyUDP = legacyUDP || ip == nil
				}
			}
		}
	}
	if len(selected) > MaxCredentials {
		return nil, errors.New("inspection credential limit exceeded")
	}
	labels := make([]string, 0, len(selected))
	for label := range selected {
		labels = append(labels, label)
	}
	slices.Sort(labels)
	credentials := 0
	for _, label := range labels {
		profile := profiles[label]
		strict := selected[label] || p.strict[profile.Protocol]
		if !matchesScope(profile, scope) {
			if strict {
				return nil, fmt.Errorf("inspection_profile_scope_denied:%s", label)
			}
			noteUnavailable("profile_scope_denied")
			continue
		}
		c, e := compileProfile(profile)
		if e != nil {
			if strict {
				return nil, fmt.Errorf("inspection_profile_invalid:%s", label)
			}
			noteUnavailable("profile_invalid")
			continue
		}
		if !p.blocked[c.protocol] {
			continue
		}
		if c.legacy && p.strict[c.protocol] {
			return nil, errors.New("inspection_legacy_is_diagnostic_only")
		}
		credentials += max(1, len(c.keys))
		if credentials > MaxCredentials {
			return nil, errors.New("inspection credential limit exceeded")
		}
		if scope.Network != "" && !slices.Contains(c.networks, scope.Network) {
			if !strict {
				noteUnavailable("profile_network_unavailable")
			}
			continue
		}
		if c.udpAssociated && scope.Network == "udp" && !scope.SOCKSAssociation {
			if strict {
				return nil, errors.New("inspection_socks5_udp_association_not_observable")
			}
			noteUnavailable("socks5_udp_association_not_observable")
			continue
		}
		p.candidates = append(p.candidates, candidate{profile: c, strict: p.strict[c.protocol]})
		p.hasAssociation = p.hasAssociation || c.udpAssociated && len(c.controlRules) > 0
		p.requiresAssociation = p.requiresAssociation || c.udpAssociated && len(c.controlRules) > 0 && p.strict[c.protocol]
	}
	for _, app := range []string{"socks4", "socks5", "http"} {
		if !p.blocked[app] && !p.unknownDeny {
			continue
		}
		found := false
		for _, c := range p.candidates {
			found = found || c.profile.protocol == app
		}
		if !found {
			p.candidates = append(p.candidates, candidate{profile: compiledProfile{protocol: app, variant: app, networks: []string{"tcp", "udp"}, udpStructural: app == "socks5" && legacyUDP && !versionedStrictUDP}, strict: p.strict[app]})
		}
	}
	for _, app := range []string{"shadowsocks", "vmess", "trojan"} {
		if !p.blocked[app] {
			continue
		}
		found := false
		for _, c := range p.candidates {
			found = found || c.profile.protocol == app
		}
		if !found {
			if p.strict[app] {
				return nil, fmt.Errorf("inspection_profile_required:%s", app)
			}
			noteUnavailable("profile_required:" + app)
		}
		if app == "trojan" && scope.Visibility == "raw" {
			if p.strict[app] {
				return nil, errors.New("inspection_business_tls_required:trojan")
			}
			noteUnavailable("opaque_tls:trojan")
		}
	}
	if scope.Network == "udp" && versionedStrictUDP {
		ok := false
		for _, c := range p.candidates {
			ok = ok || c.profile.protocol == "socks5" && (c.profile.udpStructural || c.profile.udpAssociated && scope.SOCKSAssociation)
		}
		if !ok {
			return nil, errors.New("inspection_socks5_udp_requires_association_or_structural_mode")
		}
	}
	return p, nil
}

func (p *Plan) Empty() bool         { return p == nil || len(p.blocked) == 0 && !p.unknownDeny }
func (p *Plan) UnknownDenied() bool { return p != nil && p.unknownDeny }
func (p *Plan) Requires() bool      { return !p.Empty() }

// HasAssociation reports whether this prepared plan needs live SOCKS5 control
// evidence without constructing bindings on the UDP packet path.
func (p *Plan) HasAssociation() bool { return p != nil && p.hasAssociation }

// RequiresAssociation distinguishes enforced SOCKS5 UDP prerequisites from
// optional observation; callers must never fabricate a proof for either mode.
func (p *Plan) RequiresAssociation() bool { return p != nil && p.requiresAssociation }
func (p *Plan) Generation() string {
	if p == nil {
		return ""
	}
	return p.generation
}
func (p *Plan) AssociationBindings() []association.Binding {
	if p == nil {
		return nil
	}
	var out []association.Binding
	for _, c := range p.candidates {
		if c.profile.udpAssociated {
			for _, control := range c.profile.controlRules {
				out = append(out, association.Binding{ControlRuleID: control, UDPRuleID: p.scope.RuleID, Generation: p.generation, RelayEndpoint: c.profile.relayEndpoint, RelayTarget: p.scope.Target})
			}
		}
	}
	return out
}
func (p *Plan) Protocols() []string {
	if p == nil {
		return nil
	}
	out := make([]string, 0, len(p.blocked))
	for a := range p.blocked {
		out = append(out, a)
	}
	slices.Sort(out)
	return out
}

// Feed is convenient for one complete datagram/prefix. TCP callers must use a
// NewSession so failed credentials and completed probe stages are not retried.
func (p *Plan) Feed(b []byte, end bool, network string) Detection {
	return p.NewSession().Feed(b, end, network)
}

// FeedAssociated accepts only a live registry-created proof for this exact rule,
// target and plan generation. A preparation capability bool is never proof.
func (p *Plan) FeedAssociated(b []byte, end bool, network string, proof association.Proof) Detection {
	s := p.NewSession()
	s.proof = proof
	return s.Feed(b, end, network)
}

type cryptoBudget struct{ remaining int }

func (b *cryptoBudget) take(n int) bool {
	if b.remaining < n {
		b.remaining = 0
		return false
	}
	b.remaining -= n
	return true
}
func budgetExceeded() Detection {
	return Detection{Status: Unavailable, Reason: "crypto_attempt_budget_exceeded"}
}

type candidateState struct {
	result  Detection
	started bool
}

// Session belongs to one append-only TCP prefix (or one complete datagram).
// It caches terminal detector results and each detector's next required length.
type Session struct {
	plan    *Plan
	states  []candidateState
	budget  cryptoBudget
	length  int
	network string
	proof   association.Proof
}

func (p *Plan) NewSession() *Session {
	n := 0
	if p != nil {
		n = len(p.candidates)
	}
	return &Session{plan: p, states: make([]candidateState, n), budget: cryptoBudget{remaining: 256}}
}
func (s *Session) Feed(b []byte, end bool, network string) Detection {
	p := s.plan
	if p.Empty() {
		return Detection{Status: NoMatch, Reason: "inspection_disabled"}
	}
	if len(b) > MaxPrefix {
		return Detection{Status: Unavailable, Reason: "prefix_budget_exceeded"}
	}
	if network != "tcp" && network != "udp" {
		return Detection{Status: Unavailable, Reason: "unsupported_network"}
	}
	if len(b) < s.length || s.network != "" && s.network != network {
		return Detection{Status: Unavailable, Reason: "non_monotonic_inspection_prefix"}
	}
	s.length = len(b)
	s.network = network
	var weak Detection
	next := MaxPrefix + 1
	unavailable := ""
	for index, c := range p.candidates {
		if !slices.Contains(c.profile.networks, network) {
			continue
		}
		state := &s.states[index]
		d := state.result
		if !state.started || d.Status == NeedMore && (len(b) >= d.Need || end) {
			switch c.profile.protocol {
			case "socks4", "socks5", "http":
				if c.profile.udpAssociated && network == "udp" {
					if !s.proof.ValidFor(p.scope.RuleID, p.generation, p.scope.Target) {
						d = Detection{Status: Unavailable, Reason: "socks5_udp_unassociated"}
					} else {
						d = structural(b, end, network, c.profile.protocol, true)
						if d.Status == Match {
							d.Variant = "socks5-udp-associated"
							d.Reason = "observed_control_association"
						}
					}
				} else {
					d = structural(b, end, network, c.profile.protocol, c.profile.udpStructural)
				}
			case "shadowsocks":
				d = detectSS(b, end, network, c.profile, p.now(), &s.budget)
			case "vmess":
				d = detectVMess(b, end, network, c.profile, p.now(), &s.budget)
			case "trojan":
				if p.visibility == "raw" && isTLS(b) {
					d = Detection{Status: Unavailable, Reason: "opaque_tls"}
				} else {
					d = detectTrojan(b, end, network, c.profile)
				}
			}
			state.started = true
			state.result = d
		}
		if d.Protocol == "" {
			d.Protocol = c.profile.protocol
		}
		if d.Variant == "" {
			d.Variant = c.profile.variant
		}
		if d.Status == Match {
			if d.Evidence == Authenticated {
				return d
			}
			if weak.Status != Match || d.Evidence != Probable {
				weak = d
			}
		}
		if d.Status == NeedMore && d.Need > len(b) && d.Need < next {
			next = d.Need
		}
		if d.Status == Unavailable && unavailable == "" {
			unavailable = d.Reason
		}
	}
	// Do not turn a one-byte structural collision into an early decision while
	// an authenticated candidate still requires its fixed-size header.
	if next <= MaxPrefix && !end {
		return Detection{Status: NeedMore, Need: next, Reason: "incomplete_prefix"}
	}
	if weak.Status == Match {
		return weak
	}
	if unavailable != "" {
		return Detection{Status: Unavailable, Reason: unavailable}
	}
	if len(p.unavailable) > 0 {
		return Detection{Status: Unavailable, Reason: p.unavailable[0]}
	}
	if next > MaxPrefix && next != MaxPrefix+1 {
		return Detection{Status: Unavailable, Reason: "prefix_budget_exceeded"}
	}
	if p.visibility == "raw" && isTLS(b) {
		return Detection{Status: Unavailable, Reason: "opaque_tls"}
	}
	return Detection{Status: NoMatch, Reason: "unknown_protocol"}
}

func (p *Plan) Decision(d Detection) error {
	if p == nil || d.Status == NeedMore {
		return nil
	}
	if d.Status == Match && (d.Evidence == Authenticated || d.Evidence == StructuralEvidence) && p.strict[d.Protocol] {
		return fmt.Errorf("%w:%s:%s", ErrDenied, d.Protocol, d.Evidence)
	}
	if p.unknownDeny && (d.Status != Match || d.Evidence == Probable || d.Evidence == LegacyAuth) {
		return fmt.Errorf("%w:unknown:%s", ErrDenied, d.Reason)
	}
	return nil
}

func need(n int, end bool) Detection {
	if n > MaxPrefix {
		return Detection{Status: Unavailable, Reason: "prefix_budget_exceeded"}
	}
	if end {
		return Detection{Status: NoMatch, Reason: "truncated_prefix"}
	}
	return Detection{Status: NeedMore, Need: n}
}
func noMatch(reason string) Detection { return Detection{Status: NoMatch, Reason: reason} }
func isTLS(b []byte) bool             { return len(b) >= 3 && b[0] == 22 && b[1] == 3 && b[2] <= 4 }
