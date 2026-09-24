package agent

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/probe"
)

func exchange(c net.Conn, text string) error {
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(c, text); err != nil {
		return err
	}
	b := make([]byte, len(text))
	if _, err := io.ReadFull(c, b); err != nil {
		return err
	}
	if string(b) != text {
		return errors.New("echo payload changed")
	}
	return nil
}

func TestLiveConnectionsFollowRenewedLease(t *testing.T) {
	for _, network := range []string{"tcp", "udp"} {
		t.Run(network, func(t *testing.T) {
			s, runtime, cfg := setup(t)
			tcp, udp := limitsEcho(t)
			rule := testRule(tcp)
			rule.Network = network
			if network == "udp" {
				rule.Target = udp
			}
			cfg.Rules = []contract.Rule{rule}
			if err := runtime.Apply(cfg, true); err != nil {
				t.Fatal(err)
			}
			b := runtime.listeners[key(rule)]
			var addr string
			if network == "tcp" {
				addr = b.tcp.Addr().String()
			} else {
				addr = b.udp.LocalAddr().String()
			}
			conn, err := net.Dial(network, addr)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err := exchange(conn, "before"); err != nil {
				t.Fatal(err)
			}
			b.mu.Lock()
			originalSession := b.sessions[conn.LocalAddr().String()]
			b.mu.Unlock()
			if err := s.Retire(rule.Lease.ID); err != nil {
				t.Fatal(err)
			}
			var resumed chan error
			if network == "tcp" {
				resumed = make(chan error, 1)
				go func() { resumed <- exchange(conn, "after") }()
				select {
				case <-s.usageWake:
				case <-time.After(time.Second):
					t.Fatal("exhausted flow did not request synchronization")
				}
				select {
				case err := <-resumed:
					t.Fatalf("connection ended before replacement: %v", err)
				default:
				}
			}
			renewed := *rule.Lease
			renewed.ID = "replacement"
			renewed.EntitlementID = "new-cycle"
			renewed.ExpiresAt = time.Now().Add(10 * time.Minute)
			rule.Lease = &renewed
			cfg.Rules = []contract.Rule{rule}
			cfg.Version++
			if err := runtime.Apply(cfg, true); err != nil {
				t.Fatal(err)
			}
			if network == "tcp" {
				if err := <-resumed; err != nil {
					t.Fatal("renewal disconnected TCP", err)
				}
			} else {
				if err := exchange(conn, "after"); err != nil {
					t.Fatal(err)
				}
				b.mu.Lock()
				same := originalSession != nil && originalSession == b.sessions[conn.LocalAddr().String()]
				b.mu.Unlock()
				if !same {
					t.Fatal("renewal replaced UDP session")
				}
			}
			amounts := map[string]int64{}
			for _, u := range s.Pending() {
				amounts[u.LeaseID] += u.UploadBytes + u.DownloadBytes
				if u.LeaseID == renewed.ID && u.EntitlementID != "new-cycle" {
					t.Fatal("renewed flow charged the old cycle")
				}
			}
			if amounts["lease"] != 12 || amounts["replacement"] != 10 {
				t.Fatalf("incorrect lease accounting: %v", amounts)
			}
			cfg.Version++
			cfg.Rules = nil
			if err := runtime.Apply(cfg, true); err != nil {
				t.Fatal(err)
			}
			if network == "tcp" && exchange(conn, "revoked") == nil {
				t.Fatal("revocation left connection usable")
			}
		})
	}
}

func TestMeterBackpressureResumesAndCancellationStopsSpending(t *testing.T) {
	s, runtime, cfg := setup(t)
	rule := testRule("127.0.0.1:1")
	cfg.Rules = []contract.Rule{rule}
	if err := runtime.Apply(cfg, true); err != nil {
		t.Fatal(err)
	}
	b := runtime.listeners[key(rule)]
	// Simulate a full durable spool; confirmations free the same bounded space.
	s.mu.Lock()
	for i := range MaxPendingRecords {
		s.state.Pending = append(s.state.Pending, contract.UsageRecord{ID: fmt.Sprint(i)})
	}
	s.mu.Unlock()
	if ids := s.renewals(); len(ids) != 0 {
		t.Fatal("spool pressure retired a funded lease", ids)
	}
	done := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { done <- b.chargeCurrent(ctx, rule.ID, "tcp", true, 7) }()
	select {
	case <-s.usageWake:
	case <-ctx.Done():
		t.Fatal("full spool did not request upload")
	}
	select {
	case err := <-done:
		t.Fatalf("full spool closed flow instead of waiting: %v", err)
	default:
	}
	if err := s.Confirm([]string{"0"}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := used(s, rule.Lease.ID); got != 7 {
		t.Fatal("resumed bytes charged more than once", got)
	}
	canceled, stop := context.WithCancel(context.Background())
	stop()
	if err := b.chargeCurrent(canceled, rule.ID, "tcp", true, 3); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled flow was not rejected", err)
	}
	if got := used(s, rule.Lease.ID); got != 7 {
		t.Fatal("canceled flow spent bytes", got)
	}
}

func TestBlockedUsageDoesNotBlockConfigRevocationOrProbes(t *testing.T) {
	s, runtime, cfg := setup(t)
	target, _ := limitsEcho(t)
	rule := testRule(target)
	cfg.Rules = []contract.Rule{rule}
	if err := runtime.Apply(cfg, true); err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("tcp", runtime.listeners[key(rule)].tcp.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := exchange(c, "pending"); err != nil {
		t.Fatal(err)
	}
	blocked := make(chan struct{})
	probeSeen := make(chan struct{}, 1)
	var blockOnce sync.Once
	var revoke atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent/config":
			out := cfg
			if revoke.Load() {
				out.Version++
				out.Rules = nil
			}
			json.NewEncoder(w).Encode(out)
		case "/api/v1/agent/usage":
			io.Copy(io.Discard, r.Body)
			blockOnce.Do(func() { close(blocked) })
			<-r.Context().Done()
		case "/api/v1/agent/probe":
			if revoke.Load() {
				wake(probeSeen)
			}
			w.WriteHeader(204)
		case "/api/v1/agent/ack":
			w.WriteHeader(204)
		default:
			io.WriteString(w, "{}")
		}
	}))
	defer srv.Close()
	a := &Agent{URL: srv.URL, HTTP: srv.Client(), Store: s, Runtime: runtime, Probe: &probe.Collector{}, PollInterval: 20 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	defer func() { cancel(); <-done }()
	select {
	case <-blocked:
	case <-ctx.Done():
		t.Fatal("usage request did not start")
	}
	revoke.Store(true)
	select {
	case <-probeSeen:
	case <-ctx.Done():
		t.Fatal("blocked usage prevented probes")
	}
	for runtime.Version() < cfg.Version+1 && ctx.Err() == nil {
		time.Sleep(time.Millisecond)
	}
	if runtime.Version() < cfg.Version+1 {
		t.Fatal("blocked usage prevented revocation")
	}
	if exchange(c, "revoked") == nil {
		t.Fatal("revoked connection still forwards")
	}
	if len(s.Pending()) == 0 {
		t.Fatal("unacknowledged bytes were lost")
	}
}

func TestSharedTLSChildSurvivesRefreshAndStopsOnLimitChange(t *testing.T) {
	s, runtime, cfg := setup(t)
	pair, roots := chainCertificate(t)
	target, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{pair}})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	go func() {
		conn, err := target.Accept()
		if err == nil {
			defer conn.Close()
			io.Copy(conn, conn)
		}
	}()
	parent := testRule(target.Addr().String())
	parent.UserID = "alice"
	parent.SharedTLS = &contract.SharedTLS{ServerName: "parent.local"}
	child := parent
	child.ID = "child"
	child.SharedTLS = &contract.SharedTLS{ParentID: parent.ID, ServerName: "localhost"}
	childLease := *parent.Lease
	childLease.ID = "child-lease"
	child.Lease = &childLease
	cfg.Rules = []contract.Rule{parent, child}
	if err := runtime.Apply(cfg, true); err != nil {
		t.Fatal(err)
	}
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", runtime.listeners[key(parent)].tcp.Addr().String(), &tls.Config{RootCAs: roots, ServerName: "localhost"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := exchange(conn, "before"); err != nil {
		t.Fatal(err)
	}
	cfg.ValidUntil = cfg.ValidUntil.Add(time.Minute)
	if err := runtime.Apply(cfg, true); err != nil {
		t.Fatal(err)
	}
	if err := exchange(conn, "same-lease"); err != nil {
		t.Fatal("config validity refresh disconnected shared TLS", err)
	}
	renewed := childLease
	renewed.ID = "child-renewed"
	renewed.ExpiresAt = renewed.ExpiresAt.Add(time.Minute)
	child.Lease = &renewed
	cfg.Version++
	cfg.Rules = []contract.Rule{parent, child}
	if err := runtime.Apply(cfg, true); err != nil {
		t.Fatal(err)
	}
	if err := exchange(conn, "after"); err != nil {
		t.Fatal("lease refresh disconnected shared TLS", err)
	}
	if used(s, parent.Lease.ID) != 0 || used(s, renewed.ID) == 0 {
		t.Fatal("child used its parent's allocation or failed to switch leases")
	}
	parentLease := *parent.Lease
	parentLease.Limits.MaxConnectionsPerNode = 1
	parent.Lease = &parentLease
	limitedChild := renewed
	limitedChild.Limits = parentLease.Limits
	child.Lease = &limitedChild
	cfg.Version++
	cfg.Rules = []contract.Rule{parent, child}
	if err := runtime.Apply(cfg, true); err != nil {
		t.Fatal(err)
	}
	if exchange(conn, "limited") == nil {
		t.Fatal("limit change left existing shared connections open")
	}
}

func TestExpiryWaitsWithoutSpendingAndUsesFreshDeadline(t *testing.T) {
	s, runtime, cfg := setup(t)
	rule := testRule("127.0.0.1:1")
	rule.Lease.ExpiresAt = time.Now().Add(80 * time.Millisecond)
	cfg.ValidUntil = rule.Lease.ExpiresAt
	cfg.Rules = []contract.Rule{rule}
	if err := runtime.Apply(cfg, true); err != nil {
		t.Fatal(err)
	}
	b := runtime.listeners[key(rule)]
	time.Sleep(time.Until(rule.Lease.ExpiresAt) + time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- b.chargeCurrent(ctx, rule.ID, "tcp", true, 11) }()
	select {
	case <-s.usageWake:
	case <-ctx.Done():
		t.Fatal("expired flow did not request renewal")
	}
	if used(s, rule.Lease.ID) != 0 {
		t.Fatal("expired allocation spent bytes")
	}
	renewed := *rule.Lease
	renewed.ID = "after-expiry"
	renewed.ExpiresAt = time.Now().Add(time.Minute)
	rule.Lease = &renewed
	cfg.Version++
	cfg.ValidUntil = renewed.ExpiresAt
	cfg.Rules = []contract.Rule{rule}
	if err := runtime.Apply(cfg, true); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal("flow retained the expired deadline", err)
	}
	if used(s, "lease") != 0 || used(s, renewed.ID) != 11 {
		t.Fatal("renewed bytes charged outside their allocation")
	}
}

func TestOversizedPayloadDoesNotContinuouslyRenewTinyAllocation(t *testing.T) {
	s, runtime, cfg := setup(t)
	rule := testRule("127.0.0.1:1")
	rule.Lease.Bytes = 10
	cfg.Rules = []contract.Rule{rule}
	if err := runtime.Apply(cfg, true); err != nil {
		t.Fatal(err)
	}
	b := runtime.listeners[key(rule)]
	if err := b.chargeCurrent(context.Background(), rule.ID, "tcp", true, 11); err == nil || transientMeterError(err) {
		t.Fatal("payload larger than a complete allocation should fail", err)
	}
	if len(s.Retirements()) != 0 || used(s, rule.Lease.ID) != 0 {
		t.Fatal("oversized payload churned or spent allocations")
	}
}
