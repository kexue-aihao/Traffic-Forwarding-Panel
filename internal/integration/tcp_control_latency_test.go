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
type tcpControlScenario struct {
	transport                  string
	streams, total, sourceMbps int
	delay                      func(time.Duration, int64) time.Duration
}

func tcpControlTotal(t *testing.T, fallback int) int {
	t.Helper()
	if value := os.Getenv("TFP_TEST_TCP_DOWNLOAD_MIB"); value != "" {
		mib, err := strconv.Atoi(value)
		if err != nil || mib < 512 || mib > 16384 {
			t.Fatal("TFP_TEST_TCP_DOWNLOAD_MIB must be between 512 and 16384")
		}
		return mib << 20
	}
	return fallback
}

func TestTCPDownloadAcrossControlPlaneLatency(t *testing.T) {
	total := tcpControlTotal(t, 512<<20)
	for _, transport := range []string{"direct", "tls-mux"} {
		for _, delay := range []time.Duration{60 * time.Millisecond, 240 * time.Millisecond} {
			for _, streams := range []int{1, 8} {
				t.Run(fmt.Sprintf("%s_%dms_%dstreams", transport, delay.Milliseconds(), streams), func(t *testing.T) {
					testTCPControlDownload(t, tcpControlScenario{transport: transport, streams: streams, total: total, delay: func(time.Duration, int64) time.Duration { return delay }})
				})
			}
		}
	}
}

// A slow/jittery control plane must not interrupt a single authorized TCP
// stream as its finite allocations rotate. Measure each second and verify all
// bytes and accounting, without a machine-dependent throughput assertion.
func TestTCPDownloadAcrossControlPlaneJitter(t *testing.T) {
	total := tcpControlTotal(t, 1024<<20)
	for _, transport := range []string{"direct", "tls-mux"} {
		for _, streams := range []int{1, 8} {
			t.Run(fmt.Sprintf("%s_%dstreams", transport, streams), func(t *testing.T) {
				testTCPControlDownload(t, tcpControlScenario{transport: transport, streams: streams, total: total, sourceMbps: 2400, delay: func(elapsed time.Duration, request int64) time.Duration {
					if elapsed < time.Second {
						return 120 * time.Millisecond
					}
					return []time.Duration{360 * time.Millisecond, 480 * time.Millisecond, 600 * time.Millisecond}[request%3]
				}})
			})
		}
	}
}

func testTCPControlDownload(t *testing.T, scenario tcpControlScenario) {
	t.Helper()
	pair, roots := certificate(t)
	f := newFixture(t, tunnel.Client{TLS: &tls.Config{RootCAs: roots}})
	// Enlarge only this isolated fixture's entitlement; allocations
	// still go through the actual commercial allocator and ledger.
	if _, err := f.db.DB.Exec(f.db.Rebind("UPDATE commerce_entitlements SET quota=? WHERE id=?"), max(int64(4<<30), int64(scenario.total)*2), f.ent.ID); err != nil {
		t.Fatal(err)
	}
	origin, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer origin.Close()
	payload := bytes.Repeat([]byte("latency-download"), 2048)
	sent := make(chan error, scenario.streams)
	go func() {
		for range scenario.streams {
			conn, err := origin.Accept()
			if err != nil {
				sent <- err
				return
			}
			go func() {
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(3 * time.Minute))
				if _, err := io.ReadFull(conn, make([]byte, 1)); err != nil {
					sent <- err
					return
				}
				blocks := scenario.total / scenario.streams / len(payload)
				for block := 0; block < blocks; {
					batch := min(128, blocks-block)
					started := time.Now()
					for range batch {
						if _, err := io.Copy(conn, bytes.NewReader(payload)); err != nil {
							sent <- err
							return
						}
					}
					block += batch
					if scenario.sourceMbps > 0 {
						// Bound the offered rate in roughly 4 MiB batches. Time blocked
						// writing consumes the interval; missed intervals never accrue
						// as an unlimited catch-up burst. This is not TCP RTT/loss.
						interval := time.Duration(float64(batch*len(payload)) * 8 / float64(scenario.sourceMbps/scenario.streams) / 1e6 * float64(time.Second))
						if wait := interval - time.Since(started); wait > 0 {
							time.Sleep(wait)
						}
					}
				}
				sent <- nil
			}()
		}
	}()
	var params *contract.Tunnel
	carrier := "direct"
	if scenario.transport == "tls-mux" {
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
	startedControl := time.Now()
	var requests atomic.Int64
	delayed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(scenario.delay(time.Since(startedControl), requests.Add(1))):
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
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	finished := make(chan error, 1)
	go func() { finished <- f.agent.Run(ctx) }()
	defer func() { cancel(); <-finished }()
	var readers sync.WaitGroup
	failures := make(chan error, scenario.streams)
	var maxGap, received atomic.Int64
	start := make(chan struct{})
	for range scenario.streams {
		conn, err := net.Dial("tcp", rule.Listen)
		if err != nil {
			t.Fatal(err)
		}
		readers.Go(func() {
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(3 * time.Minute))
			<-start
			if _, err := conn.Write([]byte{1}); err != nil {
				failures <- err
				return
			}
			buf := make([]byte, len(payload))
			previous := time.Now()
			for range scenario.total / scenario.streams / len(payload) {
				if _, err := io.ReadFull(conn, buf); err != nil || !bytes.Equal(buf, payload) {
					failures <- fmt.Errorf("download failed: %v", err)
					return
				}
				received.Add(int64(len(buf)))
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
	metricsDone, metricsStop := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(metricsDone)
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		previous := int64(0)
		at := started
		for {
			select {
			case <-metricsStop:
				return
			case now := <-tick.C:
				current := received.Load()
				if scenario.sourceMbps > 0 {
					t.Logf("second %d: %.2f Mbps", int(now.Sub(started).Seconds()+0.5), float64(current-previous)*8/now.Sub(at).Seconds()/1e6)
				}
				previous, at = current, now
			}
		}
	}()
	close(start)
	readers.Wait()
	elapsed := time.Since(started)
	close(metricsStop)
	<-metricsDone
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	for range scenario.streams {
		if err := <-sent; err != nil {
			t.Fatal(err)
		}
	}
	if err := f.syncContext(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.number("SELECT used FROM commerce_entitlements WHERE id=?", f.ent.ID); got != int64(scenario.total)+int64(scenario.streams) {
		t.Fatal("control latency lost or duplicated billed bytes", got)
	}
	if f.number("SELECT closed FROM commerce_lease_reservations WHERE lease_id=?", first.ID) != 1 || f.number("SELECT COUNT(*) FROM commerce_leases WHERE rule_id=?", rule.ID) < 3 {
		t.Fatal("download did not cross repeated real commercial allocations")
	}
	t.Logf("%.2f Mbps, %v, max-read-gap %.2f ms, prefetches %d, retirements %d", float64(scenario.total)*8/elapsed.Seconds()/1e6, elapsed, float64(maxGap.Load())/float64(time.Millisecond), prefetches.Load(), retirements.Load())
}
