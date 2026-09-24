package tunnel

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

type benchmarkConn struct {
	net.Conn
	r io.Reader
	w io.Writer
}

func (c benchmarkConn) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c benchmarkConn) Write(p []byte) (int, error) { return c.w.Write(p) }

// Isolate framing allocations from encryption, sockets and durable metering.
func BenchmarkSessionFrames(b *testing.B) {
	for _, size := range []int{1024, 32 * 1024, maxFrame} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			payload := bytes.Repeat([]byte("x"), size)
			b.Run("write", func(b *testing.B) {
				s := &Session{Conn: benchmarkConn{w: io.Discard}}
				b.SetBytes(int64(size))
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := s.Write(payload); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("read", func(b *testing.B) {
				wire := make([]byte, 5+size)
				wire[0] = dataFrame
				binary.BigEndian.PutUint32(wire[1:5], uint32(size))
				copy(wire[5:], payload)
				r := bytes.NewReader(wire)
				s := &Session{Conn: benchmarkConn{r: r}}
				buf := make([]byte, size)
				b.SetBytes(int64(size))
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					r.Reset(wire)
					if _, err := io.ReadFull(s, buf); err != nil {
						b.Fatal(err)
					}
				}
			})
		})
	}
}

// A persistent stream through a real loopback exit and TCP echo target.
// MB/s counts payload in one direction; each operation also echoes it back.
func BenchmarkCarrierRoundTrip(b *testing.B) {
	pair, roots := testCertificate(b)
	target, _ := echoServers(b)
	for _, transport := range []string{"tls", "ws", "wss", "http"} {
		for _, mux := range []bool{false, true} {
			b.Run(fmt.Sprintf("%s/mux=%t", transport, mux), func(b *testing.B) {
				l, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					b.Fatal(err)
				}
				s := &Server{TLS: &tls.Config{Certificates: []tls.Certificate{pair}}, Token: "benchmark-test-token", Allowed: map[string]bool{"tcp|" + target: true}}
				done := make(chan struct{})
				go func() { defer close(done); _ = s.Serve(l, transport) }()
				defer func() { s.Close(); <-done }()
				endpoint := l.Addr().String()
				if transport == "ws" || transport == "wss" {
					endpoint = transport + "://" + endpoint + "/tunnel"
				}
				client := Client{TLS: &tls.Config{RootCAs: roots}, Pool: &MuxPool{}}
				defer client.Pool.Close()
				c, err := client.DialRoute(context.Background(), transport, "tcp", target, contract.Tunnel{Endpoint: endpoint, ServerName: "localhost", Token: s.Token, Mux: mux})
				if err != nil {
					b.Fatal(err)
				}
				defer c.Close()
				payload := bytes.Repeat([]byte("x"), 32*1024)
				buf := make([]byte, len(payload))
				latencies := make([]int64, b.N)
				b.SetBytes(int64(len(payload)))
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					start := time.Now()
					c.SetDeadline(start.Add(10 * time.Second))
					if _, err := c.Write(payload); err != nil {
						b.Fatal(err)
					}
					if _, err := io.ReadFull(c, buf); err != nil {
						b.Fatal(err)
					}
					latencies[i] = time.Since(start).Nanoseconds()
				}
				b.StopTimer()
				if !bytes.Equal(buf, payload) {
					b.Fatal("payload mismatch")
				}
				slices.Sort(latencies)
				b.ReportMetric(float64(latencies[(b.N-1)*95/100])/1000, "p95-us")
			})
		}
	}
}
