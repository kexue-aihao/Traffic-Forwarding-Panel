package integration

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/tunnel"
)

// Download continuously, rather than alternating request/echo chunks. The
// real commercial ledger, lease pipeline and a delayed usage endpoint stay
// active throughout, reproducing control-plane pressure late in a speed test.
func TestContinuousTCPDownloadWithDelayedUsageAcrossLeases(t *testing.T) {
	for _, transport := range []string{"direct", "secure-direct", "tls-mux"} {
		t.Run(transport, func(t *testing.T) {
			pair, roots := certificate(t)
			f := newFixture(t, tunnel.Client{TLS: &tls.Config{RootCAs: roots}})
			origin, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer origin.Close()
			const size = 48 << 20
			payload := bytes.Repeat([]byte("continuous-lease-download"), 1500)[:32<<10]
			sent := make(chan error, 1)
			go func() {
				conn, err := origin.Accept()
				if err != nil {
					sent <- err
					return
				}
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(20 * time.Second))
				// Wait for a one-byte marker so Agent.Run is ready before demand.
				if _, err := io.ReadFull(conn, make([]byte, 1)); err != nil {
					sent <- err
					return
				}
				for range size / len(payload) {
					if _, err := io.Copy(conn, bytes.NewReader(payload)); err != nil {
						sent <- err
						return
					}
				}
				sent <- nil
			}()
			var params *contract.Tunnel
			carrier := transport
			if transport != "direct" {
				if transport == "tls-mux" {
					carrier = "tls"
				}
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				obfs := &contract.ObfuscationConfig{Strategy: "random-padding"}
				exit := &tunnel.Server{TLS: &tls.Config{Certificates: []tls.Certificate{pair}}, Token: "continuous-download-token", Obfuscation: obfs}
				finished := make(chan struct{})
				go func() { defer close(finished); _ = exit.Serve(listener, carrier) }()
				defer func() { exit.Close(); <-finished }()
				params = &contract.Tunnel{Endpoint: listener.Addr().String(), ServerName: "localhost", Token: exit.Token, Mux: transport == "tls-mux", Obfuscation: obfs}
			}
			rule := f.rule("tcp", carrier, origin.Addr().String(), params)
			f.sync()
			first := *f.lease(rule.ID)
			var mu sync.Mutex
			var prefetches, posts int
			delayed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/agent/usage" {
					select {
					case <-time.After(100 * time.Millisecond):
					case <-r.Context().Done():
						return
					}
					mu.Lock()
					posts++
					mu.Unlock()
				} else if r.URL.Path == "/api/v1/agent/leases/prefetch" {
					mu.Lock()
					prefetches++
					mu.Unlock()
				}
				f.app.Handler.ServeHTTP(w, r)
			}))
			defer delayed.Close()
			f.agent.URL, f.agent.HTTP = delayed.URL, delayed.Client()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			done := make(chan error, 1)
			go func() { done <- f.agent.Run(ctx) }()
			defer func() { cancel(); <-done }()
			conn, err := net.Dial("tcp", rule.Listen)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(15 * time.Second))
			if _, err := conn.Write([]byte{1}); err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, len(payload))
			var largestGap time.Duration
			started := time.Now()
			previous := started
			for range size / len(payload) {
				if _, err := io.ReadFull(conn, buf); err != nil || !bytes.Equal(buf, payload) {
					t.Fatal("continuous download stalled or corrupted during renewal", err)
				}
				now := time.Now()
				largestGap = max(largestGap, now.Sub(previous))
				previous = now
			}
			elapsed := time.Since(started)
			if err := <-sent; err != nil {
				t.Fatal(err)
			}
			conn.Close()
			// Settle while workers are still active, before Runtime.Close.
			if err := f.agent.Step(ctx); err != nil && !strings.Contains(err.Error(), "POST /agent/ack: HTTP 409") {
				t.Fatal("settlement failed", err)
			}
			if got := f.number("SELECT used FROM commerce_entitlements WHERE id=?", f.ent.ID); got != size+1 {
				t.Fatal("continuous cross-lease download changed billing", got, size+1)
			}
			if f.number("SELECT closed FROM commerce_lease_reservations WHERE lease_id=?", first.ID) != 1 {
				t.Fatal("consumed initial allocation was not settled")
			}
			mu.Lock()
			prefetched, reported := prefetches, posts
			mu.Unlock()
			if prefetched == 0 || reported == 0 || largestGap > 3*time.Second {
				t.Fatal("download failed to exercise a healthy pipeline", prefetched, reported, largestGap)
			}
			info, _ := json.Marshal(map[string]any{"MBps": float64(size) / elapsed.Seconds() / 1e6, "max_read_gap_ms": largestGap.Milliseconds(), "prefetches": prefetched, "usage_posts": reported})
			t.Log(string(info))
		})
	}
}
