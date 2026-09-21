package agent

import (
	"context"
	"errors"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/tunnel"
	"net"
	"time"
)

type backendState struct {
	current int
	failed  bool
}

func (b *binding) backendKey(v contract.Rule, target string) string { return v.ID + "|" + target }
func (b *binding) selectBackend(v contract.Rule, tried map[string]bool) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.backends == nil {
		b.backends = map[string]*backendState{}
	}
	total := 0
	target := ""
	var best *backendState
	for _, t := range v.Backends {
		if t.Disabled || tried[t.Target] {
			continue
		}
		k := b.backendKey(v, t.Target)
		state := b.backends[k]
		if state == nil {
			state = &backendState{}
			b.backends[k] = state
		}
		if state.failed {
			continue
		}
		state.current += t.Weight
		total += t.Weight
		if best == nil || state.current > best.current {
			best = state
			target = t.Target
		}
	}
	if best != nil {
		best.current -= total
	}
	return target
}
func (b *binding) markBackend(v contract.Rule, target string, failed bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if state := b.backends[b.backendKey(v, target)]; state != nil {
		state.failed = failed
	}
}
func (b *binding) dialBackends(ctx context.Context, v contract.Rule) (net.Conn, *tunnel.Session, error) {
	tried := map[string]bool{}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	for len(tried) < len(v.Backends) {
		target := b.selectBackend(v, tried)
		if target == "" {
			break
		}
		tried[target] = true
		next := v
		next.Target = target
		attempt, stop := context.WithTimeout(ctx, 3*time.Second)
		conn, session, e := b.dialTarget(attempt, next)
		stop()
		if e == nil {
			return conn, session, nil
		}
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		b.markBackend(v, target, true)
	}
	return nil, nil, errors.New("all backends unavailable")
}
func (b *binding) healthLoop() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		b.mu.Lock()
		if b.closed {
			b.mu.Unlock()
			return
		}
		ctx := b.ctx
		rules := []contract.Rule{b.rule}
		for _, v := range b.routes {
			if v.rule.ID != b.rule.ID {
				rules = append(rules, v.rule)
			}
		}
		b.mu.Unlock()
		for _, v := range rules {
			for _, target := range v.Backends {
				if target.Disabled {
					continue
				}
				next := v
				next.Target = target.Target
				probe, cancel := context.WithTimeout(ctx, 2*time.Second)
				conn, _, e := b.dialTarget(probe, next)
				cancel()
				if conn != nil {
					conn.Close()
				}
				if ctx.Err() == nil {
					b.markBackend(v, target.Target, e != nil)
				}
			}
		}
	}
}
