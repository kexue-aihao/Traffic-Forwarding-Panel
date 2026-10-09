package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

func TestCreditSnapshotRejectsOvercommittedRecovery(t *testing.T) {
	s, r, _ := creditRule(t)
	if e := s.Checkpoint(); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(s.path)
	if e != nil {
		t.Fatal(e)
	}
	var c checkpoint
	if e := json.Unmarshal(b, &c); e != nil {
		t.Fatal(e)
	}
	var state diskState
	if e := json.Unmarshal(c.State, &state); e != nil {
		t.Fatal(e)
	}
	state.Used[r.Lease.ID] = r.Lease.Bytes
	c.State, _ = json.Marshal(state)
	c.CRC = checksum(c.Sequence, c.State)
	b, _ = json.Marshal(c)
	s.Close()
	if e := os.WriteFile(s.path, b, 0600); e != nil {
		t.Fatal(e)
	}
	if next, e := OpenStore(s.path); e == nil {
		next.Close()
		t.Fatal("overcommitted recovery granted fresh quota")
	}
}

func TestLeaseHistoryCheckpointKeepsLiveTombstonesAndPendingFacts(t *testing.T) {
	s, _, _ := setup(t)
	s.mu.Lock()
	for _, id := range []string{"expired", "live", "pending"} {
		until := time.Now().Add(-time.Minute)
		if id == "live" {
			until = time.Now().Add(time.Minute)
		}
		s.state.Leases[id] = contract.Lease{ID: id, Bytes: 100, EntitlementID: "ent", ExpiresAt: until}
		s.state.Used[id] = 10
		s.state.Retired[id] = id != "pending"
	}
	s.state.Pending = []contract.UsageRecord{{ID: "unacked", LeaseID: "pending", NodeID: "node", EntitlementID: "ent", UploadBytes: 10, StartedAt: time.Now().Add(-2 * time.Minute), EndedAt: time.Now().Add(-time.Minute)}}
	s.mu.Unlock()
	if e := s.Checkpoint(); e != nil {
		t.Fatal(e)
	}
	s = reopen(t, s)
	if used(s, "expired") != 0 || used(s, "live") != 10 || len(s.Pending()) != 1 {
		t.Fatal("history collection lost protection or pending accounting")
	}
	if e := s.Charge(contract.Rule{Lease: &contract.Lease{ID: "live", Bytes: 100, EntitlementID: "ent", ExpiresAt: s.state.Leases["live"].ExpiresAt}}, time.Now().Add(time.Minute), true, 1); e == nil {
		t.Fatal("unexpired retired allocation reused")
	}
}

func TestRetainedLeaseCapacityStopsNewAllocations(t *testing.T) {
	s, _, c := setup(t)
	s.mu.Lock()
	for i := range maxRetainedLeases {
		id := fmt.Sprintf("retained-%d", i)
		s.state.Leases[id] = contract.Lease{ID: id, EntitlementID: "ent", Bytes: 1, ExpiresAt: time.Now().Add(time.Minute)}
	}
	s.mu.Unlock()
	if e := s.Charge(testRule("127.0.0.1:1"), c.ValidUntil, true, 1); !errors.Is(e, errSpoolFull) {
		t.Fatal("history capacity did not bound new allocation", e)
	}
}

func TestStaleCreditRefillCannotReviveRevokedConfiguration(t *testing.T) {
	s, r, until := creditRule(t)
	c := s.Config()
	c.Rules = nil
	if e := s.SetConfig(c); e != nil {
		t.Fatal(e)
	}
	if e := s.submit(&stateRequest{event: stateEvent{Kind: "credit_reserve"}, rule: r, until: until, upload: true}); !errors.Is(e, errLeaseUnavailable) {
		t.Fatal("stale request created credit", e)
	}
	if e := s.TryUDPCharge(r, until, true, 0); e == nil {
		t.Fatal("empty packet bypassed revoked credit authorization")
	}
}

func TestPrefetchConflictRefreshesConfigButDoesNotHideForbidden(t *testing.T) {
	for _, status := range []int{http.StatusConflict, http.StatusForbidden} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			s, _, c := setup(t)
			r := testRule("127.0.0.1:1")
			r.LeasePipeline = true
			c.Rules = []contract.Rule{r}
			if e := s.SetConfig(c); e != nil {
				t.Fatal(e)
			}
			if e := s.Charge(r, c.ValidUntil, true, 900000); e != nil {
				t.Fatal(e)
			}
			s.rates = map[string]leaseRateSample{r.ID: {at: time.Now().Add(-time.Second), used: map[string]int64{r.Lease.ID: 0}}}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				switch req.URL.Path {
				case "/api/v1/agent/usage":
					var batch contract.UsageBatch
					json.NewDecoder(req.Body).Decode(&batch)
					ids := []string{}
					for _, u := range batch.Records {
						ids = append(ids, u.ID)
					}
					json.NewEncoder(w).Encode(map[string]any{"accepted": ids})
				case "/api/v1/agent/leases/prefetch":
					w.WriteHeader(status)
				default:
					t.Error("unexpected request", req.URL.Path)
					w.WriteHeader(500)
				}
			}))
			defer srv.Close()
			a := Agent{Store: s, URL: srv.URL, HTTP: srv.Client()}
			err := a.syncUsage(context.Background())
			if status == http.StatusForbidden {
				if err == nil {
					t.Fatal("authorization failure hidden")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-s.configWake:
			default:
				t.Fatal("stale prefetch did not refresh config")
			}
			if e := s.Retire(r.Lease.ID); e != nil {
				t.Fatal(e)
			}
			if len(s.prefetches()) != 0 {
				t.Fatal("retired allocation prefetched")
			}
		})
	}
}
