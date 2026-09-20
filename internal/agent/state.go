package agent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

const MaxPendingRecords = 4096

type diskState struct {
	Identity contract.Registered       `json:"identity"`
	Config   contract.Config           `json:"config"`
	Used     map[string]int64          `json:"used"`
	Pending  []contract.UsageRecord    `json:"pending"`
	Retired  map[string]bool           `json:"retired"`
	Leases   map[string]contract.Lease `json:"leases"`
}
type Store struct {
	mu     sync.Mutex
	path   string
	state  diskState
	failed bool
	lock   *flock.Flock
}

func OpenStore(path string) (*Store, error) {
	s := &Store{path: path}
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return nil, e
	}
	s.lock = flock.New(path + ".lock")
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
			s.lock.Unlock()
		}
	}()
	b, e := os.ReadFile(path)
	if e == nil {
		if e = json.Unmarshal(b, &s.state); e != nil {
			return nil, e
		}
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	if s.state.Used == nil {
		s.state.Used = map[string]int64{}
	}
	if s.state.Retired == nil {
		s.state.Retired = map[string]bool{}
	}
	if s.state.Leases == nil {
		s.state.Leases = map[string]contract.Lease{}
	}
	if len(s.state.Pending) > MaxPendingRecords {
		return nil, errors.New("usage spool over limit")
	}
	ok = true
	return s, nil
}
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failed = true
	return s.lock.Unlock()
}
func (s *Store) saveLocked() error {
	if s.failed {
		return errors.New("state storage failed; restart after repairing disk")
	}
	if e := os.MkdirAll(filepath.Dir(s.path), 0700); e != nil {
		s.failed = true
		return e
	}
	b, e := json.Marshal(s.state)
	if e != nil {
		return e
	}
	f, e := os.OpenFile(s.path+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if e != nil {
		s.failed = true
		return e
	}
	_, e = f.Write(b)
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e == nil {
		e = os.Rename(s.path+".tmp", s.path)
	}
	if e == nil && runtime.GOOS != "windows" {
		dir, err := os.Open(filepath.Dir(s.path))
		if err == nil {
			err = dir.Sync()
			dir.Close()
		}
		e = err
	}
	if e != nil {
		s.failed = true
	}
	return e
}
func (s *Store) Identity() contract.Registered {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.Identity
}
func (s *Store) SetIdentity(v contract.Registered) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Identity = v
	return s.saveLocked()
}
func (s *Store) Config() contract.Config { s.mu.Lock(); defer s.mu.Unlock(); return s.state.Config }
func (s *Store) SetConfig(v contract.Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	active := map[string]bool{}
	for _, r := range v.Rules {
		if r.Enabled && r.Lease != nil {
			active[r.Lease.ID] = true
			if old, ok := s.state.Leases[r.Lease.ID]; ok && !reflect.DeepEqual(old, *r.Lease) {
				return errors.New("lease identity cannot change budget or expiration")
			}
		}
	}
	for _, r := range s.state.Config.Rules {
		if r.Lease != nil && !active[r.Lease.ID] {
			if _, ok := s.state.Retired[r.Lease.ID]; !ok {
				s.state.Retired[r.Lease.ID] = false
			}
		}
	}
	for _, r := range v.Rules {
		if r.Enabled && r.Lease != nil {
			s.state.Leases[r.Lease.ID] = *r.Lease
		}
	}
	s.state.Config = v
	return s.saveLocked()
}
func (s *Store) Pending() []contract.UsageRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]contract.UsageRecord(nil), s.state.Pending...)
}
func (s *Store) Confirm(ids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	set := map[string]bool{}
	for _, id := range ids {
		set[id] = true
	}
	pending := s.state.Pending[:0]
	for _, v := range s.state.Pending {
		if !set[v.ID] {
			pending = append(pending, v)
		}
	}
	s.state.Pending = pending
	return s.saveLocked()
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
		return errors.New("configuration or lease expired")
	}
	if _, retired := s.state.Retired[r.Lease.ID]; retired {
		return errors.New("lease retired")
	}
	if r.Lease.Bytes <= 0 || n > r.Lease.Bytes-s.state.Used[r.Lease.ID] || s.state.Used[r.Lease.ID] >= r.Lease.Bytes {
		return errors.New("lease budget exhausted")
	}
	if len(s.state.Pending) >= MaxPendingRecords {
		return errors.New("usage spool full")
	}
	return nil
}

// Charge persists before forwarding. Bytes accepted into forwarding buffers are
// charged even if the remote peer later fails, ensuring no unaccounted bytes.
func (s *Store) Charge(r contract.Rule, until time.Time, upload bool, n int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n < 0 {
		return errors.New("negative charge")
	}
	if e := s.availableLocked(r, until, int64(n)); e != nil {
		if r.Lease != nil && int64(n) > r.Lease.Bytes-s.state.Used[r.Lease.ID] {
			if _, ok := s.state.Retired[r.Lease.ID]; !ok {
				s.state.Retired[r.Lease.ID] = false
				if se := s.saveLocked(); se != nil {
					return se
				}
			}
		}
		return e
	}
	if n == 0 {
		return nil
	}
	id, e := randomID()
	if e != nil {
		return e
	}
	now := time.Now().UTC()
	u := contract.UsageRecord{ID: id, NodeID: s.state.Identity.NodeID, RuleID: r.ID, LeaseID: r.Lease.ID, EntitlementID: r.Lease.EntitlementID, StartedAt: now, EndedAt: now}
	if upload {
		u.UploadBytes = int64(n)
	} else {
		u.DownloadBytes = int64(n)
	}
	s.state.Used[r.Lease.ID] += int64(n)
	s.state.Pending = append(s.state.Pending, u)
	return s.saveLocked()
}
func randomID() (string, error) {
	b := make([]byte, 16)
	_, e := rand.Read(b)
	return hex.EncodeToString(b), e
}
func (s *Store) Retire(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.state.Retired[id]; ok {
		return nil
	}
	s.state.Retired[id] = false
	return s.saveLocked()
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
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Retired[id] = true
	return s.saveLocked()
}
