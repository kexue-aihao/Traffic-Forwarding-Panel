package tunnel

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

func TestTargetCircuitSuppressesActualOriginDialAndRecovers(t *testing.T) {
	pair, roots := testCertificate(t)
	carrier, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	reserve, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	target := reserve.Addr().String()
	reserve.Close()
	s := &Server{TLS: &tls.Config{Certificates: []tls.Certificate{pair}}, Token: "test-circuit-token-long-123", Policy: &contract.EffectivePolicy{Failover: &contract.FailoverPolicy{MaxFail: 1, CooldownSec: 1}}}
	done := make(chan struct{})
	go func() { defer close(done); s.Serve(carrier, "tls") }()
	t.Cleanup(func() { s.Close(); <-done })
	client := Client{TLS: &tls.Config{RootCAs: roots}, RuleID: "rule", Timeout: time.Second}
	dial := func() (*Session, error) {
		return client.Dial(context.Background(), "tls", carrier.Addr().String(), "localhost", s.Token, "tcp", target)
	}
	if c, err := dial(); err == nil {
		c.Close()
		t.Fatal("unavailable origin opened")
	}
	origin, err := net.Listen("tcp", target)
	if err != nil {
		t.Fatal(err)
	}
	defer origin.Close()
	accepted := make(chan struct{}, 2)
	go func() {
		for {
			c, err := origin.Accept()
			if err != nil {
				return
			}
			accepted <- struct{}{}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	if c, err := dial(); err == nil {
		c.Close()
		t.Fatal("cooling origin was dialed")
	}
	select {
	case <-accepted:
		t.Fatal("cooldown still contacted origin")
	default:
	}
	time.Sleep(1100 * time.Millisecond)
	c, err := dial()
	if err != nil {
		t.Fatal("half-open origin did not recover", err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(time.Second))
	c.Write([]byte("ok"))
	var b [2]byte
	if _, err = io.ReadFull(c, b[:]); err != nil || string(b[:]) != "ok" {
		t.Fatalf("recovered business %q %v", b, err)
	}
}
