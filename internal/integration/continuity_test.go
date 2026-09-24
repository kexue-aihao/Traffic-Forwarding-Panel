package integration

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/tunnel"
)

func TestOneTCPConnectionCrossesCommercialLeaseBudgets(t *testing.T) {
	for _, transport := range []string{"direct", "tls"} {
		t.Run(transport, func(t *testing.T) {
			pair, roots := certificate(t)
			f := newFixture(t, tunnel.Client{TLS: &tls.Config{RootCAs: roots}})
			target, _ := targets(t)
			var params *contract.Tunnel
			if transport == "tls" {
				l, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				exit := &tunnel.Server{TLS: &tls.Config{Certificates: []tls.Certificate{pair}}, Token: "continuity-exit-secret", Allowed: map[string]bool{"tcp|" + target: true}}
				done := make(chan struct{})
				go func() { defer close(done); exit.Serve(l, transport) }()
				t.Cleanup(func() { exit.Close(); <-done })
				params = &contract.Tunnel{Endpoint: l.Addr().String(), ServerName: "localhost", Token: exit.Token, Mux: true}
			}
			rule := f.rule("tcp", transport, target, params)
			f.sync()
			first := *f.lease(rule.ID)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			done := make(chan error, 1)
			go func() { done <- f.agent.Run(ctx) }()
			defer func() {
				cancel()
				if err := <-done; err != nil {
					t.Error(err)
				}
			}()
			conn, err := net.DialTimeout("tcp", rule.Listen, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(25 * time.Second))
			payload := bytes.Repeat([]byte("c"), 32<<10)
			got := make([]byte, len(payload))
			const chunks = 320 // 20 MiB combined, exceeding the 16 MiB lease.
			for i := range chunks {
				if _, err := conn.Write(payload); err != nil {
					t.Fatalf("same connection stopped writing at chunk %d: %v", i, err)
				}
				if _, err := io.ReadFull(conn, got); err != nil {
					t.Fatalf("same connection stopped reading at chunk %d: %v", i, err)
				}
				if !bytes.Equal(got, payload) {
					t.Fatal("renewal changed payload")
				}
			}
			f.sync()
			if f.lease(rule.ID).ID == first.ID {
				t.Fatal("transfer never renewed its allocation")
			}
			if f.number("SELECT closed FROM commerce_lease_reservations WHERE lease_id=?", first.ID) != 1 {
				t.Fatal("old allocation was not returned")
			}
			if got := f.number("SELECT used FROM commerce_entitlements WHERE id=?", f.ent.ID); got != 2*chunks*int64(len(payload)) {
				t.Fatalf("cross-lease traffic was lost or counted twice: %d", got)
			}
			// A purchase starts a new period without changing the existing socket.
			old := f.ent
			f.ent = f.purchase("live-renewal", old.Version)
			f.sync()
			if _, err := conn.Write([]byte("new-cycle")); err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadFull(conn, got[:9]); err != nil {
				t.Fatal("purchase disconnected the existing stream", err)
			}
			f.sync()
			if f.number("SELECT used FROM commerce_entitlements WHERE id=?", f.ent.ID) != 18 {
				t.Fatal("existing stream did not switch to the new billing period")
			}
			if f.number("SELECT used FROM commerce_entitlements WHERE id=?", old.ID) != 2*chunks*int64(len(payload)) {
				t.Fatal("new-period bytes charged the previous entitlement")
			}
		})
	}
}
