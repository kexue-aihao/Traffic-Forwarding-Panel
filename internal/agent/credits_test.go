package agent

import (
	"errors"
	"fmt"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"strings"
	"sync"
	"testing"
	"time"
)

func creditRule(t *testing.T) (*Store, contract.Rule, time.Time) {
	t.Helper()
	s, _, c := setup(t)
	r := testRule("127.0.0.1:1")
	r.Network = "udp"
	r.UserID = "credit-user"
	r.UDP = &contract.UDPOptions{CreditWindows: true, MaxSessions: 1024}
	r.Lease.Bytes = 4 << 20
	c.Rules = []contract.Rule{r}
	if e := s.SetConfig(c); e != nil {
		t.Fatal(e)
	}
	if e := s.PrimeUDPCredits(r, c.ValidUntil); e != nil {
		t.Fatal(e)
	}
	return s, r, c.ValidUntil
}

func TestUDPCreditsDoNotWaitForDiskAndSettleExactly(t *testing.T) {
	s, r, until := creditRule(t)
	// Holding the durable-state mutex simulates a writer stuck in fsync.
	s.mu.Lock()
	done := make(chan error, 1)
	go func() { done <- s.TryUDPCharge(r, until, true, 1200) }()
	select {
	case e := <-done:
		if e != nil {
			t.Error(e)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("UDP debit waited for disk mutex")
	}
	s.mu.Unlock()
	if e := s.TryUDPCharge(r, until, false, 700); e != nil {
		t.Fatal(e)
	}
	if e := s.FlushUDPCredits(); e != nil {
		t.Fatal(e)
	}
	if used(s, r.Lease.ID) != 1900 {
		t.Fatal("normal byte accounting changed", used(s, r.Lease.ID))
	}
	for _, u := range s.Pending() {
		if u.Kind != "normal" || u.WindowID == "" || u.WindowSequence == 0 {
			t.Fatal("window audit metadata missing")
		}
	}
	if e := s.Retire(r.Lease.ID); e != nil {
		t.Fatal(e)
	}
	if e := s.TryUDPCharge(r, until, true, 1); e == nil {
		t.Fatal("retired window was reused")
	}
	if used(s, r.Lease.ID) != 1900 {
		t.Fatal("unused reservation billed during normal retirement")
	}
}

func TestUDPCreditCrashRecoveryConservativeAndIdempotent(t *testing.T) {
	s, r, until := creditRule(t)
	if e := s.TryUDPCharge(r, until, true, 100); e != nil {
		t.Fatal(e)
	}
	if e := s.FlushUDPCredits(); e != nil {
		t.Fatal(e)
	}
	s.mu.Lock()
	responsibility := s.creditLiabilityLocked(r.Lease.ID)
	s.fault = func(point string) error {
		if point == "append_before" {
			return errors.New("crash")
		}
		return nil
	}
	s.mu.Unlock()
	if e := s.TryUDPCharge(r, until, false, 50); e != nil {
		t.Fatal(e)
	}
	if e := s.FlushUDPCredits(); e == nil {
		t.Fatal("fault was ignored")
	}
	next := reopen(t, s)
	if used(next, r.Lease.ID) != 100+responsibility || responsibility > maxRuleCredits {
		t.Fatal("recovered responsibility incorrect")
	}
	var recovered int64
	var ids []string
	for _, u := range next.Pending() {
		if u.Kind == "recovery" {
			recovered += u.UploadBytes + u.DownloadBytes
		}
		ids = append(ids, u.ID)
		if u.EndedAt.After(r.Lease.ExpiresAt) {
			t.Fatal("recovery shifted lease period")
		}
	}
	if recovered != responsibility {
		t.Fatal("recovery facts not separately auditable")
	}
	next = reopen(t, next)
	if used(next, r.Lease.ID) != 100+responsibility || len(next.Pending()) != len(ids) {
		t.Fatal("recovery billed twice")
	}
}

func TestUDPCreditsConcurrentQuotaAndTCPShareLedger(t *testing.T) {
	s, r, until := creditRule(t)
	if e := s.Charge(r, until, true, int(r.Lease.Bytes)); e == nil {
		t.Fatal("TCP path ignored UDP reservations")
	}
	// Retirement must close all windows before the retire marker.
	if used(s, r.Lease.ID) != 0 {
		t.Fatal("rejected TCP debit charged")
	}
	s2, r2, until2 := creditRule(t)
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			for range 100 {
				_ = s2.TryUDPCharge(r2, until2, true, 64)
			}
		})
	}
	wg.Wait()
	if e := s2.FlushUDPCredits(); e != nil {
		t.Fatal(e)
	}
	if used(s2, r2.Lease.ID) != 32*100*64 {
		t.Fatal("concurrent UDP debit lost or duplicated")
	}
}

func TestUDPCreditReserveFailureNeverAuthorizes(t *testing.T) {
	for _, point := range []string{"append_before", "append_after", "sync_before", "sync_after"} {
		t.Run(point, func(t *testing.T) {
			s, _, c := setup(t)
			r := testRule("127.0.0.1:1")
			r.Network = "udp"
			r.UDP = &contract.UDPOptions{CreditWindows: true}
			r.Lease.Bytes = 4 << 20
			s.mu.Lock()
			s.fault = func(p string) error {
				if p == point {
					return errors.New("reserve fault")
				}
				return nil
			}
			s.mu.Unlock()
			if e := s.PrimeUDPCredits(r, c.ValidUntil); e == nil {
				t.Fatal("reserve fault ignored")
			}
			if e := s.TryUDPCharge(r, c.ValidUntil, true, 1); e == nil {
				t.Fatal("unconfirmed reservation authorized")
			}
			next := reopen(t, s)
			maximum := int64(creditWindowSize)
			if point == "append_before" {
				maximum = 0
			}
			if used(next, r.Lease.ID) != maximum {
				t.Fatal("uncertain reserve recovered incorrectly", used(next, r.Lease.ID))
			}
		})
	}
}

func TestUDPCreditCloseAndCheckpointCrashBoundaries(t *testing.T) {
	for _, point := range []string{"append_before", "append_after", "sync_before", "sync_after", "snapshot_before_write", "snapshot_after_sync", "snapshot_after_rename", "rotate_before_rename", "rotate_after_rename"} {
		t.Run(point, func(t *testing.T) {
			s, r, until := creditRule(t)
			if e := s.TryUDPCharge(r, until, true, 1200); e != nil {
				t.Fatal(e)
			}
			if e := s.TryUDPCharge(r, until, false, 600); e != nil {
				t.Fatal(e)
			}
			s.mu.Lock()
			s.fault = func(p string) error {
				if p == point {
					return errors.New("credit close/checkpoint crash")
				}
				return nil
			}
			s.mu.Unlock()
			var err error
			checkpoint := strings.HasPrefix(point, "snapshot_") || strings.HasPrefix(point, "rotate_")
			if checkpoint {
				err = s.Checkpoint()
			} else {
				err = s.Retire(r.Lease.ID)
			}
			if err == nil {
				t.Fatal("crash fault ignored")
			}
			next := reopen(t, s)
			want := int64(1800)
			if checkpoint || point == "append_before" {
				want = maxRuleCredits
			}
			if used(next, r.Lease.ID) != want {
				t.Fatal("close/recovery released uncertain quota or lost known usage", used(next, r.Lease.ID), want)
			}
			ids := map[string]bool{}
			for _, u := range next.Pending() {
				if ids[u.ID] {
					t.Fatal("duplicate recovery fact")
				}
				ids[u.ID] = true
			}
			next = reopen(t, next)
			if used(next, r.Lease.ID) != want || len(next.Pending()) != len(ids) {
				t.Fatal("repeated recovery charged twice")
			}
		})
	}
}

func TestCreditBatchSyncIsBoundedAndAmortized(t *testing.T) {
	s, _, cfg := setup(t)
	r := testRule("127.0.0.1:1")
	r.Network = "udp"
	r.UDP = &contract.UDPOptions{CreditWindows: true}
	r.Lease.Bytes = 4 << 20
	cfg.Rules = []contract.Rule{r}
	if e := s.SetConfig(cfg); e != nil {
		t.Fatal(e)
	}
	syncs := 0
	s.mu.Lock()
	s.fault = func(point string) error {
		if point == "sync_before" {
			syncs++
		}
		return nil
	}
	s.mu.Unlock()
	batch := []*stateRequest{}
	for i := range 8 {
		batch = append(batch, &stateRequest{event: stateEvent{Kind: "credit_reserve"}, rule: r, until: cfg.ValidUntil, upload: i%2 == 0, done: make(chan error, 1)})
	}
	s.process(batch)
	for _, r := range batch {
		if e := <-r.done; e != nil {
			t.Fatal(e)
		}
	}
	s.mu.Lock()
	s.fault = nil
	n := len(s.state.Windows)
	s.mu.Unlock()
	if syncs != 1 || n != 4 {
		t.Fatal("reservation batch synchronized per request or exceeded directional bounds", syncs, n)
	}
}

func TestSmallCreditBudgetsPermitDirectionalAndLargePackets(t *testing.T) {
	for _, budget := range []int64{10, 100000} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			s, _, cfg := setup(t)
			r := testRule("127.0.0.1:1")
			r.Network = "udp"
			r.UDP = &contract.UDPOptions{CreditWindows: true}
			r.Lease.Bytes = budget
			cfg.Rules = []contract.Rule{r}
			if e := s.SetConfig(cfg); e != nil {
				t.Fatal(e)
			}
			if e := s.PrimeUDPCredits(r, cfg.ValidUntil); e != nil {
				t.Fatal(e)
			}
			if budget == 10 {
				for _, up := range []bool{true, false} {
					if e := s.TryUDPCharge(r, cfg.ValidUntil, up, 5); e != nil {
						t.Fatal("small exact bidirectional budget stranded", e)
					}
				}
				if e := s.FlushUDPCredits(); e != nil {
					t.Fatal(e)
				}
				if used(s, r.Lease.ID) != 10 {
					t.Fatal("small budget not settled exactly")
				}
				return
			}
			if e := s.TryUDPCharge(r, cfg.ValidUntil, true, 60000); !errors.Is(e, errCreditUnavailable) {
				t.Fatal("unreserved oversized packet admitted", e)
			}
			// FIFO flush acts as a barrier after the asynchronous rebalance.
			if e := s.FlushUDPCredits(); e != nil {
				t.Fatal(e)
			}
			if e := s.TryUDPCharge(r, cfg.ValidUntil, true, 60000); e != nil {
				t.Fatal("unused opposite-direction credit stranded large packet", e)
			}
			if e := s.FlushUDPCredits(); e != nil {
				t.Fatal(e)
			}
			_ = s.TryUDPCharge(r, cfg.ValidUntil, false, 32)
			if e := s.FlushUDPCredits(); e != nil {
				t.Fatal(e)
			}
			if e := s.TryUDPCharge(r, cfg.ValidUntil, false, 32); e != nil {
				t.Fatal("reply could not borrow unused directional credit", e)
			}
		})
	}
}

func TestUDPRefillPreservesOppositeDirectionWhenBudgetIsAvailable(t *testing.T) {
	s, _, cfg := setup(t)
	r := testRule("127.0.0.1:1")
	r.Network = "udp"
	r.UserID = "credit-user"
	r.UDP = &contract.UDPOptions{CreditWindows: true}
	r.Lease.Bytes = 4 << 20
	cfg.Rules = []contract.Rule{r}
	if e := s.SetConfig(cfg); e != nil {
		t.Fatal(e)
	}
	if e := s.submit(&stateRequest{event: stateEvent{Kind: "credit_reserve"}, rule: r, until: cfg.ValidUntil, upload: true}); e != nil {
		t.Fatal(e)
	}
	if e := s.TryUDPCharge(r, cfg.ValidUntil, true, 1200); e != nil {
		t.Fatal(e)
	}
	if e := s.FlushUDPCredits(); e != nil {
		t.Fatal(e)
	}
	if e := s.TryUDPCharge(r, cfg.ValidUntil, false, 1200); !errors.Is(e, errCreditUnavailable) {
		t.Fatal("unreserved download admitted", e)
	}
	if e := s.FlushUDPCredits(); e != nil {
		t.Fatal(e)
	}
	for _, up := range []bool{true, false} {
		if e := s.TryUDPCharge(r, cfg.ValidUntil, up, 1200); e != nil {
			t.Fatal("directional refill revoked usable opposite-direction credit", up, e)
		}
	}
	if e := s.FlushUDPCredits(); e != nil {
		t.Fatal(e)
	}
	if got := used(s, r.Lease.ID); got != 3600 {
		t.Fatal("refill accounting changed", got)
	}
}
