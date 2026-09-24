package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

const MaxPendingRecords = 4096
const maxStoreQueue = 256
const maxCommitBatch = 128

var errLeaseUnavailable = errors.New("configuration or lease unavailable")
var errSpoolFull = errors.New("usage spool full")

type diskState struct {
	Identity contract.Registered       `json:"identity"`
	Config   contract.Config           `json:"config"`
	Used     map[string]int64          `json:"used"`
	Pending  []contract.UsageRecord    `json:"pending"`
	Retired  map[string]bool           `json:"retired"`
	Leases   map[string]contract.Lease `json:"leases"`
}
type stateEvent struct {
	Kind     string                `json:"kind"`
	Identity *contract.Registered  `json:"identity,omitempty"`
	Config   *contract.Config      `json:"config,omitempty"`
	Usage    *contract.UsageRecord `json:"usage,omitempty"`
	Lease    *contract.Lease       `json:"lease,omitempty"`
	IDs      []string              `json:"ids,omitempty"`
	LeaseID  string                `json:"lease_id,omitempty"`
}
type stateRequest struct {
	ctx    context.Context
	event  stateEvent
	rule   contract.Rule
	until  time.Time
	upload bool
	n      int
	done   chan error
}
type Store struct {
	mu         sync.Mutex
	path       string
	state      diskState
	failed     bool
	lock       *flock.Flock
	wal        *os.File
	seq        uint64
	walBytes   int64
	queue      chan *stateRequest
	done       chan struct{}
	submitMu   sync.RWMutex
	closing    bool
	closeOnce  sync.Once
	closeErr   error
	changed    chan struct{}
	usageWake  chan struct{}
	configWake chan struct{}
	// Test hooks run under mu, and are nil in production.
	fault    func(string) error
	writeWAL func([]byte) (int, error)
}

func OpenStore(path string) (*Store, error) {
	absolute, e := filepath.Abs(path)
	if e != nil {
		return nil, e
	}
	s := &Store{path: absolute, queue: make(chan *stateRequest, maxStoreQueue), done: make(chan struct{}), changed: make(chan struct{}), usageWake: make(chan struct{}, 1), configWake: make(chan struct{}, 1)}
	if e = os.MkdirAll(filepath.Dir(s.path), 0700); e != nil {
		return nil, e
	}
	s.lock = flock.New(s.path + ".lock")
	locked, e := s.lock.TryLock()
	if e != nil {
		return nil, e
	}
	if !locked {
		return nil, errors.New("state already in use by another Agent")
	}
	ok := false
	defer func() {
		if !ok {
			if s.wal != nil {
				s.wal.Close()
			}
			s.lock.Unlock()
		}
	}()
	if e = s.openDurable(); e != nil {
		return nil, e
	}
	ok = true
	go s.writer()
	return s, nil
}
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		s.submitMu.Lock()
		s.closing = true
		close(s.queue)
		s.submitMu.Unlock()
		<-s.done
		s.mu.Lock()
		s.failed = true
		s.notifyChangedLocked()
		if s.wal != nil {
			s.closeErr = s.wal.Close()
		}
		s.mu.Unlock()
		if e := s.lock.Unlock(); s.closeErr == nil {
			s.closeErr = e
		}
	})
	return s.closeErr
}
func (s *Store) submit(r *stateRequest) error {
	r.done = make(chan error, 1)
	s.submitMu.RLock()
	if s.closing {
		s.submitMu.RUnlock()
		return errors.New("state closed")
	}
	if r.ctx != nil {
		select {
		case s.queue <- r:
			s.submitMu.RUnlock()
			// Once queued, wait for the durable outcome, even on cancellation.
			return <-r.done
		case <-r.ctx.Done():
			s.submitMu.RUnlock()
			return r.ctx.Err()
		}
	}
	select {
	case s.queue <- r:
		s.submitMu.RUnlock()
		return <-r.done
	default:
		s.submitMu.RUnlock()
		return errors.New("durable state queue full")
	}
}
func (s *Store) writer() {
	defer close(s.done)
	var carry *stateRequest
	for {
		var r *stateRequest
		if carry != nil {
			r = carry
			carry = nil
		} else {
			var ok bool
			r, ok = <-s.queue
			if !ok {
				return
			}
		}
		batch := []*stateRequest{r}
		if r.event.Kind == "charge" {
			// Drain requests already queued, including arrivals during the last
			// fsync. Do not add a fixed timer delay to every single-flow chunk.
		gather:
			for len(batch) < maxCommitBatch {
				select {
				case next, ok := <-s.queue:
					if !ok {
						break gather
					}
					if next.event.Kind != "charge" {
						carry = next
						break gather
					}
					batch = append(batch, next)
				default:
					break gather
				}
			}
		}
		s.process(batch)
	}
}
func (s *Store) process(batch []*stateRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed {
		for _, r := range batch {
			r.done <- errors.New("durable storage unavailable; repair and restart")
		}
		return
	}
	if batch[0].event.Kind == "checkpoint" {
		e := s.checkpointLocked()
		if e != nil {
			s.failed = true
			s.notifyChangedLocked()
		}
		batch[0].done <- e
		return
	}
	events := make([]stateEvent, 0, len(batch))
	results := make([]error, len(batch))
	reserved := map[string]int64{}
	retiring := map[string]bool{}
	known := map[string]bool{}
	pending := len(s.state.Pending)
	for i, r := range batch {
		if r.ctx != nil && r.ctx.Err() != nil {
			results[i] = r.ctx.Err()
			continue
		}
		if r.event.Kind != "charge" {
			results[i] = s.validateEvent(r.event)
			if results[i] == nil {
				events = append(events, r.event)
			}
			continue
		}
		if r.n < 0 {
			results[i] = errors.New("negative charge")
			continue
		}
		e := s.availableLocked(r.rule, r.until, 0)
		if e == nil && r.rule.Lease != nil {
			lease := r.rule.Lease
			if int64(r.n) > lease.Bytes {
				results[i] = errors.New("payload exceeds lease capacity")
				continue
			}
			remaining := lease.Bytes - s.state.Used[lease.ID] - reserved[lease.ID]
			if retiring[lease.ID] || int64(r.n) > remaining {
				e = errLeaseUnavailable
				if !retiring[lease.ID] {
					events = append(events, stateEvent{Kind: "retire", LeaseID: lease.ID})
					retiring[lease.ID] = true
				}
			}
		}
		if e == nil && pending >= MaxPendingRecords {
			e = errSpoolFull
		}
		if e != nil {
			results[i] = e
			continue
		}
		if r.n == 0 {
			continue
		}
		id, e := randomID()
		if e != nil {
			results[i] = e
			continue
		}
		now := time.Now().UTC()
		u := contract.UsageRecord{ID: id, NodeID: s.state.Identity.NodeID, RuleID: r.rule.ID, LeaseID: r.rule.Lease.ID, EntitlementID: r.rule.Lease.EntitlementID, StartedAt: now, EndedAt: now}
		if r.upload {
			u.UploadBytes = int64(r.n)
		} else {
			u.DownloadBytes = int64(r.n)
		}
		event := stateEvent{Kind: "usage", Usage: &u}
		if _, exists := s.state.Leases[u.LeaseID]; !exists && !known[u.LeaseID] {
			copy := *r.rule.Lease
			event.Lease = &copy
			known[u.LeaseID] = true
		}
		events = append(events, event)
		reserved[u.LeaseID] += int64(r.n)
		pending++
	}
	if len(events) > 0 {
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
			s.notifyChangedLocked()
			for _, r := range batch {
				r.done <- e
			}
			return
		}
	}
	if batch[0].event.Kind != "charge" || len(retiring) > 0 {
		s.notifyChangedLocked()
	}
	if len(s.state.Pending) >= 100 || len(retiring) > 0 {
		s.requestUsage()
	}
	for i, r := range batch {
		r.done <- results[i]
	}
}
func (s *Store) validateEvent(e stateEvent) error {
	switch e.Kind {
	case "identity":
		if e.Identity == nil {
			return errors.New("identity missing")
		}
		if s.state.Identity.NodeID != "" && !reflect.DeepEqual(s.state.Identity, *e.Identity) {
			return errors.New("node identity immutable")
		}
	case "config":
		if e.Config == nil {
			return errors.New("config missing")
		}
		for _, r := range e.Config.Rules {
			if r.Enabled && r.Lease != nil {
				if old, ok := s.state.Leases[r.Lease.ID]; ok && !sameLease(old, *r.Lease) {
					return errors.New("lease identity cannot change budget or expiration")
				}
			}
		}
	case "ack":
	case "retire":
		if e.LeaseID == "" {
			return errors.New("lease ID required")
		}
	case "retire_ack":
		if _, ok := s.state.Retired[e.LeaseID]; !ok {
			return errors.New("lease not stopped")
		}
		for _, u := range s.state.Pending {
			if u.LeaseID == e.LeaseID {
				return errors.New("cannot retire with unconfirmed usage")
			}
		}
	default:
		return errors.New("unknown state mutation")
	}
	return nil
}
func sameLease(a, b contract.Lease) bool {
	return a.ID == b.ID && a.EntitlementID == b.EntitlementID && a.Bytes == b.Bytes && a.ExpiresAt.Equal(b.ExpiresAt)
}
func (s *Store) applyEvent(e stateEvent) error {
	if e.Kind != "usage" {
		if err := s.validateEvent(e); err != nil {
			return err
		}
	}
	switch e.Kind {
	case "identity":
		s.state.Identity = *e.Identity
	case "config":
		active := map[string]bool{}
		for _, r := range e.Config.Rules {
			if r.Enabled && r.Lease != nil {
				active[r.Lease.ID] = true
				s.state.Leases[r.Lease.ID] = *r.Lease
			}
		}
		for _, r := range s.state.Config.Rules {
			if r.Lease != nil && !active[r.Lease.ID] {
				if _, ok := s.state.Retired[r.Lease.ID]; !ok {
					s.state.Retired[r.Lease.ID] = false
				}
			}
		}
		s.state.Config = *e.Config
	case "usage":
		u := e.Usage
		if u == nil || u.ID == "" || u.UploadBytes < 0 || u.DownloadBytes < 0 || u.UploadBytes > int64(^uint64(0)>>1)-u.DownloadBytes {
			return errors.New("invalid WAL usage")
		}
		if _, retired := s.state.Retired[u.LeaseID]; retired {
			return errors.New("WAL usage after retirement")
		}
		lease, ok := s.state.Leases[u.LeaseID]
		if !ok {
			if e.Lease == nil || e.Lease.ID != u.LeaseID {
				return errors.New("WAL lease missing")
			}
			lease = *e.Lease
			s.state.Leases[u.LeaseID] = lease
		}
		n := u.UploadBytes + u.DownloadBytes
		if n > lease.Bytes-s.state.Used[u.LeaseID] || u.NodeID != s.state.Identity.NodeID || u.EntitlementID != lease.EntitlementID || len(s.state.Pending) >= MaxPendingRecords {
			return errors.New("WAL quota/spool invariant violated")
		}
		s.state.Used[u.LeaseID] += n
		s.state.Pending = append(s.state.Pending, *u)
	case "ack":
		set := map[string]bool{}
		for _, id := range e.IDs {
			set[id] = true
		}
		pending := s.state.Pending[:0]
		for _, u := range s.state.Pending {
			if !set[u.ID] {
				pending = append(pending, u)
			}
		}
		s.state.Pending = pending
	case "retire":
		if _, ok := s.state.Retired[e.LeaseID]; !ok {
			s.state.Retired[e.LeaseID] = false
		}
	case "retire_ack":
		s.state.Retired[e.LeaseID] = true
	}
	return nil
}
func (s *Store) Identity() contract.Registered {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.Identity
}
func (s *Store) SetIdentity(v contract.Registered) error {
	return s.submit(&stateRequest{event: stateEvent{Kind: "identity", Identity: &v}})
}
func (s *Store) Config() contract.Config { s.mu.Lock(); defer s.mu.Unlock(); return s.state.Config }
func (s *Store) SetConfig(v contract.Config) error {
	return s.submit(&stateRequest{event: stateEvent{Kind: "config", Config: &v}})
}
func (s *Store) Pending() []contract.UsageRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]contract.UsageRecord(nil), s.state.Pending...)
}
func (s *Store) Confirm(ids []string) error {
	return s.submit(&stateRequest{event: stateEvent{Kind: "ack", IDs: append([]string(nil), ids...)}})
}
func (s *Store) Available(r contract.Rule, until time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.availableLocked(r, until, 0)
}
func (s *Store) availableLocked(r contract.Rule, until time.Time, n int64) error {
	now := time.Now()
	if s.failed {
		return errors.New("durable storage unavailable")
	}
	if !now.Before(until) || r.Lease == nil || !now.Before(r.Lease.ExpiresAt) {
		return errLeaseUnavailable
	}
	if _, retired := s.state.Retired[r.Lease.ID]; retired {
		return errLeaseUnavailable
	}
	if old, ok := s.state.Leases[r.Lease.ID]; ok && !sameLease(old, *r.Lease) {
		return errors.New("lease changed")
	}
	if r.Lease.Bytes <= 0 || n > r.Lease.Bytes-s.state.Used[r.Lease.ID] || s.state.Used[r.Lease.ID] >= r.Lease.Bytes {
		return errLeaseUnavailable
	}
	if len(s.state.Pending) >= MaxPendingRecords {
		return errSpoolFull
	}
	return nil
}

// Charge completes only after the containing WAL batch is synced. No tentative
// reservations or unflushed usage become visible to readers or the sender.
func (s *Store) Charge(r contract.Rule, until time.Time, upload bool, n int) error {
	return s.submit(&stateRequest{event: stateEvent{Kind: "charge"}, rule: r, until: until, upload: upload, n: n})
}

func (s *Store) chargeContext(ctx context.Context, r contract.Rule, until time.Time, upload bool, n int) error {
	return s.submit(&stateRequest{ctx: ctx, event: stateEvent{Kind: "charge"}, rule: r, until: until, upload: upload, n: n})
}

func wake(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (s *Store) requestUsage() { wake(s.usageWake) }

func (s *Store) notifyChangedLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *Store) changes() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.changed
}

// Renewal only stops spending the old allocation; live sockets stay open.
// Pending-record pressure is not a reason to retire an otherwise valid lease.
func (s *Store) renewals() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	var ids []string
	for _, r := range s.state.Config.Rules {
		l := r.Lease
		if !r.Enabled || l == nil {
			continue
		}
		if _, retired := s.state.Retired[l.ID]; retired {
			continue
		}
		used := s.state.Used[l.ID]
		low := used > 0 && l.Bytes-used <= min(l.Bytes/8, 64<<10)
		if low || !now.Add(15*time.Second).Before(minTime(s.state.Config.ValidUntil, l.ExpiresAt)) {
			ids = append(ids, l.ID)
		}
	}
	return ids
}
func randomID() (string, error) {
	b := make([]byte, 16)
	_, e := rand.Read(b)
	return hex.EncodeToString(b), e
}
func (s *Store) Retire(id string) error {
	return s.submit(&stateRequest{event: stateEvent{Kind: "retire", LeaseID: id}})
}
func (s *Store) Retirements() map[string]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := map[string]int64{}
	for id, done := range s.state.Retired {
		if !done {
			m[id] = s.state.Used[id]
		}
	}
	return m
}
func (s *Store) ConfirmRetired(id string) error {
	return s.submit(&stateRequest{event: stateEvent{Kind: "retire_ack", LeaseID: id}})
}

// Checkpoint atomically consolidates all committed usage, including unacked
// records, before replacing the WAL. It is also invoked at the WAL size limit.
func (s *Store) Checkpoint() error {
	return s.submit(&stateRequest{event: stateEvent{Kind: "checkpoint"}})
}
