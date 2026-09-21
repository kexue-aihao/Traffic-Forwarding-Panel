package agent

import (
	"bytes"
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

func limitsEcho(t *testing.T) (string, string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	u, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { u.Close() })
	go func() {
		p := make([]byte, 65535)
		for {
			n, peer, err := u.ReadFromUDP(p)
			if err != nil {
				return
			}
			u.WriteToUDP(p[:n], peer)
		}
	}()
	return l.Addr().String(), u.LocalAddr().String()
}

func TestSharedTCPUDPConnectionLimitsAndRelease(t *testing.T) {
	_, r, cfg := setup(t)
	tcp, udp := limitsEcho(t)
	a := testRule(tcp)
	a.UserID = "alice"
	a.Lease.Limits = contract.ResourceLimits{MaxConnectionsPerNode: 2, MaxIPsPerNode: 1}
	b := testRule(udp)
	b.ID = "udp-rule"
	b.UserID = "alice"
	b.Network = "udp"
	b.Lease.ID = "udp-lease"
	b.Lease.Limits = a.Lease.Limits
	cfg.Rules = []contract.Rule{a, b}
	if err := r.Apply(cfg, true); err != nil {
		t.Fatal(err)
	}
	tcpAddr := r.listeners[key(a)].tcp.Addr().String()
	udpAddr := r.listeners[key(b)].udp.LocalAddr().String()
	echo := func(c net.Conn) error {
		c.SetDeadline(time.Now().Add(time.Second))
		if _, err := c.Write([]byte("x")); err != nil {
			return err
		}
		p := make([]byte, 1)
		_, err := io.ReadFull(c, p)
		return err
	}
	first, err := net.Dial("tcp", tcpAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if err = echo(first); err != nil {
		t.Fatal(err)
	}
	second, err := net.Dial("udp", udpAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err = echo(second); err != nil {
		t.Fatal(err)
	}
	third, err := net.Dial("tcp", tcpAddr)
	if err != nil {
		t.Fatal(err)
	}
	if err = echo(third); err == nil {
		t.Fatal("TCP bypassed active UDP session limit")
	}
	third.Close()
	// Retirement cancels the UDP session and releases its account slot.
	r.StopLease(b.Lease.ID)
	deadline := time.Now().Add(2 * time.Second)
	for {
		c, err := net.Dial("tcp", tcpAddr)
		if err == nil {
			err = echo(c)
			c.Close()
		}
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("retired session leaked account slot", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestActiveIPCanonicalizationAndRelease(t *testing.T) {
	p := &resourcePool{}
	p.configure(contract.ResourceLimits{MaxIPsPerNode: 1})
	a, ok := p.acquire(&net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 1})
	if !ok {
		t.Fatal("first IP rejected")
	}
	b, ok := p.acquire(&net.TCPAddr{IP: net.ParseIP("::ffff:192.0.2.1"), Port: 2})
	if !ok {
		t.Fatal("mapped IPv4 double counted")
	}
	other := &net.TCPAddr{IP: net.ParseIP("2001:db8::2"), Port: 3}
	if release, ok := p.acquire(other); ok {
		release()
		t.Fatal("distinct IP passed limit")
	}
	a()
	a()
	if release, ok := p.acquire(other); ok {
		release()
		t.Fatal("IP removed with one connection still active")
	}
	b()
	release, ok := p.acquire(other)
	if !ok {
		t.Fatal("closed IP retained")
	}
	release()
}

func TestSharedBandwidthTCPAndRefreshCancellation(t *testing.T) {
	_, r, cfg := setup(t)
	target, _ := limitsEcho(t)
	a := testRule(target)
	a.UserID = "alice"
	a.Lease.Limits = contract.ResourceLimits{BytesPerSecondPerNode: 256 << 10}
	cfg.Rules = []contract.Rule{a}
	if err := r.Apply(cfg, true); err != nil {
		t.Fatal(err)
	}
	addr := r.listeners[key(a)].tcp.Addr().String()
	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			c, err := net.Dial("tcp", addr)
			if err != nil {
				errs <- err
				return
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(8 * time.Second))
			payload := bytes.Repeat([]byte("b"), 64<<10)
			_, err = c.Write(payload)
			if err == nil {
				err = c.(*net.TCPConn).CloseWrite()
			}
			if err != nil {
				errs <- err
				return
			}
			got, err := io.ReadAll(c)
			if err == nil && !bytes.Equal(got, payload) {
				err = io.ErrUnexpectedEOF
			}
			errs <- err
		})
	}
	// Polling the same config must not refill the shared burst.
	for range 4 {
		time.Sleep(25 * time.Millisecond)
		if err := r.Apply(cfg, false); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed < 650*time.Millisecond {
		t.Fatal("shared upload+download rate exceeded", elapsed)
	}
	p := r.pools[limitOwner(a)]
	p.configure(contract.ResourceLimits{BytesPerSecondPerNode: 1})
	p.mu.Lock()
	p.tokens = 0
	p.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.wait(ctx, 32768) }()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled limiter allowed bytes")
		}
	case <-time.After(time.Second):
		t.Fatal("rate wait leaked after cancellation")
	}
}
