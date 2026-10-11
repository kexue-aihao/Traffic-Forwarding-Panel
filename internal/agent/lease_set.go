package agent

import (
	"math"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

type leaseRateSample struct {
	at, active time.Time
	used       map[string]int64
	rate       float64
}
type leasePrefetch struct {
	RuleID    string `json:"rule_id"`
	LeaseID   string `json:"lease_id"`
	RawBudget int64  `json:"raw_budget,string"`
}

type leaseRetirement struct {
	LeaseID        string `json:"lease_id"`
	Used           int64  `json:"used_bytes,string"`
	PrefetchBudget int64  `json:"-"`
}

func leasePrefetchBudget(rate float64) int64 {
	return int64(min(float64(256<<20), max(float64(16<<20), rate*2)))
}

// A funded standby can be promoted and replenished by the same retirement
// response. Older panels ignore the optional header and use normal prefetch.
func (s *Store) retirementPrefetchBudget(leaseID string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.state.Config.Rules {
		if !r.Enabled || r.Network != "tcp" || !r.LeasePipeline || r.Lease == nil || r.Lease.ID != leaseID {
			continue
		}
		demand := s.rates[r.ID]
		if s.state.Used[leaseID] < min(int64(1<<20), r.Lease.Bytes/4) || demand.rate <= 0 || time.Since(demand.active) >= 5*time.Second {
			return 0
		}
		return leasePrefetchBudget(demand.rate)
	}
	return 0
}

func (s *Store) beginPrefetch(request leasePrefetch) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, retired := s.state.Retired[request.LeaseID]; retired {
		return false
	}
	if s.prefetching == nil {
		s.prefetching = map[string]string{}
	}
	s.prefetching[request.RuleID] = request.LeaseID
	return true
}

func (s *Store) endPrefetch(request leasePrefetch) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.prefetching, request.RuleID)
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
		var previousSpent int64
		for _, used := range previous.used {
			previousSpent += used
		}
		current := leaseRateSample{at: now, active: previous.active, used: map[string]int64{}, rate: previous.rate}
		var delta int64
		var first time.Time
		for _, l := range ruleLeases(r) {
			if l == nil {
				continue
			}
			total := s.state.Used[l.ID]
			s.creditMu.Lock()
			for _, c := range s.credits {
				if c.window.LeaseID == l.ID {
					total += c.used - c.window.Confirmed
					if first.IsZero() || c.window.StartedAt.Before(first) {
						first = c.window.StartedAt
					}
				}
			}
			s.creditMu.Unlock()
			current.used[l.ID] = total
			if old, ok := previous.used[l.ID]; ok {
				delta += max(0, total-old)
			} else if previous.at.IsZero() {
				delta += total
			}
		}
		if delta > 0 {
			current.active = now
		}
		// The first active window already provides a rate sample. Waiting for
		// two usage uploads can exhaust a 16 MiB initial lease before prefetch.
		if (previous.at.IsZero() || previousSpent == 0 && delta > 0) && !first.IsZero() {
			previous.at = first
		}
		if !previous.at.IsZero() && now.Sub(previous.at) >= 10*time.Millisecond {
			measured := float64(delta) / now.Sub(previous.at).Seconds()
			// Decay by elapsed time, not polling frequency. At 100 ms polls,
			// the old 0.8 factor erased most demand during a few slow control
			// round trips, shrinking the next lease and amplifying each stall.
			current.rate = max(measured, previous.rate*math.Exp(-now.Sub(previous.at).Seconds()/5))
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
		budget := leasePrefetchBudget(current.rate)
		out = append(out, leasePrefetch{RuleID: r.ID, LeaseID: r.Lease.ID, RawBudget: budget})
	}
	for id := range s.rates {
		if !active[id] {
			delete(s.rates, id)
		}
	}
	return out
}
