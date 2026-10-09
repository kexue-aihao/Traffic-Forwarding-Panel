package integration

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/tunnel"
	"net"
	"testing"
	"time"
)

func TestUDPDatagramAndDirectCrossFiniteCommercialLeases(t *testing.T) {
	for _, transport := range []string{"direct", "quic"} {
		t.Run(transport, func(t *testing.T) {
			pair, roots := certificate(t)
			f := newFixture(t, tunnel.Client{TLS: &tls.Config{RootCAs: roots}})
			_, target := targets(t)
			var params *contract.Tunnel
			if transport == "quic" {
				s := &tunnel.DatagramServer{TLS: &tls.Config{Certificates: []tls.Certificate{pair}}, Token: "integration-udp-token-0123456"}
				done := make(chan error, 1)
				go func() { done <- s.Serve("127.0.0.1:0") }()
				deadline := time.Now().Add(3 * time.Second)
				for s.Addr() == nil && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				if s.Addr() == nil {
					t.Fatal("UDP exit did not start")
				}
				t.Cleanup(func() { s.Close(); <-done })
				params = &contract.Tunnel{Endpoint: s.Addr().String(), ServerName: "localhost", Token: s.Token}
			}
			rule := f.rule("udp", transport, target, params)
			f.sync()
			first := *f.lease(rule.ID)
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			done := make(chan error, 1)
			go func() { done <- f.agent.Run(ctx) }()
			defer func() {
				cancel()
				if e := <-done; e != nil {
					t.Error(e)
				}
			}()
			conn, e := net.Dial("udp", rule.Listen)
			if e != nil {
				t.Fatal(e)
			}
			defer conn.Close()
			payload := bytes.Repeat([]byte("u"), 1200)
			got := make([]byte, 65535)
			const count = 7500 // 18,000,000 combined bytes, exceeding the first 16 MiB lease.
			for i := 0; i < count; i++ {
				binary.BigEndian.PutUint64(payload[:8], uint64(i))
				success := false
				for attempt := 0; attempt < 5; attempt++ {
					conn.SetDeadline(time.Now().Add(500 * time.Millisecond))
					if _, e = conn.Write(payload); e != nil {
						t.Fatal(e)
					}
					for {
						n, e := conn.Read(got)
						if e != nil {
							break
						}
						if n == len(payload) && bytes.Equal(got[:n], payload) {
							success = true
							break
						}
					}
					if success {
						break
					}
				}
				if !success {
					t.Fatalf("flow stopped at packet %d: context=%v udp=%+v", i, ctx.Err(), f.agent.Runtime.UDPStats())
				}
			}
			f.sync()
			if f.lease(rule.ID).ID == first.ID { // Give background final retirement one cycle.
				if e := f.store.Retire(first.ID); e != nil {
					t.Fatal(e)
				}
				f.sync()
			}
			if f.lease(rule.ID).ID == first.ID {
				t.Fatal("finite allocation never renewed")
			}
			facts := f.number("SELECT used FROM commerce_entitlements WHERE id=?", f.ent.ID)
			if facts < 2*count*int64(len(payload)) || facts > f.ent.Quota {
				t.Fatal("cross-lease quota/accounting invariant violated", facts)
			}
			if f.number("SELECT closed FROM commerce_lease_reservations WHERE lease_id=?", first.ID) != 1 {
				t.Fatal("old lease not durably settled and returned")
			}
			if f.number("SELECT COUNT(*) FROM cp_usage WHERE rule_id=?", rule.ID) > count/2 {
				t.Fatal("UDP still produces a durable fact per datagram")
			}
			if len(f.agent.Runtime.UDPStats()) != 1 {
				t.Fatal("UDP path statistics missing")
			}
		})
	}
}
