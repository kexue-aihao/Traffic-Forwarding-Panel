package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

func TestTCPPrimaryTailUsesFundedStandbyAndRequestsPromotion(t *testing.T) {
	for _, budget := range []int64{100000, 16 << 20} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			s, b, rule, cfg := tcpCreditBinding(t, budget)
			standby := *rule.Lease
			standby.ID, standby.Bytes = "funded-standby", 64<<20
			rule.LeasePipeline, rule.StandbyLease = true, &standby
			cfg.Rules, cfg.Version = []contract.Rule{rule}, cfg.Version+1
			if err := b.runtime.Apply(cfg, true); err != nil {
				t.Fatal(err)
			}
			// Leave one byte in the primary. A full relay block must come from
			// the existing standby without waiting for a new configuration.
			if err := s.Charge(rule, cfg.ValidUntil, true, int(budget-1)); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := b.chargeCurrent(ctx, rule.ID, "tcp", false, 32<<10); err != nil {
				t.Fatal("unused primary tail stranded funded standby", err)
			}
			if err := s.FlushCredits(); err != nil {
				t.Fatal(err)
			}
			if used(s, rule.Lease.ID) != budget-1 || used(s, standby.ID) != 32<<10 {
				t.Fatal("tail handoff changed lease accounting")
			}
			ids := s.renewals()
			if len(ids) != 1 || ids[0] != rule.Lease.ID {
				t.Fatal("standby consumption did not promote the lease pipeline", ids)
			}
		})
	}
}

func TestTCPAdaptiveWindowsGrowAndSettleExactly(t *testing.T) {
	s, b, rule, _ := tcpCreditBinding(t, 256<<20)
	var syncs atomic.Int64
	s.mu.Lock()
	s.fault = func(point string) error {
		if point == "sync_before" {
			syncs.Add(1)
		}
		return nil
	}
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for range 1024 {
		if err := b.chargeCurrent(ctx, rule.ID, "tcp", false, 32<<10); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.FlushCredits(); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.fault = nil
	var largest, liability int64
	for _, w := range s.state.Windows {
		largest = max(largest, w.Capacity)
		liability += w.Capacity - w.Confirmed
	}
	err := s.validateSnapshot()
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if largest != maxTCPCreditWindow || liability > maxTCPRuleCredits || liability > maxAccountCredits {
		t.Fatal("active TCP windows did not grow within responsibility bounds", largest, liability)
	}
	if used(s, rule.Lease.ID) != 32<<20 {
		t.Fatal("adaptive debit lost or duplicated bytes")
	}
	if syncs.Load() > 64 {
		t.Fatal("sustained TCP still syncs each 256 KiB", syncs.Load())
	}
}

func TestTCPUpgradeCandidateKeepsLegacyWindowBounds(t *testing.T) {
	s, b, rule, _ := tcpCreditBinding(t, 256<<20)
	s.pauseTCPGrowth.Store(true)
	for range 128 {
		if err := b.chargeCurrent(context.Background(), rule.ID, "tcp", false, 32<<10); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.FlushCredits(); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	var liability int64
	for _, w := range s.state.Windows {
		if w.Capacity > creditWindowSize {
			s.mu.Unlock()
			t.Fatal("uncommitted upgrade created credit unreadable by rollback binary")
		}
		liability += w.Capacity - w.Confirmed
	}
	expanded := s.expandedCredits
	s.mu.Unlock()
	if liability > maxRuleCredits || expanded {
		t.Fatal("upgrade candidate exceeded legacy ledger bounds")
	}
	// The supervisor's durable success enables adaptive windows afterward.
	s.pauseTCPGrowth.Store(false)
	for range 256 {
		if err := b.chargeCurrent(context.Background(), rule.ID, "tcp", false, 32<<10); err != nil {
			t.Fatal(err)
		}
	}
	s.mu.Lock()
	expanded = s.expandedCredits
	s.mu.Unlock()
	if !expanded {
		t.Fatal("committed upgrade did not enable adaptive windows")
	}
}

func TestTCPExpandedRefillReleasesLiabilityInOneSync(t *testing.T) {
	s, _, rule, cfg := tcpCreditBinding(t, 64<<20)
	for range 2 {
		if err := s.submit(&stateRequest{event: stateEvent{Kind: "credit_reserve"}, rule: rule, until: cfg.ValidUntil, creditSize: maxTCPCreditWindow}); err != nil {
			t.Fatal(err)
		}
	}
	entered, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(resume) }) }
	defer unblock()
	var syncs atomic.Int64
	s.mu.Lock()
	s.fault = func(point string) error {
		if point == "sync_before" && syncs.Add(1) == 1 {
			close(entered)
			<-resume
		}
		return nil
	}
	for range 9 {
		if err := s.TryCreditCharge(rule, cfg.ValidUntil, false, 256<<10); err != nil {
			s.mu.Unlock()
			t.Fatal(err)
		}
	}
	s.mu.Unlock()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("expanded refill did not reach sync")
	}
	for range 7 {
		if err := s.TryCreditCharge(rule, cfg.ValidUntil, false, 256<<10); err != nil {
			t.Fatal("old persisted credit blocked during sync", err)
		}
	}
	if err := s.TryCreditCharge(rule, cfg.ValidUntil, false, 1); !errors.Is(err, errCreditUnavailable) {
		t.Fatal("unsynced expanded reservation leaked", err)
	}
	unblock()
	s.mu.Lock()
	s.fault = nil
	count := syncs.Load()
	err := s.validateSnapshot()
	s.mu.Unlock()
	if count != 1 || err != nil {
		t.Fatal("replacement needed an extra liability-settlement sync", count, err)
	}
	if err := s.FlushCredits(); err != nil {
		t.Fatal(err)
	}
	if used(s, rule.Lease.ID) != 4<<20 {
		t.Fatal("in-flight expanded consumption changed accounting")
	}
}

func TestTCPExpandedWindowRecoveryAndCleanRollback(t *testing.T) {
	for _, point := range []string{"", "append_before", "append_after", "sync_before", "sync_after"} {
		t.Run(point, func(t *testing.T) {
			s, _, rule, cfg := tcpCreditBinding(t, 64<<20)
			s.mu.Lock()
			s.fault = func(p string) error {
				if point != "" && p == point {
					return errors.New("expanded reservation fault")
				}
				return nil
			}
			s.mu.Unlock()
			err := s.submit(&stateRequest{event: stateEvent{Kind: "credit_reserve"}, rule: rule, until: cfg.ValidUntil, creditSize: maxTCPCreditWindow})
			want := maxTCPCreditWindow
			if point == "append_before" {
				want = 0
			}
			if point == "" {
				if err != nil {
					t.Fatal(err)
				}
				if err := s.TryCreditCharge(rule, cfg.ValidUntil, false, 12345); err != nil {
					t.Fatal(err)
				}
				want = 12345
			} else if err == nil {
				t.Fatal("fault ignored")
			}
			next := reopen(t, s)
			if used(next, rule.Lease.ID) != want {
				t.Fatal("expanded recovery changed charged responsibility", used(next, rule.Lease.ID), want)
			}
			count := len(next.Pending())
			if err := next.Close(); err != nil {
				t.Fatal(err)
			}
			if point != "append_before" {
				stat, err := os.Stat(next.path + ".wal")
				if err != nil || stat.Size() != walHeaderSize {
					t.Fatal("clean shutdown retained expanded history unreadable by an older Agent", err)
				}
			}
			next = reopen(t, next)
			if used(next, rule.Lease.ID) != want || len(next.Pending()) != count {
				t.Fatal("expanded recovery was not idempotent")
			}
		})
	}
}
