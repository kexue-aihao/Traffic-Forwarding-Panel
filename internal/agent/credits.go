package agent

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

const creditWindowSize int64 = 256 << 10
const maxRuleCredits int64 = 1 << 20
const maxAccountCredits int64 = 8 << 20
const tcpCreditIdleTimeout = time.Second

var errCreditUnavailable = errors.New("persisted credit unavailable")

func usesCreditWindows(r contract.Rule) bool {
	return r.Network == "tcp" || r.Network == "udp" && r.UDP != nil && r.UDP.CreditWindows
}

type creditWindow struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	RuleID    string    `json:"rule_id"`
	LeaseID   string    `json:"lease_id"`
	Upload    bool      `json:"upload"`
	Capacity  int64     `json:"capacity"`
	Confirmed int64     `json:"confirmed"`
	Sequence  uint64    `json:"sequence"`
	StartedAt time.Time `json:"started_at"`
	Until     time.Time `json:"until"`
	Closed    bool      `json:"closed,omitempty"`
}

type liveCredit struct {
	window creditWindow
	used   int64
	last   time.Time
	closed bool
}

type creditPoolKey struct {
	rule   string
	upload bool
}

func creditKey(rule string, up bool) creditPoolKey { return creditPoolKey{rule, up} }

func (s *Store) creditLiabilityLocked(lease string) int64 {
	var n int64
	for _, w := range s.state.Windows {
		if w.LeaseID == lease {
			n += w.Capacity - w.Confirmed
		}
	}
	return n
}

// TryCreditCharge never acquires the durable-state mutex or waits for the writer.
// A reservation becomes visible here only after its WAL sync has succeeded.
// TCP waits for a refill in the caller; UDP drops an unreserved datagram.
func (s *Store) TryCreditCharge(r contract.Rule, until time.Time, up bool, n int) error {
	if n < 0 {
		return errors.New("negative charge")
	}
	if s.fastFailed.Load() {
		return errors.New("durable storage unavailable")
	}
	if r.Lease == nil {
		return errLeaseUnavailable
	}
	if r.Network == "tcp" {
		var capacity int64
		for _, l := range ruleLeases(r) {
			if l != nil {
				capacity = max(capacity, l.Bytes)
			}
		}
		if int64(n) > capacity {
			return errors.New("payload exceeds lease capacity")
		}
	}
	k := creditKey(r.ID, up)
	s.creditMu.Lock()
	// A new window can be published while this debit waits for creditMu.
	// Timestamp the debit after acquiring the lock so usage cannot predate
	// its reservation, and expiry is checked at the point of admission.
	now := time.Now()
	if !now.Before(until) {
		s.creditMu.Unlock()
		return errLeaseUnavailable
	}
	var remaining int64
	var candidates [2]*liveCredit
	eligible := candidates[:0]
	for _, c := range s.creditPools[k] {
		matches := false
		for _, l := range ruleLeases(r) {
			if l != nil && l.ID == c.window.LeaseID && now.Before(l.ExpiresAt) {
				matches = true
			}
		}
		if c.closed || !matches || c.window.UserID != limitOwner(r) || !now.Before(c.window.Until) {
			continue
		}
		left := c.window.Capacity - c.used
		remaining += left
		eligible = append(eligible, c)
	}
	admitted := len(eligible) > 0 && int64(n) <= remaining
	if admitted {
		needed := int64(n)
		for _, c := range eligible {
			debit := min(needed, c.window.Capacity-c.used)
			c.used += debit
			if debit > 0 {
				c.last = now.UTC()
			}
			needed -= debit
			if needed == 0 {
				break
			}
		}
		remaining -= int64(n)
	}
	refill := remaining < creditWindowSize && !s.refilling[k]
	if refill {
		s.refilling[k] = true
	}
	s.creditMu.Unlock()
	if refill {
		req := &stateRequest{event: stateEvent{Kind: "credit_reserve"}, rule: r, until: until, upload: up, n: n, done: make(chan error, 1)}
		s.submitMu.RLock()
		queued := false
		if !s.closing {
			select {
			case s.queue <- req:
				queued = true
			default:
			}
		}
		s.submitMu.RUnlock()
		if !queued {
			s.creditMu.Lock()
			delete(s.refilling, k)
			s.creditMu.Unlock()
		}
	}
	if !admitted {
		s.requestUsage()
		return errCreditUnavailable
	}
	return nil
}

func (s *Store) TryUDPCharge(r contract.Rule, until time.Time, up bool, n int) error {
	return s.TryCreditCharge(r, until, up, n)
}

func (s *Store) PrimeUDPCredits(r contract.Rule, until time.Time) error {
	if r.UDP == nil || !r.UDP.CreditWindows {
		return nil
	}
	for _, up := range []bool{true, false} {
		for range 2 {
			if e := s.submit(&stateRequest{event: stateEvent{Kind: "credit_reserve"}, rule: r, until: until, upload: up, creditPrime: true}); e != nil && !transientMeterError(e) {
				return e
			}
		}
	}
	return nil
}

func (s *Store) FlushUDPCredits() error {
	return s.FlushCredits()
}

// FlushCredits settles actual TCP/UDP consumption, preserving unused credit.
func (s *Store) FlushCredits() error {
	return s.submit(&stateRequest{event: stateEvent{Kind: "credit_flush"}})
}

func (s *Store) hasSpendableCredit(r contract.Rule, until time.Time) bool {
	now := time.Now()
	if s.fastFailed.Load() || !now.Before(until) {
		return false
	}
	s.creditMu.Lock()
	defer s.creditMu.Unlock()
	for _, up := range []bool{true, false} {
		for _, c := range s.creditPools[creditKey(r.ID, up)] {
			if c.closed || c.window.UserID != limitOwner(r) || c.used >= c.window.Capacity || !now.Before(c.window.Until) {
				continue
			}
			for _, l := range ruleLeases(r) {
				if l != nil && l.ID == c.window.LeaseID && now.Before(l.ExpiresAt) {
					return true
				}
			}
		}
	}
	return false
}

func (s *Store) reserveCreditLocked(r *stateRequest, staged []stateEvent) (*stateEvent, error) {
	k := creditKey(r.rule.ID, r.upload)
	defer func() { s.creditMu.Lock(); delete(s.refilling, k); s.creditMu.Unlock() }()
	if s.state.Config.NodeID != "" {
		current := false
		for _, rule := range s.state.Config.Rules {
			if rule.ID == r.rule.ID && rule.Enabled && usesCreditWindows(rule) && sameForwardingRule(rule, r.rule) {
				r.rule = rule
				r.until = minTime(r.until, s.state.Config.ValidUntil)
				current = true
				break
			}
		}
		if !current {
			return nil, errLeaseUnavailable
		}
	}
	// Account for earlier reservations in this batch without publishing any
	// unsynced memory credit. All candidates remain covered by one WAL sync.
	for _, l := range ruleLeases(r.rule) {
		if l == nil {
			continue
		}
		candidate := r.rule
		candidate.Lease = l
		var extra int64
		for _, e := range staged {
			if e.Window.LeaseID == l.ID {
				extra += e.Window.Capacity
			}
		}
		if s.availableLocked(candidate, r.until, extra+1) == nil {
			r.rule = candidate
			break
		}
	}
	if e := s.availableLocked(r.rule, r.until, 0); e != nil {
		return nil, e
	}
	incoming := []*contract.Lease{r.rule.Lease}
	for _, e := range staged {
		incoming = append(incoming, e.Lease)
	}
	if e := s.validateLeaseCapacity(incoming); e != nil {
		return nil, e
	}
	if len(s.state.Pending)+len(s.state.Windows)+len(staged) >= MaxPendingRecords {
		return nil, errSpoolFull
	}
	var ruleTotal, userTotal, directionTotal int64
	count := 0
	owner := limitOwner(r.rule)
	for _, w := range s.state.Windows {
		left := w.Capacity - w.Confirmed
		if w.RuleID == r.rule.ID {
			ruleTotal += left
			if w.Upload == r.upload {
				count++
				if w.LeaseID == r.rule.Lease.ID {
					directionTotal += left
				}
			}
		}
		if w.UserID == owner {
			userTotal += left
		}
	}
	var stagedLease int64
	for _, e := range staged {
		w := e.Window
		if w.RuleID == r.rule.ID {
			ruleTotal += w.Capacity
			if w.Upload == r.upload {
				count++
				if w.LeaseID == r.rule.Lease.ID {
					directionTotal += w.Capacity
				}
			}
		}
		if w.UserID == owner {
			userTotal += w.Capacity
		}
		if w.LeaseID == r.rule.Lease.ID {
			stagedLease += w.Capacity
		}
	}
	if count >= 2 {
		return nil, nil
	}
	capacity := min(creditWindowSize, max(int64(r.n), (r.rule.Lease.Bytes-1)/4+1), maxRuleCredits-ruleTotal, maxAccountCredits-userTotal, r.rule.Lease.Bytes-s.state.Used[r.rule.Lease.ID]-s.creditLiabilityLocked(r.rule.Lease.ID)-stagedLease)
	if r.creditPrime {
		remaining := r.rule.Lease.Bytes - s.state.Used[r.rule.Lease.ID]
		half := remaining / 2
		if r.upload {
			half += remaining % 2
		}
		capacity = min(capacity, half-directionTotal)
	}
	if capacity <= 0 {
		return nil, errLeaseUnavailable
	}
	if r.n > 0 && capacity < int64(r.n) {
		return nil, errLeaseUnavailable
	}
	id, e := randomID()
	if e != nil {
		return nil, e
	}
	w := creditWindow{ID: id, UserID: owner, RuleID: r.rule.ID, LeaseID: r.rule.Lease.ID, Upload: r.upload, Capacity: capacity, StartedAt: time.Now().UTC(), Until: minTime(r.until, r.rule.Lease.ExpiresAt)}
	l := *r.rule.Lease
	return &stateEvent{Kind: "credit_reserve", Window: &w, Lease: &l}, nil
}

// Snapshot memory progress without holding creditMu during append/sync. Closing
// first freezes debits; already admitted sends are included in the final total.
func (s *Store) creditProgressLocked(trigger stateEvent) ([]stateEvent, error) {
	active := map[string]bool{}
	tcpRules := map[string]bool{}
	for _, r := range s.state.Config.Rules {
		if r.Network == "tcp" {
			tcpRules[r.ID] = true
		}
	}
	if trigger.Config != nil {
		for _, r := range trigger.Config.Rules {
			if r.Enabled && usesCreditWindows(r) && r.Lease != nil {
				// A destination, owner or limit change revokes the old windows even
				// when the control plane keeps the same lease identifier.
				compatible := false
				for _, old := range s.state.Config.Rules {
					if old.ID == r.ID && sameForwardingRule(old, r) {
						compatible = true
						break
					}
				}
				if !compatible {
					continue
				}
				for _, l := range ruleLeases(r) {
					if l != nil {
						active[l.ID] = true
					}
				}
			}
		}
	}
	s.creditMu.Lock()
	defer s.creditMu.Unlock()
	ids := make([]string, 0, len(s.state.Windows))
	for id := range s.state.Windows {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	events := []stateEvent{}
	room := MaxPendingRecords - len(s.state.Pending) - len(s.state.Windows)
	for _, id := range ids {
		w := s.state.Windows[id]
		c := s.credits[id]
		if c == nil {
			return nil, errors.New("missing live credit")
		}
		// Return idle TCP reservations to the shared account budget. Rules that
		// once transferred data must not hold all credit slots indefinitely.
		idle := tcpRules[w.RuleID] && time.Since(c.last) >= tcpCreditIdleTimeout
		closeWindow := idle || trigger.Kind == "credit_close" || trigger.Kind == "credit_rebalance" && trigger.RuleID == w.RuleID || c.used == w.Capacity || !time.Now().Before(w.Until) || trigger.Kind == "retire" && trigger.LeaseID == w.LeaseID || trigger.Config != nil && !active[w.LeaseID]
		if !closeWindow && (c.used == w.Confirmed || room <= 0) {
			continue
		}
		if closeWindow {
			c.closed = true
		} else {
			room--
		}
		w.Closed = closeWindow
		w.Sequence++
		w.Confirmed = c.used
		e := stateEvent{Kind: "credit_progress", Window: &w}
		if delta := c.used - s.state.Windows[id].Confirmed; delta > 0 {
			u := s.creditUsage(w, delta, "normal", minTime(c.last, w.Until))
			e.Usage = &u
		}
		events = append(events, e)
	}
	return events, nil
}

func (s *Store) creditUsage(w creditWindow, n int64, kind string, end time.Time) contract.UsageRecord {
	u := contract.UsageRecord{ID: fmt.Sprintf("%s:%d", w.ID, w.Sequence), NodeID: s.state.Identity.NodeID, RuleID: w.RuleID, LeaseID: w.LeaseID, EntitlementID: s.state.Leases[w.LeaseID].EntitlementID, StartedAt: w.StartedAt, EndedAt: end, Kind: kind, WindowID: w.ID, WindowSequence: w.Sequence}
	if w.Upload {
		u.UploadBytes = n
	} else {
		u.DownloadBytes = n
	}
	return u
}

func (s *Store) validateCreditEvent(e stateEvent) error {
	w := e.Window
	if w == nil || len(w.ID) != 32 || w.RuleID == "" || w.UserID == "" || w.Capacity <= 0 || w.Capacity > creditWindowSize || w.Confirmed < 0 || w.Confirmed > w.Capacity || w.StartedAt.IsZero() || !w.Until.After(w.StartedAt) {
		return errors.New("invalid credit window")
	}
	if e.Kind == "credit_reserve" {
		if _, exists := s.state.Windows[w.ID]; exists || w.Confirmed != 0 || w.Sequence != 0 || w.Closed || e.Lease == nil || w.LeaseID != e.Lease.ID {
			return errors.New("invalid credit reservation")
		}
		if old, ok := s.state.Leases[w.LeaseID]; ok && !sameLease(old, *e.Lease) {
			return errors.New("credit lease changed")
		}
		if _, retired := s.state.Retired[w.LeaseID]; retired {
			return errLeaseUnavailable
		}
		if e := s.validateLeaseCapacity([]*contract.Lease{e.Lease}); e != nil {
			return e
		}
		var ruleTotal, userTotal int64
		count := 0
		for _, old := range s.state.Windows {
			left := old.Capacity - old.Confirmed
			if old.RuleID == w.RuleID {
				ruleTotal += left
				if old.Upload == w.Upload {
					count++
				}
			}
			if old.UserID == w.UserID {
				userTotal += left
			}
		}
		if count >= 2 || w.Capacity > maxRuleCredits-ruleTotal || w.Capacity > maxAccountCredits-userTotal {
			return errors.New("credit reservation exceeds responsibility bounds")
		}
		if w.Capacity > e.Lease.Bytes-s.state.Used[w.LeaseID]-s.creditLiabilityLocked(w.LeaseID) || w.Until.After(e.Lease.ExpiresAt) || len(s.state.Pending)+len(s.state.Windows) >= MaxPendingRecords {
			return errors.New("credit reservation exceeds budget")
		}
	} else {
		old, ok := s.state.Windows[w.ID]
		if !ok || w.LeaseID != old.LeaseID || w.RuleID != old.RuleID || w.UserID != old.UserID || w.Upload != old.Upload || w.Capacity != old.Capacity || !w.StartedAt.Equal(old.StartedAt) || !w.Until.Equal(old.Until) || w.Sequence != old.Sequence+1 || w.Confirmed < old.Confirmed {
			return errors.New("invalid credit progress")
		}
		delta := w.Confirmed - old.Confirmed
		if delta > 0 {
			u := e.Usage
			if u == nil || !u.ValidWindowMetadata() || u.ID != fmt.Sprintf("%s:%d", w.ID, w.Sequence) || u.WindowID != w.ID || u.WindowSequence != w.Sequence || u.RuleID != w.RuleID || u.LeaseID != w.LeaseID || u.NodeID != s.state.Identity.NodeID || u.EntitlementID != s.state.Leases[w.LeaseID].EntitlementID || u.UploadBytes < 0 || u.DownloadBytes < 0 || u.UploadBytes+u.DownloadBytes != delta || w.Upload && u.DownloadBytes != 0 || !w.Upload && u.UploadBytes != 0 || !u.StartedAt.Equal(w.StartedAt) || u.EndedAt.Before(w.StartedAt) || u.EndedAt.After(w.Until) || u.Kind == "recovery" && (!w.Closed || w.Confirmed != w.Capacity) || len(s.state.Pending) >= MaxPendingRecords {
				return errors.New("invalid credit usage")
			}
		} else if e.Usage != nil {
			return errors.New("unexpected credit usage")
		}
	}
	return nil
}

func (s *Store) applyCreditEvent(e stateEvent) error {
	if err := s.validateCreditEvent(e); err != nil {
		return err
	}
	w := *e.Window
	if e.Kind == "credit_reserve" {
		s.state.Leases[w.LeaseID] = *e.Lease
		s.state.Windows[w.ID] = w
		s.creditMu.Lock()
		c := &liveCredit{window: w, last: w.StartedAt}
		s.credits[w.ID] = c
		k := creditKey(w.RuleID, w.Upload)
		s.creditPools[k] = append(s.creditPools[k], c)
		s.creditMu.Unlock()
		return nil
	}
	if e.Usage != nil {
		s.state.Used[w.LeaseID] += e.Usage.UploadBytes + e.Usage.DownloadBytes
		s.state.Pending = append(s.state.Pending, *e.Usage)
	}
	if w.Closed {
		delete(s.state.Windows, w.ID)
	} else {
		s.state.Windows[w.ID] = w
	}
	s.creditMu.Lock()
	if c := s.credits[w.ID]; c != nil {
		c.window = w
		if w.Closed {
			c.closed = true
			delete(s.credits, w.ID)
			k := creditKey(w.RuleID, w.Upload)
			pool := s.creditPools[k][:0]
			for _, other := range s.creditPools[k] {
				if other != c {
					pool = append(pool, other)
				}
			}
			if len(pool) == 0 {
				delete(s.creditPools, k)
			} else {
				s.creditPools[k] = pool
			}
		}
	}
	s.creditMu.Unlock()
	return nil
}

func (s *Store) commitCreditEventsLocked(events []stateEvent) error {
	if len(events) == 0 {
		return nil
	}
	e := s.appendEvents(events)
	if e == nil {
		for _, event := range events {
			if e = s.applyEvent(event); e != nil {
				break
			}
		}
	}
	if e == nil && s.walBytes >= walCheckpointBytes {
		e = s.checkpointLocked()
	}
	if e != nil {
		s.failed = true
		s.fastFailed.Store(true)
		s.notifyChangedLocked()
	} else {
		s.notifyChangedLocked()
		s.requestUsage()
	}
	return e
}

func (s *Store) processCreditsLocked(r *stateRequest) error {
	events, e := s.creditProgressLocked(stateEvent{Kind: "credit_flush"})
	if e != nil {
		return e
	}
	if r.event.Kind == "credit_close" {
		events, e = s.creditProgressLocked(r.event)
		if e != nil {
			return e
		}
	}
	if e = s.commitCreditEventsLocked(events); e != nil {
		return e
	}
	if r.event.Kind != "credit_reserve" {
		return nil
	}
	if e := s.rebalanceCreditsLocked(r); e != nil {
		return e
	}
	event, e := s.reserveCreditLocked(r, nil)
	if e != nil || event == nil {
		return e
	}
	return s.commitCreditEventsLocked([]stateEvent{*event})
}

func (s *Store) processCreditBatchLocked(batch []*stateRequest) {
	if len(batch) == 1 {
		batch[0].done <- s.processCreditsLocked(batch[0])
		return
	}
	progress, e := s.creditProgressLocked(stateEvent{Kind: "credit_flush"})
	if e == nil {
		e = s.commitCreditEventsLocked(progress)
	}
	if e != nil {
		for _, r := range batch {
			r.done <- e
		}
		return
	}
	events := []stateEvent{}
	results := make([]error, len(batch))
	for i, r := range batch {
		if e := s.rebalanceCreditsLocked(r); e != nil {
			for _, req := range batch {
				req.done <- e
			}
			return
		}
		event, err := s.reserveCreditLocked(r, events)
		results[i] = err
		if err == nil && event != nil {
			events = append(events, *event)
		}
	}
	if e := s.commitCreditEventsLocked(events); e != nil {
		for _, r := range batch {
			r.done <- e
		}
		return
	}
	for i, r := range batch {
		r.done <- results[i]
	}
}

func (s *Store) rebalanceCreditsLocked(r *stateRequest) error {
	if r.n <= 0 {
		return nil
	}
	s.creditMu.Lock()
	pool := s.creditPools[creditKey(r.rule.ID, r.upload)]
	var remaining int64
	for _, c := range pool {
		if !c.closed {
			remaining += c.window.Capacity - c.used
		}
	}
	hasWindows := len(pool) > 0 || len(s.creditPools[creditKey(r.rule.ID, !r.upload)]) > 0
	rebalance := hasWindows && remaining < int64(r.n)
	s.creditMu.Unlock()
	if !rebalance {
		return nil
	}
	// A missing directional window can be filled from unused lease budget.
	// Closing the opposite direction in that case can make echo traffic
	// alternate between upload-only and download-only credit indefinitely.
	if len(pool) < 2 {
		var ruleTotal, userTotal int64
		owner := limitOwner(r.rule)
		for _, w := range s.state.Windows {
			left := w.Capacity - w.Confirmed
			if w.RuleID == r.rule.ID {
				ruleTotal += left
			}
			if w.UserID == owner {
				userTotal += left
			}
		}
		if int64(r.n) <= maxRuleCredits-ruleTotal && int64(r.n) <= maxAccountCredits-userTotal {
			for _, l := range ruleLeases(r.rule) {
				if l == nil {
					continue
				}
				candidate := r.rule
				candidate.Lease = l
				if s.availableLocked(candidate, r.until, int64(r.n)) == nil {
					return nil
				}
			}
		}
	}
	// Small leases can have enough total budget for a packet while the two
	// directional windows strand unused bytes. Freeze and settle the rule's
	// windows before returning those bytes to the shared budget. This happens
	// only in the writer; TCP waits and UDP drops instead of blocking its reader.
	events, e := s.creditProgressLocked(stateEvent{Kind: "credit_rebalance", RuleID: r.rule.ID})
	if e != nil {
		return e
	}
	return s.commitCreditEventsLocked(events)
}

func (s *Store) recoverCredits() error {
	// No live credits survive a process restart. Account the uncertain remainder
	// using the original lease/time interval and one durable, retryable fact.
	ids := make([]string, 0, len(s.state.Windows))
	for id := range s.state.Windows {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		w := s.state.Windows[id]
		w.Sequence++
		w.Closed = true
		delta := w.Capacity - w.Confirmed
		w.Confirmed = w.Capacity
		event := stateEvent{Kind: "credit_progress", Window: &w}
		if delta > 0 {
			u := s.creditUsage(w, delta, "recovery", minTime(time.Now().UTC(), w.Until))
			event.Usage = &u
		}
		if e := s.commitCreditEventsLocked([]stateEvent{event}); e != nil {
			return e
		}
	}
	return nil
}
