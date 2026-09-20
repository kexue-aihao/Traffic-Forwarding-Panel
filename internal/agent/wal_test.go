package agent

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

func reopen(t *testing.T, s *Store) *Store {
	t.Helper()
	path := s.path
	s.Close()
	next, e := OpenStore(path)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { next.Close() })
	return next
}
func used(s *Store, id string) int64 { s.mu.Lock(); defer s.mu.Unlock(); return s.state.Used[id] }
func TestWALChargeAndCheckpointCrashBoundaries(t *testing.T) {
	for _, point := range []string{"append_before", "append_after", "sync_before", "sync_after"} {
		t.Run(point, func(t *testing.T) {
			s, _, c := setup(t)
			r := testRule("127.0.0.1:1")
			s.mu.Lock()
			s.fault = func(p string) error {
				if p == point {
					return errors.New("simulated crash")
				}
				return nil
			}
			s.mu.Unlock()
			if e := s.Charge(r, c.ValidUntil, true, 12); e == nil {
				t.Fatal("fault ignored")
			}
			if e := s.Charge(r, c.ValidUntil, true, 1); e == nil {
				t.Fatal("storage failure did not stop forwarding")
			}
			next := reopen(t, s)
			want := int64(12)
			if point == "append_before" {
				want = 0
			}
			if used(next, r.Lease.ID) != want || len(next.Pending()) != int(want/12) {
				t.Fatal("complete uncertain record lost or budget refilled")
			}
		})
	}
	for _, point := range []string{"snapshot_before_write", "snapshot_after_write", "snapshot_after_sync", "snapshot_after_rename", "snapshot_after_dirsync", "rotate_after_write", "rotate_after_sync", "rotate_before_rename", "rotate_after_rename", "rotate_after_dirsync"} {
		t.Run(point, func(t *testing.T) {
			s, _, c := setup(t)
			r := testRule("127.0.0.1:1")
			if e := s.Charge(r, c.ValidUntil, true, 12); e != nil {
				t.Fatal(e)
			}
			original := s.Pending()[0]
			s.mu.Lock()
			s.fault = func(p string) error {
				if p == point {
					return errors.New("simulated crash")
				}
				return nil
			}
			s.mu.Unlock()
			if e := s.Checkpoint(); e == nil {
				t.Fatal("fault ignored")
			}
			next := reopen(t, s)
			if used(next, r.Lease.ID) != 12 || len(next.Pending()) != 1 || next.Pending()[0].ID != original.ID {
				t.Fatal("checkpoint lost unacknowledged usage")
			}
			if e := next.Charge(r, c.ValidUntil, false, 3); e != nil {
				t.Fatal(e)
			}
			next = reopen(t, next)
			if used(next, r.Lease.ID) != 15 || len(next.Pending()) != 2 {
				t.Fatal("post-recovery sequence did not persist")
			}
		})
	}
}
func TestWALTornTailAndInteriorCorruption(t *testing.T) {
	t.Run("partial final record", func(t *testing.T) {
		s, _, c := setup(t)
		r := testRule("127.0.0.1:1")
		if e := s.Charge(r, c.ValidUntil, true, 7); e != nil {
			t.Fatal(e)
		}
		s.mu.Lock()
		s.writeWAL = func(p []byte) (int, error) {
			n := len(p) / 2
			written, e := s.wal.Write(p[:n])
			if e != nil {
				return written, e
			}
			return written, errors.New("partial disk write")
		}
		s.mu.Unlock()
		if e := s.Charge(r, c.ValidUntil, true, 8); e == nil {
			t.Fatal("partial write accepted")
		}
		next := reopen(t, s)
		if used(next, r.Lease.ID) != 7 || len(next.Pending()) != 1 {
			t.Fatal("torn uncommitted tail changed budget")
		}
		if e := next.Charge(r, c.ValidUntil, true, 2); e != nil {
			t.Fatal(e)
		}
		next = reopen(t, next)
		if used(next, r.Lease.ID) != 9 {
			t.Fatal("sequence after tail repair invalid")
		}
	})
	for _, where := range []string{"middle CRC", "last complete CRC", "middle length", "middle sequence"} {
		t.Run(where, func(t *testing.T) {
			s, _, c := setup(t)
			r := testRule("127.0.0.1:1")
			for i := 0; i < 2; i++ {
				if e := s.Charge(r, c.ValidUntil, true, 7); e != nil {
					t.Fatal(e)
				}
			}
			path := s.path
			s.Close()
			b, e := os.ReadFile(path + ".wal")
			if e != nil {
				t.Fatal(e)
			}
			offset := walHeaderSize
			offset += recordHeaderSize + int(binary.BigEndian.Uint32(b[offset+4:offset+8]))
			second := offset
			last := second + recordHeaderSize + int(binary.BigEndian.Uint32(b[second+4:second+8]))
			switch where {
			case "middle CRC":
				b[second+recordHeaderSize+10] ^= 1
			case "last complete CRC":
				b[last+recordHeaderSize+10] ^= 1
			case "middle length":
				binary.BigEndian.PutUint32(b[second+4:second+8], maxWALRecord)
			case "middle sequence":
				b[second+15] ^= 1
			}
			if e = os.WriteFile(path+".wal", b, 0600); e != nil {
				t.Fatal(e)
			}
			if next, e := OpenStore(path); e == nil {
				next.Close()
				t.Fatal("corruption silently discarded")
			}
			after, _ := os.ReadFile(path + ".wal")
			if len(after) != len(b) {
				t.Fatal("corrupt interior was truncated")
			}
		})
	}
}
func TestWALMigrationAcknowledgementAndRetirement(t *testing.T) {
	s, _, c := setup(t)
	r := testRule("127.0.0.1:1")
	c.Rules = []contract.Rule{r}
	if e := s.SetConfig(c); e != nil {
		t.Fatal(e)
	}
	if e := s.Charge(r, c.ValidUntil, true, 12); e != nil {
		t.Fatal(e)
	}
	original := s.Pending()[0]
	s.mu.Lock()
	legacy, e := json.Marshal(s.state)
	s.mu.Unlock()
	if e != nil {
		t.Fatal(e)
	}
	path := s.path
	s.Close()
	if e = os.Remove(path + ".wal"); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(path, legacy, 0600); e != nil {
		t.Fatal(e)
	}
	next, e := OpenStore(path)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { next.Close() })
	if used(next, r.Lease.ID) != 12 || next.Pending()[0].ID != original.ID {
		t.Fatal("legacy migration lost facts")
	}
	checkpointBytes, _ := os.ReadFile(path)
	var snapshot checkpoint
	if e = json.Unmarshal(checkpointBytes, &snapshot); e != nil || snapshot.Version != walVersion {
		t.Fatal("legacy not migrated")
	}
	if e = next.Retire(r.Lease.ID); e != nil {
		t.Fatal(e)
	}
	if e = next.ConfirmRetired(r.Lease.ID); e == nil {
		t.Fatal("retirement allowed before usage acknowledgement")
	}
	if e = next.Confirm([]string{original.ID}); e != nil {
		t.Fatal(e)
	}
	if e = next.ConfirmRetired(r.Lease.ID); e != nil {
		t.Fatal(e)
	}
	next = reopen(t, next)
	if len(next.Pending()) != 0 || len(next.Retirements()) != 0 || used(next, r.Lease.ID) != 12 {
		t.Fatal("ACK/retirement replay failed")
	}
	if e = next.Charge(r, c.ValidUntil, true, 1); e == nil {
		t.Fatal("retired lease revived")
	}
	if e = next.Checkpoint(); e != nil {
		t.Fatal(e)
	}
	next.Close()
	if e = os.Remove(path + ".wal"); e != nil {
		t.Fatal(e)
	}
	if restored, e := OpenStore(path); e == nil {
		restored.Close()
		t.Fatal("incomplete backup accepted")
	}
}
func TestWALGroupCommitNoEarlySendAndConcurrentBudget(t *testing.T) {
	s, _, c := setup(t)
	r := testRule("127.0.0.1:1")
	r.Lease.Bytes = 32
	var syncs atomic.Int64
	s.mu.Lock()
	s.fault = func(p string) error {
		if p == "sync_after" {
			syncs.Add(1)
		}
		return nil
	}
	s.mu.Unlock()
	start := make(chan struct{})
	var wg sync.WaitGroup
	var success atomic.Int64
	for i := 0; i < 128; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if s.Charge(r, c.ValidUntil, true, 1) == nil {
				success.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if success.Load() != 32 || used(s, r.Lease.ID) != 32 || len(s.Pending()) != 32 {
		t.Fatalf("concurrent budget %d / %d", success.Load(), used(s, r.Lease.ID))
	}
	if syncs.Load() >= 32 {
		t.Fatalf("group commit did not combine writes: %d", syncs.Load())
	}
	next := reopen(t, s)
	if used(next, r.Lease.ID) != 32 {
		t.Fatal("concurrent durable budget mismatch")
	}
	s, _, c = setup(t)
	r = testRule("127.0.0.1:1")
	entered := make(chan struct{})
	release := make(chan struct{})
	s.mu.Lock()
	s.fault = func(p string) error {
		if p == "sync_before" {
			close(entered)
			<-release
		}
		return nil
	}
	s.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- s.Charge(r, c.ValidUntil, true, 1) }()
	<-entered
	select {
	case <-done:
		t.Fatal("Charge returned before fsync")
	default:
	}
	close(release)
	if e := <-done; e != nil {
		t.Fatal(e)
	}
}
func TestWALQueueAndSpoolBounds(t *testing.T) {
	s, _, c := setup(t)
	r := testRule("127.0.0.1:1")
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	s.mu.Lock()
	s.fault = func(p string) error {
		if p == "sync_before" {
			once.Do(func() { close(entered); <-release })
		}
		return nil
	}
	s.mu.Unlock()
	done := make(chan error, maxStoreQueue+1)
	go func() { done <- s.Charge(r, c.ValidUntil, true, 1) }()
	<-entered
	for i := 0; i < maxStoreQueue; i++ {
		go func() { done <- s.Charge(r, c.ValidUntil, true, 1) }()
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(s.queue) < maxStoreQueue && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if len(s.queue) != maxStoreQueue {
		close(release)
		t.Fatal("queue did not fill")
	}
	if e := s.Charge(r, c.ValidUntil, true, 1); e == nil {
		close(release)
		t.Fatal("queue allowed unlimited requests")
	}
	close(release)
	for i := 0; i <= maxStoreQueue; i++ {
		if e := <-done; e != nil {
			t.Fatal(e)
		}
	}
	s.mu.Lock()
	for len(s.state.Pending) < MaxPendingRecords {
		s.state.Pending = append(s.state.Pending, contract.UsageRecord{})
	}
	s.mu.Unlock()
	if e := s.Charge(r, c.ValidUntil, true, 1); e == nil {
		t.Fatal("spool exceeded limit")
	}
}

func TestWALAckRetireCrashAndAutomaticCheckpoint(t *testing.T) {
	for _, kind := range []string{"ack", "retire", "retire_ack"} {
		for _, point := range []string{"append_before", "sync_after"} {
			t.Run(kind+"/"+point, func(t *testing.T) {
				s, _, c := setup(t)
				r := testRule("127.0.0.1:1")
				if e := s.Charge(r, c.ValidUntil, true, 12); e != nil {
					t.Fatal(e)
				}
				id := s.Pending()[0].ID
				if kind == "retire_ack" {
					if e := s.Retire(r.Lease.ID); e != nil {
						t.Fatal(e)
					}
					if e := s.Confirm([]string{id}); e != nil {
						t.Fatal(e)
					}
				}
				s.mu.Lock()
				s.fault = func(p string) error {
					if p == point {
						return errors.New("injected power loss")
					}
					return nil
				}
				s.mu.Unlock()
				var e error
				switch kind {
				case "ack":
					e = s.Confirm([]string{id})
				case "retire":
					e = s.Retire(r.Lease.ID)
				case "retire_ack":
					e = s.ConfirmRetired(r.Lease.ID)
				}
				if e == nil {
					t.Fatal("fault ignored")
				}
				next := reopen(t, s)
				if used(next, r.Lease.ID) != 12 {
					t.Fatal("budget forgotten")
				}
				switch kind {
				case "ack":
					want := 1
					if point == "sync_after" {
						want = 0
					}
					if len(next.Pending()) != want {
						t.Fatal("ACK crash lost or revived wrong record")
					}
				case "retire":
					if point == "sync_after" && next.Available(r, c.ValidUntil) == nil {
						t.Fatal("durable retire marker revived")
					}
					if len(next.Pending()) != 1 {
						t.Fatal("retirement deleted unacked usage")
					}
				case "retire_ack":
					if next.Available(r, c.ValidUntil) == nil {
						t.Fatal("lease revived")
					}
					want := 1
					if point == "sync_after" {
						want = 0
					}
					if len(next.Retirements()) != want {
						t.Fatal("retirement acknowledgment lost")
					}
				}
			})
		}
	}
	s, _, c := setup(t)
	r := testRule("127.0.0.1:1")
	s.mu.Lock()
	s.walBytes = walCheckpointBytes
	s.mu.Unlock()
	if e := s.Charge(r, c.ValidUntil, true, 5); e != nil {
		t.Fatal(e)
	}
	s.mu.Lock()
	size := s.walBytes
	s.mu.Unlock()
	if size != walHeaderSize {
		t.Fatal("automatic checkpoint did not rotate")
	}
	next := reopen(t, s)
	if used(next, r.Lease.ID) != 5 || len(next.Pending()) != 1 {
		t.Fatal("automatic checkpoint lost event")
	}
}

// This benchmark deliberately starts with 3000 pending records to expose the
// former O(pending) whole-JSON rewrite. The legacy branch performs the same
// JSON write, fsync, rename and directory sync as the pre-WAL implementation.
func BenchmarkDurableUsage(b *testing.B) {
	for _, mode := range []string{"legacy-json", "wal"} {
		for _, workers := range []int{1, 32} {
			b.Run(fmt.Sprintf("%s/workers=%d", mode, workers), func(b *testing.B) {
				path := filepath.Join(b.TempDir(), "state.json")
				s, e := OpenStore(path)
				if e != nil {
					b.Fatal(e)
				}
				defer s.Close()
				if e = s.SetIdentity(contract.Registered{NodeID: "node", Token: "test"}); e != nil {
					b.Fatal(e)
				}
				rule := testRule("127.0.0.1:1")
				rule.Lease.Bytes = 1 << 60
				until := time.Now().Add(time.Hour)
				s.mu.Lock()
				for i := 0; i < 3000; i++ {
					s.state.Pending = append(s.state.Pending, contract.UsageRecord{ID: fmt.Sprint("old-", i), NodeID: "node", RuleID: rule.ID, LeaseID: rule.Lease.ID, EntitlementID: rule.Lease.EntitlementID, StartedAt: time.Now(), EndedAt: time.Now(), UploadBytes: 1})
				}
				s.state.Used[rule.Lease.ID] = 3000
				s.state.Leases[rule.Lease.ID] = *rule.Lease
				s.mu.Unlock()
				if e = s.Checkpoint(); e != nil {
					b.Fatal(e)
				}
				var index atomic.Int64
				var legacyMu sync.Mutex
				b.ResetTimer()
				var wg sync.WaitGroup
				for worker := 0; worker < workers; worker++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						for {
							n := index.Add(1)
							if n > int64(b.N) {
								return
							}
							if mode == "wal" {
								if e := s.Charge(rule, until, true, 32*1024); e != nil {
									b.Error(e)
									return
								}
							} else {
								legacyMu.Lock()
								s.state.Used[rule.Lease.ID] += 32 * 1024
								s.state.Pending = append(s.state.Pending, contract.UsageRecord{ID: fmt.Sprint("new-", n), NodeID: "node", RuleID: rule.ID, LeaseID: rule.Lease.ID, EntitlementID: rule.Lease.EntitlementID, StartedAt: time.Now(), EndedAt: time.Now(), UploadBytes: 32 * 1024})
								data, e := json.Marshal(s.state)
								if e == nil {
									var f *os.File
									f, e = os.OpenFile(path+".legacy.tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
									if e == nil {
										_, e = f.Write(data)
										if e == nil {
											e = f.Sync()
										}
										f.Close()
									}
								}
								if e == nil {
									e = os.Rename(path+".legacy.tmp", path+".legacy")
								}
								if e == nil {
									e = syncDirectory(path)
								}
								legacyMu.Unlock()
								if e != nil {
									b.Error(e)
									return
								}
							}
						}
					}()
				}
				wg.Wait()
				b.StopTimer()
				b.SetBytes(32 * 1024)
			})
		}
	}
}
