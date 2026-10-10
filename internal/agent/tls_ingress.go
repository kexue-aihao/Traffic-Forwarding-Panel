package agent

import (
	"context"
	"errors"
	"net"
	"slices"
	"strconv"
	"syscall"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/policy/detect"
)

// A wildcard-to-specific IP change on the same port cannot stage two sockets.
// Only this deliberate listener change closes existing streams before binding;
// a failed apply restores the previous listening socket.
func (r *Runtime) replaceIngressSocket(v contract.SharedTLSIngress, original error, restorers *[]func() error) (net.Listener, error) {
	if !errors.Is(original, syscall.EADDRINUSE) {
		return nil, original
	}
	_, port, _ := net.SplitHostPort(v.Listen)
	for _, old := range r.listeners {
		_, oldPort, _ := net.SplitHostPort(old.rule.Listen)
		if old.ingressID != v.ID || oldPort != port {
			continue
		}
		old.mu.Lock()
		previous := old.rule.Listen
		old.tcp.Close()
		for c := range old.conns {
			c.Close()
		}
		old.mu.Unlock()
		*restorers = append(*restorers, func() error {
			l, err := net.Listen("tcp", previous)
			if err != nil {
				return err
			}
			old.mu.Lock()
			old.tcp = l
			old.mu.Unlock()
			go old.serveTCP()
			return nil
		})
		return net.Listen("tcp", v.Listen)
	}
	return nil, original
}

func validateTLSIngresses(c contract.Config) error {
	ids := map[string]contract.SharedTLSIngress{}
	listeners := map[string]bool{}
	counts := map[string]int{}
	for _, v := range c.TLSIngresses {
		host, p, err := net.SplitHostPort(v.Listen)
		port, e := strconv.Atoi(p)
		if err != nil || e != nil || net.ParseIP(host) == nil || port < 1 || port > 65535 || v.ID == "" || v.GroupID == "" || v.NodeID != c.NodeID || listeners[v.Listen] {
			return errors.New("invalid shared TLS ingress placement/listener")
		}
		if _, exists := ids[v.ID]; exists {
			return errors.New("duplicate shared TLS ingress")
		}
		ids[v.ID], listeners[v.Listen] = v, true
	}
	for _, r := range c.Rules {
		if !r.Enabled {
			continue
		}
		if r.SharedTLS != nil && r.SharedTLS.IngressID != "" {
			v, ok := ids[r.SharedTLS.IngressID]
			if !ok || r.NodeID != v.NodeID || r.GroupID != v.GroupID || r.Listen != v.Listen || r.Network != "tcp" || r.SharedTLS.ParentID != "" {
				return errors.New("shared TLS ingress unavailable or placement mismatch")
			}
			counts[v.ID]++
			if counts[v.ID] > 64 {
				return errors.New("at most 64 SNI routes per ingress")
			}
		} else if r.Network == "tcp" && listeners[r.Listen] {
			return errors.New("dedicated rule conflicts with shared TLS ingress")
		}
	}
	return nil
}

func (b *binding) rejectIngress(reason string) {
	if b.ingressID == "" {
		return
	}
	b.mu.Lock()
	b.ingressRejected++
	b.ingressLastReject = reason
	b.mu.Unlock()
}

func (r *Runtime) TLSIngressStatuses() []contract.TLSIngressStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []contract.TLSIngressStatus{}
	for _, b := range r.listeners {
		if b.ingressID == "" {
			continue
		}
		b.mu.Lock()
		v := contract.TLSIngressStatus{ID: b.ingressID, GroupID: b.rule.GroupID, Listen: b.rule.Listen, State: "listening", Routes: len(b.routes), Connections: len(b.slots), Rejected: b.ingressRejected, LastReject: b.ingressLastReject}
		if b.ctx.Err() != nil || !time.Now().Before(b.until) {
			v.State = "expired"
		}
		out = append(out, v)
		b.mu.Unlock()
	}
	slices.SortFunc(out, func(a, b contract.TLSIngressStatus) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	return out
}

// Called with b.mu held. Route generations own cancellation and connection
// revocation, while the administrator-owned socket survives route changes.
func (b *binding) updateIngressRoutes(next map[string]route, profiles detect.Profiles, business map[string]*preparedBusiness, pools map[string]*resourcePool) {
	if b.ctx.Err() != nil {
		b.ctx, b.cancel = context.WithCancel(context.Background())
	}
	for name, v := range next {
		selectedProfiles := detect.Profiles{}
		for _, layer := range ruleLayers(v.rule) {
			if layer.Inspection != nil {
				for _, label := range layer.Inspection.Profiles {
					if profile, ok := profiles[label]; ok {
						selectedProfiles[label] = profile
					}
				}
			}
		}
		v.generation = inspectionGeneration(selectedProfiles, business, v.rule, nil)
		old, ok := b.routes[name]
		if ok && old.ctx != nil && old.ctx.Err() == nil && old.generation == v.generation && sameForwardingRule(old.rule, v.rule) {
			v.ctx, v.cancel = old.ctx, old.cancel
		} else {
			if ok {
				b.revokeIngressRoute(old)
			}
			v.ctx, v.cancel = context.WithCancel(b.ctx)
		}
		v.pool = pools[limitOwner(v.rule)]
		next[name] = v
	}
	for name, old := range b.routes {
		if _, ok := next[name]; !ok {
			b.revokeIngressRoute(old)
		}
	}
	b.routes = next
	active := map[string]bool{}
	backendKeys := map[string]bool{}
	for _, v := range next {
		active[v.rule.ID] = true
		for _, candidate := range candidates(v.rule) {
			backendKeys[b.backendKey(v.rule, candidate.ID)] = true
		}
	}
	for id := range b.policyStatus {
		if !active[id] {
			delete(b.policyStatus, id)
		}
	}
	for key := range b.backends {
		if !backendKeys[key] {
			delete(b.backends, key)
		}
	}
}

// Called while holding b.mu, including late results from revoked handlers.
func (b *binding) activeIngressRule(id string) bool {
	if b.ingressID == "" {
		return true
	}
	for _, v := range b.routes {
		if v.rule.ID == id {
			return true
		}
	}
	return false
}

func (b *binding) revokeIngressRoute(v route) {
	if v.cancel != nil {
		v.cancel()
	}
	for c, id := range b.connRoutes {
		if id == v.rule.ID {
			c.Close()
		}
	}
}

func (b *binding) trackIngressTarget(c net.Conn, id string, ctx context.Context) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || ctx.Err() != nil || !time.Now().Before(b.until) {
		c.Close()
		return false
	}
	b.conns[c] = struct{}{}
	b.connRoutes[c] = id
	return true
}

func (b *binding) resetIngressExpiry() {
	if b.expiry != nil {
		b.expiry.Stop()
	}
	until := b.until
	b.expiry = time.AfterFunc(time.Until(until), func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.closed || b.until != until || time.Now().Before(b.until) {
			return
		}
		b.cancel()
		for c := range b.conns {
			c.Close()
		}
	})
}
