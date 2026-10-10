package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

func tcpCreditBinding(t *testing.T, budget int64) (*Store, *binding, contract.Rule, contract.Config) {
	t.Helper()
	s, runtime, cfg := setup(t)
	rule := testRule("127.0.0.1:1")
	rule.UserID = "tcp-credit-owner"
	rule.Lease.Bytes = budget
	cfg.Rules = []contract.Rule{rule}
	if err := runtime.Apply(cfg, true); err != nil {
		t.Fatal(err)
	}
	return s, runtime.listeners[key(rule)], rule, cfg
}

func reserveTCP(t *testing.T, s *Store, r contract.Rule, until time.Time, up bool) {
	t.Helper()
	if err := s.submit(&stateRequest{event: stateEvent{Kind: "credit_reserve"}, rule: r, until: until, upload: up}); err != nil {
		t.Fatal(err)
	}
}

func TestTCPWindowDebitsDoNotWaitForWriterOrSyncPerChunk(t *testing.T) {
	s, b, rule, cfg := tcpCreditBinding(t, 4<<20)
	s.mu.Lock()
	initial := len(s.state.Windows)
	s.mu.Unlock()
	if initial != 0 {
		t.Fatal("idle TCP rule reserved budget before any transfer")
	}
	reserveTCP(t, s, rule, cfg.ValidUntil, true)
	reserveTCP(t, s, rule, cfg.ValidUntil, true)
	var syncs atomic.Int64
	s.mu.Lock()
	s.fault = func(point string) error {
		if point == "sync_before" {
			syncs.Add(1)
		}
		return nil
	}
	// The writer cannot advance until this mutex is released. Both the change
	// notification snapshot and the debit must remain independent of it.
	done := make(chan error, 1)
	go func() {
		for range 12 {
			if err := b.chargeCurrent(context.Background(), rule.ID, "tcp", true, 32<<10); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	var err error
	select {
	case err = <-done:
	case <-time.After(time.Second):
		err = errors.New("TCP window debit blocked on durable-state mutex")
	}
	beforeUnlock := syncs.Load()
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if beforeUnlock != 0 {
		t.Fatal("memory debit synced the WAL")
	}
	if err := s.FlushCredits(); err != nil {
		t.Fatal(err)
	}
	if got := used(s, rule.Lease.ID); got != 12*(32<<10) {
		t.Fatal("batched TCP usage lost or duplicated", got)
	}
	if syncs.Load() > 3 {
		t.Fatal("TCP still synced per 32 KiB chunk", syncs.Load())
	}
}

func TestTCPWaitsForDurableReservationAndCancellationDoesNotSpend(t *testing.T) {
	s, b, rule, _ := tcpCreditBinding(t, 4<<20)
	entered, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	s.mu.Lock()
	s.fault = func(point string) error {
		if point == "sync_before" {
			once.Do(func() { close(entered); <-resume })
		}
		return nil
	}
	s.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var release sync.Once
	unblock := func() { release.Do(func() { close(resume) }) }
	defer unblock()
	done := make(chan error, 1)
	go func() { done <- b.chargeCurrent(ctx, rule.ID, "tcp", true, 32<<10) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("TCP never requested a durable reservation")
	}
	select {
	case err := <-done:
		t.Fatalf("TCP used unsynced reservation: %v", err)
	default:
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("TCP ignored cancellation during fsync", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled TCP waited for fsync")
	}
	unblock()
	if err := s.FlushCredits(); err != nil {
		t.Fatal(err)
	}
	if got := used(s, rule.Lease.ID); got != 0 {
		t.Fatal("canceled TCP consumed reserved bytes", got)
	}
	if err := b.chargeCurrent(context.Background(), rule.ID, "tcp", true, 32<<10); err != nil {
		t.Fatal("durable reservation did not wake a later TCP debit", err)
	}
	if err := s.FlushCredits(); err != nil {
		t.Fatal(err)
	}
	if got := used(s, rule.Lease.ID); got != 32<<10 {
		t.Fatal("resumed debit changed accounting", got)
	}
}

func TestTCPReservationFailureFailsClosedAndRecoversOnce(t *testing.T) {
	for _, point := range []string{"append_before", "append_after", "sync_before", "sync_after"} {
		t.Run(point, func(t *testing.T) {
			s, b, rule, _ := tcpCreditBinding(t, 4<<20)
			s.mu.Lock()
			s.fault = func(p string) error {
				if p == point {
					return errors.New("TCP reservation storage fault")
				}
				return nil
			}
			s.mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := b.chargeCurrent(ctx, rule.ID, "tcp", true, 32<<10); err == nil || transientMeterError(err) || errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("failed durable reservation authorized TCP or failed to wake it", err)
			}
			if got := used(s, rule.Lease.ID); got != 0 {
				t.Fatal("failed reservation published actual consumption", got)
			}
			next := reopen(t, s)
			want := creditWindowSize
			if point == "append_before" {
				want = 0
			}
			if got := used(next, rule.Lease.ID); got != want {
				t.Fatal("TCP uncertain reservation recovered incorrectly", got, want)
			}
			for _, u := range next.Pending() {
				if u.Kind != "recovery" || u.RuleID != rule.ID || u.UploadBytes != want || u.DownloadBytes != 0 {
					t.Fatal("TCP recovery lost its owner/direction/classification", u)
				}
			}
			count := len(next.Pending())
			next = reopen(t, next)
			if used(next, rule.Lease.ID) != want || len(next.Pending()) != count {
				t.Fatal("TCP recovery charged twice")
			}
		})
	}
}

func TestTCPConcurrentDirectionsNeverOverspend(t *testing.T) {
	s, b, rule, _ := tcpCreditBinding(t, 64*(32<<10))
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := range 64 {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			errs <- b.chargeCurrent(ctx, rule.ID, "tcp", i%2 == 0, 32<<10)
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal("funded concurrent TCP debit failed", err)
		}
	}
	if err := s.FlushCredits(); err != nil {
		t.Fatal(err)
	}
	var up, down int64
	for _, u := range s.Pending() {
		up += u.UploadBytes
		down += u.DownloadBytes
	}
	if up != rule.Lease.Bytes/2 || down != rule.Lease.Bytes/2 || used(s, rule.Lease.ID) != rule.Lease.Bytes {
		t.Fatal("concurrent directional accounting changed", up, down)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := b.chargeCurrent(ctx, rule.ID, "tcp", true, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("exhausted TCP budget was reused", err)
	}
}

func TestTCPSharedAccountResponsibilityAndIdleCreditReclaim(t *testing.T) {
	s, runtime, cfg := setup(t)
	for i := range 17 {
		r := testRule("127.0.0.1:1")
		r.ID, r.UserID = fmt.Sprint("rule-", i), "one-owner"
		l := *r.Lease
		l.ID, l.Bytes = fmt.Sprint("lease-", i), 4<<20
		r.Lease = &l
		cfg.Rules = append(cfg.Rules, r)
	}
	// Each rule needs a distinct listener address for Runtime.Apply.
	for i := range cfg.Rules {
		cfg.Rules[i].Listen = ingressTestAddress(t)
	}
	if err := runtime.Apply(cfg, true); err != nil {
		t.Fatal(err)
	}
	for _, r := range cfg.Rules[:16] {
		reserveTCP(t, s, r, cfg.ValidUntil, true)
		reserveTCP(t, s, r, cfg.ValidUntil, false)
	}
	s.mu.Lock()
	var total int64
	for _, w := range s.state.Windows {
		total += w.Capacity - w.Confirmed
	}
	s.mu.Unlock()
	if total != maxAccountCredits {
		t.Fatal("unexpected shared-account responsibility", total)
	}
	r := cfg.Rules[16]
	if err := s.submit(&stateRequest{event: stateEvent{Kind: "credit_reserve"}, rule: r, until: cfg.ValidUntil, upload: true, n: 32 << 10}); !errors.Is(err, errLeaseUnavailable) {
		t.Fatal("TCP exceeded account reservation bound", err)
	}
	// Move idle memory timestamps into the past without waiting a full second.
	s.creditMu.Lock()
	for _, c := range s.credits {
		c.last = time.Now().Add(-2 * tcpCreditIdleTimeout)
	}
	s.creditMu.Unlock()
	if err := s.FlushCredits(); err != nil {
		t.Fatal(err)
	}
	b := runtime.listeners[key(r)]
	if err := b.chargeCurrent(context.Background(), r.ID, "tcp", true, 32<<10); err != nil {
		t.Fatal("idle rules stranded shared-account credit", err)
	}
	if err := s.FlushCredits(); err != nil {
		t.Fatal(err)
	}
	for _, idle := range cfg.Rules[:16] {
		if used(s, idle.Lease.ID) != 0 {
			t.Fatal("normal idle reclaim billed unused reservation")
		}
	}
	if used(s, r.Lease.ID) != 32<<10 {
		t.Fatal("reclaimed budget not settled exactly")
	}
}

func TestTCPSmallLeaseRebalancesDirectionsAndNormalCloseSettlesExactly(t *testing.T) {
	s, b, rule, _ := tcpCreditBinding(t, 10)
	for _, debit := range []struct {
		up bool
		n  int
	}{{true, 6}, {false, 3}, {true, 1}} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := b.chargeCurrent(ctx, rule.ID, "tcp", debit.up, debit.n)
		cancel()
		if err != nil {
			t.Fatal("small bidirectional TCP budget was stranded", err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	next := reopen(t, s)
	var up, down int64
	for _, u := range next.Pending() {
		if u.Kind != "normal" {
			t.Fatal("clean shutdown conservatively billed unused bytes")
		}
		up += u.UploadBytes
		down += u.DownloadBytes
	}
	if up != 7 || down != 3 || used(next, rule.Lease.ID) != 10 {
		t.Fatal("small TCP budget settled incorrectly", up, down)
	}
}

func TestTCPPolicyChangeClosesOldWindowsWithoutBillingUnusedBytes(t *testing.T) {
	s, b, rule, cfg := tcpCreditBinding(t, 4<<20)
	if err := b.chargeCurrent(context.Background(), rule.ID, "tcp", true, 32<<10); err != nil {
		t.Fatal(err)
	}
	// Same lease ID, different destination: no old memory authorization survives.
	rule.Target = "127.0.0.1:2"
	cfg.Version++
	cfg.Rules = []contract.Rule{rule}
	if err := b.runtime.Apply(cfg, true); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	windows := len(s.state.Windows)
	s.mu.Unlock()
	if windows != 0 || used(s, rule.Lease.ID) != 32<<10 {
		t.Fatal("changed TCP policy retained old windows or billed unused credit")
	}
}
