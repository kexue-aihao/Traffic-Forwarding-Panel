package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/probe"
)

func TestManagedUpgradeEnablesTCPGrowthOnlyAfterJournalRemoval(t *testing.T) {
	s, runtime, cfg := setup(t)
	u := &Upgrader{Journal: filepath.Join(t.TempDir(), "upgrade.json")}
	if err := u.save(upgradeJournal{Phase: "staged"}); err != nil {
		t.Fatal(err)
	}
	acked := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent/config":
			json.NewEncoder(w).Encode(cfg)
		case "/api/v1/agent/ack":
			wake(acked)
			w.WriteHeader(http.StatusNoContent)
		default:
			io.WriteString(w, "{}")
		}
	}))
	defer server.Close()
	a := &Agent{URL: server.URL, HTTP: server.Client(), Store: s, Runtime: runtime, Probe: &probe.Collector{}, PollInterval: 20 * time.Millisecond, Upgrader: u}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	defer func() { cancel(); <-done }()
	select {
	case <-acked:
	case <-ctx.Done():
		t.Fatal("managed worker did not initialize")
	}
	if !s.pauseTCPGrowth.Load() {
		t.Fatal("upgrade grew windows before supervisor committed success")
	}
	if err := os.Remove(u.Journal); err != nil {
		t.Fatal(err)
	}
	for s.pauseTCPGrowth.Load() && ctx.Err() == nil {
		time.Sleep(time.Millisecond)
	}
	if s.pauseTCPGrowth.Load() {
		t.Fatal("successful activation did not enable window growth")
	}
}

func TestStaleConfigAckRefreshesWithoutWaitingForPoll(t *testing.T) {
	s, runtime, cfg := setup(t)
	var reads, acks atomic.Int64
	confirmed := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent/config":
			current := cfg
			if reads.Add(1) > 1 {
				current.Version++
			}
			json.NewEncoder(w).Encode(current)
		case "/api/v1/agent/ack":
			if acks.Add(1) == 1 {
				w.WriteHeader(http.StatusConflict)
			} else {
				wake(confirmed)
				w.WriteHeader(http.StatusNoContent)
			}
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer server.Close()
	a := &Agent{URL: server.URL, HTTP: server.Client(), Store: s, Runtime: runtime, Probe: &probe.Collector{}, PollInterval: time.Hour}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	defer func() { cancel(); <-done }()
	select {
	case <-confirmed:
		if runtime.Version() != cfg.Version+1 {
			t.Fatal("stale ACK did not apply the current configuration")
		}
	case <-ctx.Done():
		t.Fatal("stale ACK waited for the next periodic config poll")
	}
}

func TestBlockedUsageDoesNotBlockLeasePrefetch(t *testing.T) {
	s, runtime, cfg := setup(t)
	rule := testRule("127.0.0.1:1")
	rule.LeasePipeline = true
	cfg.Rules = []contract.Rule{rule}
	if err := runtime.Apply(cfg, true); err != nil {
		t.Fatal(err)
	}
	if err := s.Charge(rule, cfg.ValidUntil, false, 900000); err != nil {
		t.Fatal(err)
	}
	s.rates = map[string]leaseRateSample{rule.ID: {at: time.Now().Add(-time.Second), used: map[string]int64{rule.Lease.ID: 0}}}
	blocked, prefetched := make(chan struct{}), make(chan struct{})
	var usageOnce, prefetchOnce sync.Once
	var nextMu sync.Mutex
	next := cfg
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent/config":
			nextMu.Lock()
			defer nextMu.Unlock()
			json.NewEncoder(w).Encode(next)
		case "/api/v1/agent/usage":
			io.Copy(io.Discard, r.Body)
			usageOnce.Do(func() { close(blocked) })
			<-r.Context().Done()
		case "/api/v1/agent/leases/prefetch":
			select {
			case <-blocked:
			case <-r.Context().Done():
				return
			}
			nextMu.Lock()
			standby := *rule.Lease
			standby.ID, standby.Bytes = "next-allocation", 64<<20
			updated := rule
			updated.StandbyLease = &standby
			next.Rules = []contract.Rule{updated}
			next.Version++
			nextMu.Unlock()
			json.NewEncoder(w).Encode(map[string]string{"lease_id": standby.ID})
			prefetchOnce.Do(func() { close(prefetched) })
		case "/api/v1/agent/probe", "/api/v1/agent/ack":
			w.WriteHeader(http.StatusNoContent)
		default:
			io.WriteString(w, "{}")
		}
	}))
	defer server.Close()
	a := &Agent{URL: server.URL, HTTP: server.Client(), Store: s, Runtime: runtime, Probe: &probe.Collector{}, PollInterval: time.Hour}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	defer func() { cancel(); <-done }()
	select {
	case <-prefetched:
	case <-ctx.Done():
		t.Fatal("blocked usage starved lease prefetch")
	}
	for ctx.Err() == nil {
		current := s.Config()
		if len(current.Rules) == 1 && current.Rules[0].StandbyLease != nil {
			if len(s.Pending()) == 0 {
				t.Fatal("prefetch discarded unacknowledged usage")
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("prefetch did not wake configuration refresh while usage was blocked")
}

func TestPrefetchUsesFirstActiveWindowSample(t *testing.T) {
	s, b, rule, cfg := tcpCreditBinding(t, 16<<20)
	rule.LeasePipeline = true
	cfg.Version++
	cfg.Rules = []contract.Rule{rule}
	if err := b.runtime.Apply(cfg, true); err != nil {
		t.Fatal(err)
	}
	if err := b.chargeCurrent(context.Background(), rule.ID, "tcp", false, 1); err != nil {
		t.Fatal(err)
	}
	requests := s.prefetches()
	// The first observed consumption is below the meaningful-use threshold.
	if len(requests) != 0 {
		t.Fatal("tiny transfer prematurely reserved another allocation")
	}
	for range 32 {
		if err := b.chargeCurrent(context.Background(), rule.ID, "tcp", false, 32<<10); err != nil {
			t.Fatal(err)
		}
	}
	// Observe actual active credit with no prior rate sample or usage POST.
	time.Sleep(20 * time.Millisecond)
	s.mu.Lock()
	s.rates = nil
	s.mu.Unlock()
	requests = s.prefetches()
	if len(requests) != 1 || requests[0].RawBudget <= 16<<20 || requests[0].RawBudget > 256<<20 {
		t.Fatal("first meaningful sample did not request enough runway", requests)
	}
}
