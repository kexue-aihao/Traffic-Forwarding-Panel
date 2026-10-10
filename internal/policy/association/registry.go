// Package association observes successful, operator-controlled SOCKS5 UDP
// associations. It does not create proxy replies or trust a peer's assertion.
package association

import (
	"errors"
	"net"
	"net/netip"
	"slices"
	"sync"
	"time"
)

const MaxBindings = 256
const MaxSessions = 4096
const maxPeerSessions = 32

type lookupKey struct {
	ruleID, generation, target string
	peer                       netip.Addr
}

// Binding is an operator-authorized relationship. RelayEndpoint must equal the
// successful server BND reply and the reachable public entry UDP endpoint.
// RelayTarget is the exact fixed UDP origin rule target, not BND metadata.
type Binding struct {
	ControlRuleID string
	UDPRuleID     string
	Generation    string
	RelayEndpoint netip.AddrPort
	RelayTarget   string
}

type session struct {
	id       uint64
	bindings []Binding
	peer     netip.Addr
	port     uint16
	ready    bool
	closed   bool
	expires  time.Time
}
type Registry struct {
	mu       sync.Mutex
	limit    int
	ttl      time.Duration
	now      func() time.Time
	next     uint64
	allowed  map[Binding]bool
	sessions map[uint64]*session
	index    map[lookupKey]map[uint64]Binding
}

func New(limit int, ttl time.Duration) *Registry {
	if limit <= 0 || limit > MaxSessions {
		limit = MaxSessions
	}
	if ttl <= 0 || ttl > 24*time.Hour {
		ttl = 2 * time.Minute
	}
	return &Registry{limit: limit, ttl: ttl, now: time.Now, allowed: map[Binding]bool{}, sessions: map[uint64]*session{}, index: map[lookupKey]map[uint64]Binding{}}
}

func validBinding(b Binding) bool {
	return b.ControlRuleID != "" && b.UDPRuleID != "" && b.Generation != "" && len(b.ControlRuleID) <= 128 && len(b.UDPRuleID) <= 128 && len(b.Generation) <= 256 && b.RelayEndpoint.IsValid() && !b.RelayEndpoint.Addr().IsUnspecified() && b.RelayEndpoint.Port() != 0 && b.RelayTarget != "" && len(b.RelayTarget) <= 512
}
func canonicalBinding(b Binding) Binding {
	if b.RelayEndpoint.IsValid() {
		b.RelayEndpoint = netip.AddrPortFrom(b.RelayEndpoint.Addr().Unmap(), b.RelayEndpoint.Port())
	}
	return b
}

// Configure is called only at configuration commit. It drops removed/rotated
// relationships atomically and prevents old control snapshots from registering.
func (r *Registry) Configure(bindings []Binding) error {
	if len(bindings) > MaxBindings {
		return errors.New("SOCKS association binding capacity exceeded")
	}
	allowed := make(map[Binding]bool, len(bindings))
	for _, b := range bindings {
		b = canonicalBinding(b)
		if !validBinding(b) {
			return errors.New("invalid SOCKS association binding")
		}
		allowed[b] = true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.allowed = allowed
	r.pruneLocked()
	for id, s := range r.sessions {
		s.bindings = slices.DeleteFunc(s.bindings, func(b Binding) bool { return !allowed[b] })
		if len(s.bindings) == 0 {
			s.closed = true
			delete(r.sessions, id)
		}
	}
	clear(r.index)
	for _, s := range r.sessions {
		if s.ready && !r.indexSessionLocked(s) {
			s.ready = false
		}
	}
	return nil
}

func (r *Registry) pruneLocked() {
	now := r.now()
	for id, s := range r.sessions {
		if s.closed || !now.Before(s.expires) {
			r.removeSessionLocked(id, s)
		}
	}
}
func (r *Registry) removeSessionLocked(id uint64, s *session) {
	s.closed = true
	delete(r.sessions, id)
	for _, b := range s.bindings {
		k := lookupKey{b.UDPRuleID, b.Generation, b.RelayTarget, s.peer}
		delete(r.index[k], id)
		if len(r.index[k]) == 0 {
			delete(r.index, k)
		}
	}
}
func (r *Registry) indexSessionLocked(s *session) bool {
	for _, b := range s.bindings {
		k := lookupKey{b.UDPRuleID, b.Generation, b.RelayTarget, s.peer}
		if len(r.index[k]) >= maxPeerSessions {
			return false
		}
	}
	for _, b := range s.bindings {
		k := lookupKey{b.UDPRuleID, b.Generation, b.RelayTarget, s.peer}
		if r.index[k] == nil {
			r.index[k] = map[uint64]Binding{}
		}
		r.index[k][s.id] = b
	}
	return true
}
func (r *Registry) RevokeRule(ruleID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for b := range r.allowed {
		if b.ControlRuleID == ruleID || b.UDPRuleID == ruleID {
			delete(r.allowed, b)
		}
	}
	for id, s := range r.sessions {
		for _, b := range s.bindings {
			if b.ControlRuleID == ruleID || b.UDPRuleID == ruleID {
				r.removeSessionLocked(id, s)
				break
			}
		}
	}
}
func (r *Registry) RevokeGeneration(generation string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for b := range r.allowed {
		if b.Generation == generation {
			delete(r.allowed, b)
		}
	}
	for id, s := range r.sessions {
		for _, b := range s.bindings {
			if b.Generation == generation {
				r.removeSessionLocked(id, s)
				break
			}
		}
	}
}
func (r *Registry) RevokeAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.sessions {
		s.closed = true
	}
	clear(r.sessions)
	clear(r.index)
	clear(r.allowed)
}
func (r *Registry) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked()
	return len(r.sessions)
}

// Proof cannot be manufactured through configuration or a public struct
// literal. Its validity is checked against the live control TCP session.
type Proof struct {
	registry  *Registry
	sessionID uint64
	binding   Binding
	source    netip.AddrPort
}

func (p Proof) ValidFor(ruleID, generation, target string) bool {
	if p.registry == nil || p.binding.UDPRuleID != ruleID || p.binding.Generation != generation || p.binding.RelayTarget != target {
		return false
	}
	r := p.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.sessions[p.sessionID]
	if s != nil && !r.now().Before(s.expires) {
		r.removeSessionLocked(s.id, s)
		return false
	}
	return s != nil && s.ready && !s.closed && r.allowed[p.binding] && s.peer == p.source.Addr() && s.port == p.source.Port() && slices.Contains(s.bindings, p.binding)
}

// Admission derives evidence from the real UDP socket's source and a complete
// RFC1928 header. Unknown tuples return no proof; they are not authorization
// failures. Callers apply their independent unknown policy.
func (r *Registry) Admission(ruleID, generation string, source netip.AddrPort, target string, payload []byte) Proof {
	if !source.IsValid() || source.Port() == 0 || !validUDP(payload) {
		return Proof{}
	}
	source = netip.AddrPortFrom(source.Addr().Unmap(), source.Port())
	r.mu.Lock()
	defer r.mu.Unlock()
	var chosen *session
	var chosenBinding Binding
	k := lookupKey{ruleID, generation, target, source.Addr()}
	now := r.now()
	for id, b := range r.index[k] {
		s := r.sessions[id]
		if s == nil {
			delete(r.index[k], id)
			continue
		}
		if !now.Before(s.expires) {
			r.removeSessionLocked(id, s)
			continue
		}
		if !s.ready || s.closed || s.peer != source.Addr() || s.port != 0 && s.port != source.Port() {
			continue
		}
		if r.allowed[b] {
			if chosen != nil && chosen.id != s.id {
				return Proof{}
			}
			chosen = s
			chosenBinding = b
		}
	}
	if chosen == nil {
		return Proof{}
	}
	chosen.port = source.Port()
	chosen.expires = r.now().Add(r.ttl)
	return Proof{registry: r, sessionID: chosen.id, binding: chosenBinding, source: source}
}

func peerIP(c net.Conn) (netip.Addr, error) {
	a, e := netip.ParseAddrPort(c.RemoteAddr().String())
	if e != nil || !a.IsValid() || a.Addr().IsUnspecified() {
		return netip.Addr{}, errors.New("SOCKS association requires actual TCP peer endpoint")
	}
	return a.Addr().Unmap(), nil
}

func validUDP(b []byte) bool {
	if len(b) < 4 || b[0] != 0 || b[1] != 0 {
		return false
	}
	n := 4
	switch b[3] {
	case 1:
		n += 4
	case 4:
		n += 16
	case 3:
		if len(b) <= n || b[n] == 0 {
			return false
		}
		n += 1 + int(b[n])
	default:
		return false
	}
	return len(b) >= n+2
}
