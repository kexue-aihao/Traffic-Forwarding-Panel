package agent

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/tunnel"
)

type backendState struct {
	current, failures int
	until             time.Time
	probing           bool
}

func (b *binding) backendKey(v contract.Rule, target string) string {
	h := ""
	if v.EffectivePolicy != nil {
		h = v.EffectivePolicy.Hash
	}
	return v.ID + "|" + h + "|" + target
}
func candidates(v contract.Rule) []contract.RouteCandidate {
	if len(v.RouteCandidates) > 0 {
		return v.RouteCandidates
	}
	out := []contract.RouteCandidate{}
	for _, x := range v.Backends {
		if !x.Disabled {
			out = append(out, contract.RouteCandidate{ID: x.Target, Target: x.Target, Weight: x.Weight, Transport: v.Transport, Tunnel: v.Tunnel, EffectivePolicy: v.EffectivePolicy, BillingMultiplier: v.BillingMultiplier})
		}
	}
	if len(out) == 0 && len(v.Backends) == 0 {
		out = append(out, contract.RouteCandidate{ID: v.Target, Target: v.Target, Weight: 1, Transport: v.Transport, Tunnel: v.Tunnel, EffectivePolicy: v.EffectivePolicy, BillingMultiplier: v.BillingMultiplier})
	}
	return out
}
func (b *binding) selectCandidate(v contract.Rule, tried map[string]bool, now time.Time) (contract.RouteCandidate, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.backends == nil {
		b.backends = map[string]*backendState{}
	}
	total := 0
	var best *backendState
	var selected contract.RouteCandidate
	for _, c := range candidates(v) {
		if tried[c.ID] {
			continue
		}
		k := b.backendKey(v, c.ID)
		s := b.backends[k]
		if s == nil {
			s = &backendState{}
			b.backends[k] = s
		}
		if now.Before(s.until) || s.probing {
			continue
		}
		s.current += max(1, c.Weight)
		total += max(1, c.Weight)
		if best == nil || s.current > best.current {
			best = s
			selected = c
		}
	}
	if best == nil {
		return selected, false
	}
	best.current -= total
	if best.failures > 0 && !best.until.IsZero() {
		best.probing = true
	}
	return selected, true
}
func (b *binding) markCandidate(v contract.Rule, id string, failed bool, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.backends[b.backendKey(v, id)]
	if s == nil {
		return
	}
	s.probing = false
	if !failed {
		s.failures = 0
		s.until = time.Time{}
		return
	}
	s.failures++
	maxFail, cooldown := 1, 10
	if v.EffectivePolicy != nil && v.EffectivePolicy.Failover != nil {
		f := v.EffectivePolicy.Failover
		maxFail = max(1, f.MaxFail)
		cooldown = f.CooldownSec
	}
	if s.failures >= maxFail {
		s.until = now.Add(time.Duration(max(1, cooldown)) * time.Second)
	}
}
func (b *binding) dialBackends(ctx context.Context, v contract.Rule) (net.Conn, *tunnel.Session, error) {
	return b.dialCandidates(ctx, v, nil)
}
func (b *binding) dialCandidates(ctx context.Context, v contract.Rule, check func(*contract.EffectivePolicy) error) (net.Conn, *tunnel.Session, error) {
	tried := map[string]bool{}
	lifetime := ctx
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	for len(tried) < len(candidates(v)) {
		c, ok := b.selectCandidate(v, tried, time.Now())
		if !ok {
			break
		}
		tried[c.ID] = true
		if c.BillingMultiplier != v.BillingMultiplier || c.ExitGroupID != "" && c.ExitGroupID != v.ExitGroupID {
			return nil, nil, errors.New("candidate authorization/billing mismatch")
		}
		if check != nil {
			if e := check(c.EffectivePolicy); e != nil {
				b.mu.Lock()
				b.backends[b.backendKey(v, c.ID)].probing = false
				b.mu.Unlock()
				return nil, nil, e
			}
		}
		next := v
		next.Target, next.Transport, next.Tunnel, next.EffectivePolicy = c.Target, c.Transport, c.Tunnel, c.EffectivePolicy
		attempt, stop := context.WithTimeout(ctx, 3*time.Second)
		if v.Network == "udp" {
			attempt = tunnel.WithDatagramLifetime(attempt, lifetime)
		}
		conn, session, e := b.dialTarget(attempt, next)
		stop()
		if e == nil {
			b.markCandidate(v, c.ID, false, time.Now())
			b.recordDial(next, c.ID, conn)
			return conn, session, nil
		}
		if ctx.Err() != nil {
			b.mu.Lock()
			b.backends[b.backendKey(v, c.ID)].probing = false
			b.mu.Unlock()
			return nil, nil, ctx.Err()
		}
		b.markCandidate(v, c.ID, true, time.Now())
	}
	return nil, nil, errors.New("all_candidates_unavailable")
}

// Recovery is demand-driven and single-flight; no data probes on business ports.
func (b *binding) healthLoop() { b.mu.Lock(); ctx := b.ctx; b.mu.Unlock(); <-ctx.Done() }
