package agent

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

// One pool per account per Agent: targets and intermediate tunnel hops never
// consume additional slots. TCP streams and UDP peer sessions each use one.
type resourcePool struct {
	mu          sync.Mutex
	limits      contract.ResourceLimits
	connections int
	ips         map[netip.Addr]int
	tokens      float64
	updated     time.Time
}

func limitOwner(v contract.Rule) string {
	if v.UserID != "" {
		return "user:" + v.UserID
	}
	return "rule:" + v.ID // Legacy local test configurations have no account.
}

func rateBurst(rate int64) float64 {
	if rate == 0 {
		return 0
	}
	return float64(min(max(rate/10, 65535), 1<<20))
}

func (p *resourcePool) refill(now time.Time) {
	if p.updated.IsZero() {
		p.tokens = rateBurst(p.limits.BytesPerSecondPerNode)
	} else {
		p.tokens = min(rateBurst(p.limits.BytesPerSecondPerNode), p.tokens+max(0, now.Sub(p.updated).Seconds())*float64(p.limits.BytesPerSecondPerNode))
	}
	p.updated = now
}

func (p *resourcePool) configure(limits contract.ResourceLimits) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ips == nil {
		p.ips = map[netip.Addr]int{}
	}
	if !p.updated.IsZero() {
		p.refill(time.Now())
	}
	p.limits = limits
	p.refill(time.Now()) // Does not refill the burst on lease/config refresh.
}

func (p *resourcePool) acquire(peer net.Addr) (func(), bool) {
	host, _, err := net.SplitHostPort(peer.String())
	if err != nil {
		return nil, false
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return nil, false
	}
	ip = ip.Unmap().WithZone("")
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.limits.MaxConnectionsPerNode > 0 && p.connections >= p.limits.MaxConnectionsPerNode {
		return nil, false
	}
	if p.limits.MaxIPsPerNode > 0 && p.ips[ip] == 0 && len(p.ips) >= p.limits.MaxIPsPerNode {
		return nil, false
	}
	p.connections++
	p.ips[ip]++
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.connections--
			p.ips[ip]--
			if p.ips[ip] == 0 {
				delete(p.ips, ip)
			}
		})
	}, true
}

func (p *resourcePool) take(n int) (bool, time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	rate := p.limits.BytesPerSecondPerNode
	if rate == 0 {
		return true, 0
	}
	p.refill(time.Now())
	if p.tokens >= float64(n) {
		p.tokens -= float64(n)
		return true, 0
	}
	return false, max(time.Millisecond, time.Duration((float64(n)-p.tokens)/float64(rate)*float64(time.Second)))
}

func (p *resourcePool) wait(ctx context.Context, n int) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ok, delay := p.take(n)
		if ok {
			return nil
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (b *binding) charge(ctx context.Context, pool *resourcePool, v contract.Rule, up bool, n int) error {
	waitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if v.Network == "udp" {
		if waitCtx.Err() != nil {
			return waitCtx.Err()
		}
		if ok, _ := pool.take(n); !ok {
			return errRateDrop
		}
	} else if err := pool.wait(waitCtx, n); err != nil {
		return err
	}
	return b.chargeCurrent(waitCtx, v.ID, v.Network, up, n)
}

var errRateDrop = errors.New("UDP rate limit")

func minTime(values ...time.Time) time.Time {
	out := values[0]
	for _, v := range values[1:] {
		if v.Before(out) {
			out = v
		}
	}
	return out
}
