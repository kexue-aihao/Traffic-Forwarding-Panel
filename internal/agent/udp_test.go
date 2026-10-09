package agent

import (
	"bytes"
	"context"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"net"
	"sync"
	"testing"
	"time"
)

func TestOptimizedDirectUDPBoundariesRenewalAndRevocation(t *testing.T) {
	s, r, c := setup(t)
	_, target := limitsEcho(t)
	rule := testRule(target)
	rule.Network = "udp"
	rule.UDP = &contract.UDPOptions{CreditWindows: true, MaxSessions: 1024}
	rule.Lease.Bytes = 4 << 20
	c.Rules = []contract.Rule{rule}
	if e := r.Apply(c, true); e != nil {
		t.Fatal(e)
	}
	b := r.listeners[key(rule)]
	conn, e := net.Dial("udp", b.udp.LocalAddr().String())
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	var amount int64
	for _, size := range []int{0, 1, 64, 1200, 8192, 60000} {
		p := bytes.Repeat([]byte{byte(size)}, size)
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		if _, e = conn.Write(p); e != nil {
			t.Fatal(e)
		}
		buf := make([]byte, 65535)
		n, e := conn.Read(buf)
		if e != nil || !bytes.Equal(p, buf[:n]) {
			t.Fatalf("size=%d n=%d error=%v", size, n, e)
		}
		amount += int64(size) * 2
	}
	if e := s.FlushUDPCredits(); e != nil {
		t.Fatal(e)
	}
	if used(s, rule.Lease.ID) != amount {
		t.Fatal("optimized byte accounting changed", amount, used(s, rule.Lease.ID))
	}
	b.mu.Lock()
	original := b.sessions[conn.LocalAddr().String()]
	b.mu.Unlock()
	lease := *rule.Lease
	lease.ID = "next-udp-lease"
	rule.Lease = &lease
	c.Version++
	c.Rules = []contract.Rule{rule}
	if e := r.Apply(c, true); e != nil {
		t.Fatal(e)
	}
	if e := exchange(conn, "renewed"); e != nil {
		t.Fatal(e)
	}
	b.mu.Lock()
	same := b.sessions[conn.LocalAddr().String()] == original
	b.mu.Unlock()
	if !same {
		t.Fatal("credit renewal replaced UDP session")
	}
	c.Version++
	c.Rules = nil
	if e := r.Apply(c, true); e != nil {
		t.Fatal(e)
	}
	if original.ctx.Err() == nil {
		t.Fatal("revocation left UDP session active")
	}
	if e := s.TryUDPCharge(rule, c.ValidUntil, true, 1); e == nil {
		t.Fatal("revocation did not freeze credits")
	}
}

func TestOptimizedUDPSlowDialDoesNotBlockOtherPeer(t *testing.T) {
	s, r, c := setup(t)
	_, target := limitsEcho(t)
	rule := testRule(target)
	rule.Network = "udp"
	rule.UDP = &contract.UDPOptions{CreditWindows: true, MaxSessions: 1024}
	rule.Lease.Bytes = 4 << 20
	c.Rules = []contract.Rule{rule}
	if e := r.Apply(c, true); e != nil {
		t.Fatal(e)
	}
	b := r.listeners[key(rule)]
	// Occupy the independent dial pool. Reception remains live and rejects new
	// sessions with an explicit capacity counter rather than blocking a worker.
	for i := 0; i < cap(r.udpDialSlots); i++ {
		r.udpDialSlots <- struct{}{}
	}
	conn, e := net.Dial("udp", b.udp.LocalAddr().String())
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	conn.Write([]byte("full"))
	deadline := time.Now().Add(time.Second)
	for b.udpStats.capacityDrops.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if b.udpStats.capacityDrops.Load() == 0 {
		t.Fatal("receive loop blocked on dial capacity")
	}
	for i := 0; i < cap(r.udpDialSlots); i++ {
		<-r.udpDialSlots
	}
	if e := exchange(conn, "available"); e != nil {
		t.Fatal(e)
	}
	_ = s
}

func TestUDPStandbyCreditSwitchAndSharedAccountBound(t *testing.T) {
	s, r, until := creditRule(t)
	standby := *r.Lease
	standby.ID = "standby"
	standby.Bytes = 4 << 20
	standby.ExpiresAt = time.Now().Add(4 * time.Minute)
	r.LeasePipeline = true
	r.StandbyLease = &standby
	c := s.Config()
	c.Rules = []contract.Rule{r}
	if e := s.SetConfig(c); e != nil {
		t.Fatal(e)
	}
	if e := s.Retire(r.Lease.ID); e != nil {
		t.Fatal(e)
	}
	if e := s.PrimeUDPCredits(r, until); e != nil {
		t.Fatal(e)
	}
	if e := s.TryUDPCharge(r, until, true, 100); e != nil {
		t.Fatal("standby did not authorize UDP", e)
	}
	if e := s.FlushUDPCredits(); e != nil {
		t.Fatal(e)
	}
	if used(s, standby.ID) != 100 {
		t.Fatal("standby usage bound to wrong lease")
	}
	s2, _, cfg := setup(t)
	rules := []contract.Rule{}
	for i := 0; i < 16; i++ {
		rule := testRule("127.0.0.1:1")
		rule.ID = string(rune('a' + i))
		rule.UserID = "shared"
		rule.Network = "udp"
		rule.UDP = &contract.UDPOptions{CreditWindows: true}
		l := *rule.Lease
		l.ID = rule.ID
		l.Bytes = 16 << 20
		rule.Lease = &l
		rules = append(rules, rule)
	}
	cfg.Rules = rules
	if e := s2.SetConfig(cfg); e != nil {
		t.Fatal(e)
	}
	for _, rule := range rules {
		if e := s2.PrimeUDPCredits(rule, cfg.ValidUntil); e != nil {
			t.Fatal(e)
		}
	}
	s2.mu.Lock()
	var total int64
	for _, w := range s2.state.Windows {
		total += w.Capacity - w.Confirmed
	}
	s2.mu.Unlock()
	if total > maxAccountCredits {
		t.Fatal("aggregate account crash responsibility exceeded", total)
	}
}

func TestUDPCreditCloseConcurrentConsumers(t *testing.T) {
	s, r, until := creditRule(t)
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for range 8 {
		wg.Go(func() {
			for ctx.Err() == nil {
				_ = s.TryUDPCharge(r, until, true, 1)
			}
		})
	}
	if e := s.Retire(r.Lease.ID); e != nil {
		t.Fatal(e)
	}
	cancel()
	wg.Wait()
	before := used(s, r.Lease.ID)
	if e := s.TryUDPCharge(r, until, true, 1); e == nil {
		t.Fatal("close refunded credits still used by a worker")
	}
	if used(s, r.Lease.ID) != before {
		t.Fatal("post-retirement counter changed")
	}
}
