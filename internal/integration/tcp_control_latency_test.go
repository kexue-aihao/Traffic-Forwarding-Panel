package integration

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/tunnel"
)

// Delay every Agent HTTP endpoint, including config/prefetch/retirement.
// Unlike a fixed-lease benchmark, this exercises the real commercial budget
// pipeline across repeated allocations and concurrent streams on one rule.
func TestTCPDownloadAcrossControlPlaneLatency(t *testing.T) {
	total := 512 << 20
	if value := os.Getenv("TFP_TEST_TCP_DOWNLOAD_MIB"); value != "" {
		mib, err := strconv.Atoi(value)
		if err != nil || mib < 512 || mib > 4096 {
			t.Fatal("TFP_TEST_TCP_DOWNLOAD_MIB must be between 512 and 4096")
		}
		total = mib << 20
	}
	for _, transport := range []string{"direct", "tls-mux"} {
		for _, delay := range []time.Duration{60 * time.Millisecond, 240 * time.Millisecond} {
			for _, streams := range []int{1, 8} {
				t.Run(fmt.Sprintf("%s_%dms_%dstreams", transport, delay.Milliseconds(), streams), func(t *testing.T) {
					pair, roots := certificate(t)
					f := newFixture(t, tunnel.Client{TLS: &tls.Config{RootCAs: roots}})
					// Enlarge only this isolated fixture's entitlement; allocations
					// still go through the actual commercial allocator and ledger.
					if _, err := f.db.DB.Exec(f.db.Rebind("UPDATE commerce_entitlements SET quota=? WHERE id=?"), max(int64(4<<30), int64(total)*2), f.ent.ID); err != nil {
						t.Fatal(err)
					}
					origin, err := net.Listen("tcp", "127.0.0.1:0")
					if err != nil {
						t.Fatal(err)
					}
					defer origin.Close()
					payload := bytes.Repeat([]byte("latency-download"), 2048)
					sent := make(chan error, streams)
					go func() {
						for range streams {
							conn, err := origin.Accept()
							if err != nil {
								sent <- err
								return
							}
							go func() {
								defer conn.Close()
								conn.SetDeadline(time.Now().Add(time.Minute))
								if _, err := io.ReadFull(conn, make([]byte, 1)); err != nil {
									sent <- err
									return
								}
								for range total / streams / len(payload) {
									if _, err := io.Copy(conn, bytes.NewReader(payload)); err != nil {
										sent <- err
										return
									}
								}
								sent <- nil
							}()
						}
					}()
					var params *contract.Tunnel
					carrier := "direct"
					if transport == "tls-mux" {
						listener, err := net.Listen("tcp", "127.0.0.1:0")
						if err != nil {
							t.Fatal(err)
						}
						exit := &tunnel.Server{TLS: &tls.Config{Certificates: []tls.Certificate{pair}}, Token: "control-latency-exit", Obfuscation: &contract.ObfuscationConfig{Strategy: "random-padding"}}
						done := make(chan struct{})
						go func() { defer close(done); _ = exit.Serve(listener, "tls") }()
						defer func() { exit.Close(); <-done }()
						params = &contract.Tunnel{Endpoint: listener.Addr().String(), ServerName: "localhost", Token: exit.Token, Mux: true, Obfuscation: exit.Obfuscation}
						carrier = "tls"
					}
					rule := f.rule("tcp", carrier, origin.Addr().String(), params)
					f.sync()
					first := *f.lease(rule.ID)
					var prefetches, retirements atomic.Int64
					delayed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						select {
						case <-time.After(delay):
						case <-r.Context().Done():
							return
						}
						switch r.URL.Path {
						case "/api/v1/agent/leases/prefetch":
							prefetches.Add(1)
						case "/api/v1/agent/leases/retire":
							retirements.Add(1)
						}
						f.app.Handler.ServeHTTP(w, r)
					}))
					defer delayed.Close()
					f.agent.URL, f.agent.HTTP = delayed.URL, delayed.Client()
					ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
					finished := make(chan error, 1)
					go func() { finished <- f.agent.Run(ctx) }()
					defer func() { cancel(); <-finished }()
					var readers sync.WaitGroup
					failures := make(chan error, streams)
					var maxGap atomic.Int64
					start := make(chan struct{})
					for range streams {
						conn, err := net.Dial("tcp", rule.Listen)
						if err != nil {
							t.Fatal(err)
						}
						readers.Go(func() {
							defer conn.Close()
							conn.SetDeadline(time.Now().Add(time.Minute))
							<-start
							if _, err := conn.Write([]byte{1}); err != nil {
								failures <- err
								return
							}
							buf := make([]byte, len(payload))
							previous := time.Now()
							for range total / streams / len(payload) {
								if _, err := io.ReadFull(conn, buf); err != nil || !bytes.Equal(buf, payload) {
									failures <- fmt.Errorf("download failed: %v", err)
									return
								}
								now := time.Now()
								gap := int64(now.Sub(previous))
								for old := maxGap.Load(); gap > old; old = maxGap.Load() {
									if maxGap.CompareAndSwap(old, gap) {
										break
									}
								}
								previous = now
							}
						})
					}
					started := time.Now()
					close(start)
					readers.Wait()
					elapsed := time.Since(started)
					close(failures)
					for err := range failures {
						t.Fatal(err)
					}
					for range streams {
						if err := <-sent; err != nil {
							t.Fatal(err)
						}
					}
					if err := f.syncContext(ctx); err != nil {
						t.Fatal(err)
					}
					if got := f.number("SELECT used FROM commerce_entitlements WHERE id=?", f.ent.ID); got != int64(total)+int64(streams) {
						t.Fatal("control latency lost or duplicated billed bytes", got)
					}
					if f.number("SELECT closed FROM commerce_lease_reservations WHERE lease_id=?", first.ID) != 1 || prefetches.Load() < 2 {
						t.Fatal("download did not cross repeated real commercial allocations")
					}
					t.Logf("%.2f Mbps, %v, max-read-gap %.2f ms, prefetches %d, retirements %d", float64(total)*8/elapsed.Seconds()/1e6, elapsed, float64(maxGap.Load())/float64(time.Millisecond), prefetches.Load(), retirements.Load())
				})
			}
		}
	}
}
