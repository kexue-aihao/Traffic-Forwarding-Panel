package agent

import (
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"time"
)

type leaseRateSample struct {
	at   time.Time
	used map[string]int64
	rate float64
}
type leasePrefetch struct {
	RuleID    string `json:"rule_id"`
	LeaseID   string `json:"lease_id"`
	RawBudget int64  `json:"raw_budget,string"`
}

func ruleLeases(r contract.Rule) []*contract.Lease {
	leases := []*contract.Lease{r.Lease}
	if r.StandbyLease != nil {
		leases = append(leases, r.StandbyLease)
	}
	return leases
}

func (s *Store) spendingRuleLocked(r contract.Rule, until time.Time, n int64) contract.Rule {
	for _, l := range ruleLeases(r) {
		if l == nil {
			continue
		}
		candidate := r
		copy := *l
		if r.Lease != nil {
			copy.Limits = r.Lease.Limits
		}
		candidate.Lease = &copy
		if s.availableLocked(candidate, until, n) == nil {
			return candidate
		}
	}
	return r
}

func (s *Store) prefetches() []leasePrefetch {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rates == nil {
		s.rates = map[string]leaseRateSample{}
	}
	now := time.Now()
	out := []leasePrefetch{}
	active := map[string]bool{}
	for _, r := range s.state.Config.Rules {
		if !r.Enabled || !r.LeasePipeline || r.Lease == nil {
			continue
		}
		active[r.ID] = true
		previous := s.rates[r.ID]
		current := leaseRateSample{at: now, used: map[string]int64{}, rate: previous.rate}
		var delta int64
		for _, l := range ruleLeases(r) {
			if l == nil {
				continue
			}
			total := s.state.Used[l.ID]
			s.creditMu.Lock()
			for _, c := range s.credits {
				if c.window.LeaseID == l.ID {
					total += c.used - c.window.Confirmed
				}
			}
			s.creditMu.Unlock()
			current.used[l.ID] = total
			if old, ok := previous.used[l.ID]; ok {
				delta += max(0, total-old)
			}
		}
		if !previous.at.IsZero() && now.Sub(previous.at) >= 10*time.Millisecond {
			measured := float64(delta) / now.Sub(previous.at).Seconds()
			current.rate = max(measured, previous.rate*0.8)
		}
		s.rates[r.ID] = current
		_, retired := s.state.Retired[r.Lease.ID]
		if retired || r.StandbyLease != nil || !now.Before(r.Lease.ExpiresAt) {
			continue
		}
		spent := current.used[r.Lease.ID]
		// Idle or tiny flows must not reserve the remaining subscription and
		// starve other rules. Prefetch after meaningful use at the measured rate.
		if spent < min(int64(1<<20), r.Lease.Bytes/4) || spent == 0 || current.rate == 0 {
			continue
		}
		threshold := max(int64(64<<10), int64(min(float64(256<<20), current.rate*2)))
		if r.Lease.Bytes-spent > threshold {
			continue
		}
		budget := int64(min(float64(256<<20), max(float64(16<<20), current.rate*2)))
		out = append(out, leasePrefetch{RuleID: r.ID, LeaseID: r.Lease.ID, RawBudget: budget})
	}
	for id := range s.rates {
		if !active[id] {
			delete(s.rates, id)
		}
	}
	return out
}
