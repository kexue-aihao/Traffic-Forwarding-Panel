package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/tunnel"
)

// Continuous single-stream downloads over real loopback TCP/TLS sockets.
// The metered mode runs the actual Runtime, durable Store and random padding;
// a large finite lease isolates the data path from control-plane renewal.
func BenchmarkSecureDirectDownload(b *testing.B) {
	pair, roots := ingressTestCertificate(b)
	for _, mode := range []string{"tcp_unmetered", "secure_direct_unmetered", "secure_direct_metered", "secure_direct_metered_3ms", "secure_direct_controlled", "secure_direct_controlled_100ms", "secure_direct_controlled_3ms_100ms"} {
		b.Run(mode, func(b *testing.B) {
			controlled := strings.HasPrefix(mode, "secure_direct_controlled")
			origin, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				b.Fatal(err)
			}
			defer origin.Close()
			accepted := make(chan net.Conn, 1)
			go func() { c, _ := origin.Accept(); accepted <- c }()
			target := origin.Addr().String()
			var client net.Conn
			var store *Store
			if mode == "tcp_unmetered" {
				client, err = net.Dial("tcp", target)
			} else {
				listener, e := net.Listen("tcp", "127.0.0.1:0")
				if e != nil {
					b.Fatal(e)
				}
				obfs := &contract.ObfuscationConfig{Strategy: "random-padding"}
				server := &tunnel.Server{TLS: &tls.Config{Certificates: []tls.Certificate{pair}}, Token: "diagnostic-test-only", Obfuscation: obfs}
				done := make(chan struct{})
				go func() { defer close(done); _ = server.Serve(listener, "secure-direct") }()
				defer func() { server.Close(); <-done }()
				tlsClient := tunnel.Client{TLS: &tls.Config{RootCAs: roots}}
				route := contract.Tunnel{Endpoint: listener.Addr().String(), ServerName: "a.example.com", Token: server.Token, Obfuscation: obfs}
				if mode == "secure_direct_unmetered" {
					client, err = tlsClient.DialRoute(context.Background(), "secure-direct", "tcp", target, route)
				} else {
					store, err = OpenStore(filepath.Join(b.TempDir(), "state.json"))
					if err != nil {
						b.Fatal(err)
					}
					defer store.Close()
					if err = store.SetIdentity(contract.Registered{NodeID: "node", Token: "diagnostic"}); err != nil {
						b.Fatal(err)
					}
					runtime := NewRuntime(store, tlsClient)
					defer runtime.Close()
					rule := testRule(target)
					rule.Listen, rule.Transport, rule.Tunnel = ingressTestAddress(b), "secure-direct", &route
					rule.UserID = "diagnostic-owner"
					rule.Lease.Bytes = 1 << 40
					cfg := contract.Config{ContractVersion: 1, NodeID: "node", Version: 1, ValidUntil: time.Now().Add(time.Hour), Rules: []contract.Rule{rule}}
					if err = runtime.Apply(cfg, true); err != nil {
						b.Fatal(err)
					}
					if controlled {
						server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
							switch req.URL.Path {
							case "/api/v1/agent/config":
								json.NewEncoder(w).Encode(cfg)
							case "/api/v1/agent/usage":
								if strings.HasSuffix(mode, "100ms") {
									select {
									case <-time.After(100 * time.Millisecond):
									case <-req.Context().Done():
										return
									}
								}
								var batch contract.UsageBatch
								if err := json.NewDecoder(req.Body).Decode(&batch); err != nil {
									w.WriteHeader(http.StatusBadRequest)
									return
								}
								ids := make([]string, 0, len(batch.Records))
								for _, u := range batch.Records {
									ids = append(ids, u.ID)
								}
								json.NewEncoder(w).Encode(map[string]any{"accepted": ids})
							default:
								w.WriteHeader(http.StatusNoContent)
							}
						}))
						defer server.Close()
						agent := &Agent{Store: store, Runtime: runtime, URL: server.URL, HTTP: server.Client(), PollInterval: time.Hour}
						ctx, cancel := context.WithCancel(context.Background())
						finished := make(chan error, 1)
						go func() { finished <- agent.Run(ctx) }()
						defer func() { cancel(); <-finished }()
					}
					client, err = net.Dial("tcp", rule.Listen)
				}
			}
			if err != nil {
				b.Fatal(err)
			}
			defer client.Close()
			var peer net.Conn
			select {
			case peer = <-accepted:
			case <-time.After(10 * time.Second):
				b.Fatal("origin connection timeout")
			}
			if peer == nil {
				b.Fatal("origin accept failed")
			}
			defer peer.Close()
			payload := bytes.Repeat([]byte("continuous-download"), 2048)[:32*1024]
			received := make([]byte, len(payload))
			var largestGap time.Duration
			run := func(count int) error {
				client.SetDeadline(time.Now().Add(time.Minute))
				peer.SetDeadline(time.Now().Add(time.Minute))
				sent := make(chan error, 1)
				go func() {
					for range count {
						remaining := payload
						for len(remaining) > 0 {
							n, e := peer.Write(remaining)
							if e != nil {
								sent <- e
								return
							}
							if n == 0 {
								sent <- io.ErrShortWrite
								return
							}
							remaining = remaining[n:]
						}
					}
					sent <- nil
				}()
				previous := time.Now()
				for range count {
					if _, e := io.ReadFull(client, received); e != nil {
						peer.Close()
						<-sent
						return e
					}
					if !bytes.Equal(payload, received) {
						peer.Close()
						<-sent
						return fmt.Errorf("corrupt payload")
					}
					if controlled {
						now := time.Now()
						largestGap = max(largestGap, now.Sub(previous))
						previous = now
					}
				}
				return <-sent
			}
			if err = run(16); err != nil {
				b.Fatal(err)
			}
			var syncStart time.Time
			var syncTime time.Duration
			syncs := 0
			if store != nil {
				store.mu.Lock()
				store.fault = func(point string) error {
					if point == "sync_before" {
						syncStart = time.Now()
						if strings.Contains(mode, "3ms") {
							// Simulate a slower WAL volume without changing durability.
							time.Sleep(3 * time.Millisecond)
						}
					}
					if point == "sync_after" {
						syncTime += time.Since(syncStart)
						syncs++
					}
					return nil
				}
				store.mu.Unlock()
			}
			b.SetBytes(int64(len(payload)))
			b.ReportAllocs()
			largestGap = 0
			b.ResetTimer()
			if err = run(b.N); err != nil {
				b.Fatal(err)
			}
			b.StopTimer()
			if controlled {
				b.ReportMetric(float64(largestGap.Microseconds())/1000, "max-read-gap-ms")
			}
			if store != nil {
				store.mu.Lock()
				store.fault = nil
				b.ReportMetric(float64(syncs)/float64(b.N), "syncs/32KiB")
				if syncs > 0 {
					b.ReportMetric(float64(syncTime.Nanoseconds())/float64(syncs)/1000, "fsync-us")
				}
				store.mu.Unlock()
			}
		})
	}
}
