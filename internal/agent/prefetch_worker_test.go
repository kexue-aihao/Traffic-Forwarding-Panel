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

func TestLeaseConfigAppliesWhileInitialAckIsBlocked(t *testing.T) {
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
	next := cfg
	next.Version++
	standby := *rule.Lease
	standby.ID, standby.Bytes = "inline-standby", 64<<20
	updated := rule
	updated.StandbyLease = &standby
	next.Rules = []contract.Rule{updated}
	blocked := make(chan struct{})
	settled := make(chan struct{}, 1)
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent/config":
			json.NewEncoder(w).Encode(cfg)
		case "/api/v1/agent/ack":
			io.Copy(io.Discard, r.Body)
			once.Do(func() { close(blocked) })
			<-r.Context().Done()
		case "/api/v1/agent/usage":
			var batch contract.UsageBatch
			if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			accepted := make([]string, 0, len(batch.Records))
			for _, record := range batch.Records {
				accepted = append(accepted, record.ID)
			}
			json.NewEncoder(w).Encode(map[string]any{"accepted": accepted})
			wake(settled)
		case "/api/v1/agent/leases/prefetch":
			if r.Header.Get("X-TFP-Lease-Config") != "1" {
				t.Error("missing optional config negotiation")
			}
			select {
			case <-blocked:
			case <-r.Context().Done():
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"lease_id": standby.ID, "config": next})
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer server.Close()
	a := &Agent{URL: server.URL, HTTP: server.Client(), Store: s, Runtime: runtime, PollInterval: time.Hour}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	defer func() { cancel(); <-done }()
	for runtime.Version() != next.Version && ctx.Err() == nil {
		time.Sleep(time.Millisecond)
	}
	if runtime.Version() != next.Version || s.Config().Rules[0].StandbyLease == nil {
		t.Fatal("initial GET/ACK blocked funded standby application")
	}
	select {
	case <-settled:
	case <-ctx.Done():
		t.Fatal("initial GET/ACK blocked usage settlement")
	}
	for len(s.Pending()) != 0 && ctx.Err() == nil {
		time.Sleep(time.Millisecond)
	}
	if len(s.Pending()) != 0 {
		t.Fatal("initial GET/ACK blocked durable usage confirmation")
	}
	// Optional snapshots use the normal identity/validation barrier.
	invalid := next
	invalid.Version++
	invalid.NodeID = "other-node"
	if err := a.applyLeaseConfig(&invalid); err == nil {
		t.Fatal("inline snapshot bypassed node identity validation")
	}
	if runtime.Version() != next.Version {
		t.Fatal("invalid inline snapshot changed the active policy")
	}
	if err := a.applyLeaseConfig(&cfg); err != nil || runtime.Version() != next.Version {
		t.Fatal("late snapshot rolled back newer policy", err)
	}
}

func TestPendingPrefetchKeepsPrimaryEligibleUntilResponse(t *testing.T) {
	s, b, rule, cfg := tcpCreditBinding(t, 16<<20)
	rule.LeasePipeline = true
	cfg.Version++
	cfg.Rules = []contract.Rule{rule}
	if err := b.runtime.Apply(cfg, true); err != nil {
		t.Fatal(err)
	}
	if err := s.Charge(rule, cfg.ValidUntil, false, int(rule.Lease.Bytes-1)); err != nil {
		t.Fatal(err)
	}
	if got := s.renewals(); len(got) != 1 || got[0] != rule.Lease.ID {
		t.Fatal("nearly consumed primary was not eligible for retirement", got)
	}
	request := leasePrefetch{RuleID: rule.ID, LeaseID: rule.Lease.ID}
	if !s.beginPrefetch(request) {
		t.Fatal("current primary could not begin prefetch")
	}
	if got := s.renewals(); len(got) != 0 {
		t.Fatal("pending standby was invalidated by premature retirement", got)
	}
	if err := s.retireRenewal(rule.Lease.ID); err != nil {
		t.Fatal(err)
	}
	if len(s.Retirements()) != 0 {
		t.Fatal("a stale renewal decision retired an in-flight prefetch")
	}
	s.endPrefetch(request)
	if got := s.renewals(); len(got) != 1 || got[0] != rule.Lease.ID {
		t.Fatal("finished prefetch prevented normal retirement", got)
	}
	if err := s.retireRenewal(rule.Lease.ID); err != nil {
		t.Fatal(err)
	}
	if len(s.Retirements()) != 1 {
		t.Fatal("completed prefetch prevented durable retirement")
	}
	if s.beginPrefetch(request) {
		t.Fatal("retired primary accepted a new prefetch")
	}
}

func TestPendingPrefetchDoesNotDeferExpiringAuthorization(t *testing.T) {
	s, b, rule, cfg := tcpCreditBinding(t, 16<<20)
	rule.LeasePipeline = true
	cfg.Version++
	cfg.ValidUntil = time.Now().Add(10 * time.Second)
	cfg.Rules = []contract.Rule{rule}
	if err := b.runtime.Apply(cfg, true); err != nil {
		t.Fatal(err)
	}
	request := leasePrefetch{RuleID: rule.ID, LeaseID: rule.Lease.ID}
	if !s.beginPrefetch(request) {
		t.Fatal("current primary could not begin prefetch")
	}
	defer s.endPrefetch(request)
	if err := s.retireRenewal(rule.Lease.ID); err != nil {
		t.Fatal(err)
	}
	if len(s.Retirements()) != 1 {
		t.Fatal("pending prefetch extended expiring authorization")
	}
}

func TestPrefetchRateSurvivesSlowRoundTripsAndIdleSamples(t *testing.T) {
	s, b, rule, cfg := tcpCreditBinding(t, 256<<20)
	rule.LeasePipeline = true
	cfg.Version++
	cfg.Rules = []contract.Rule{rule}
	if err := b.runtime.Apply(cfg, true); err != nil {
		t.Fatal(err)
	}
	if err := s.Charge(rule, cfg.ValidUntil, false, 1<<20); err != nil {
		t.Fatal(err)
	}
	s.rates = map[string]leaseRateSample{rule.ID: {at: time.Now(), used: map[string]int64{rule.Lease.ID: 1 << 20}, rate: 200 << 20}}
	// Ten empty samples represent waiting one second for control-plane work.
	for range 10 {
		s.mu.Lock()
		previous := s.rates[rule.ID]
		previous.at = time.Now().Add(-100 * time.Millisecond)
		s.rates[rule.ID] = previous
		s.mu.Unlock()
		if requests := s.prefetches(); len(requests) != 1 || requests[0].RawBudget < 128<<20 {
			t.Fatal("poll frequency erased recent demand", requests)
		}
	}
}
