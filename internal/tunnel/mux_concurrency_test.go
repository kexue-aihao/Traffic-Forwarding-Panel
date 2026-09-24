package tunnel

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

func muxTestExit(t *testing.T, config *tls.Config, target string) contract.Tunnel {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{TLS: config, Token: "concurrent-mux-test-token", Allowed: map[string]bool{"tcp|" + target: true}}
	done := make(chan struct{})
	go func() { defer close(done); _ = s.Serve(l, "tls") }()
	t.Cleanup(func() { s.Close(); <-done })
	return contract.Tunnel{Endpoint: l.Addr().String(), ServerName: "localhost", Token: s.Token, Mux: true}
}

func TestMuxConcurrentDialsShareOneCarrier(t *testing.T) {
	pair, roots := testCertificate(t)
	target, _ := echoServers(t)
	var handshakes atomic.Int64
	spec := muxTestExit(t, &tls.Config{Certificates: []tls.Certificate{pair}, VerifyConnection: func(tls.ConnectionState) error {
		handshakes.Add(1)
		return nil
	}}, target)
	client := Client{TLS: &tls.Config{RootCAs: roots}, Pool: &MuxPool{}, Timeout: 3 * time.Second}
	defer client.Pool.Close()
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			c, err := client.DialRoute(context.Background(), "tls", "tcp", target, spec)
			if err != nil {
				t.Error(err)
				return
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(3 * time.Second))
			payload := bytes.Repeat([]byte(fmt.Sprintf("stream-%d", i)), 300)
			if _, err := c.Write(payload); err != nil {
				t.Error(err)
				return
			}
			if err := c.CloseWrite(); err != nil {
				t.Error(err)
				return
			}
			got, err := io.ReadAll(c)
			if err != nil || !bytes.Equal(got, payload) {
				t.Errorf("stream %d: mismatch or read error: %v", i, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := handshakes.Load(); got != 1 {
		t.Fatalf("same key dialled %d carriers", got)
	}
	spec.Token = "wrong-credential-test-token"
	if c, err := client.DialRoute(context.Background(), "tls", "tcp", target, spec); err == nil {
		c.Close()
		t.Fatal("different credentials reused authenticated carrier")
	}
	if handshakes.Load() != 2 {
		t.Fatal("different credential did not use a separate carrier")
	}
}

func TestMuxSlowDialIsolationCancellationAndClose(t *testing.T) {
	pair, roots := testCertificate(t)
	target, _ := echoServers(t)
	good := muxTestExit(t, &tls.Config{Certificates: []tls.Certificate{pair}}, target)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		if c, err := l.Accept(); err == nil {
			accepted <- c
			io.Copy(io.Discard, c) // Consume ClientHello, never finish TLS.
			c.Close()
		}
	}()
	client := Client{TLS: &tls.Config{RootCAs: roots}, Pool: &MuxPool{}, Timeout: 5 * time.Second}
	defer client.Pool.Close()
	bad := good
	bad.Endpoint = l.Addr().String()
	firstCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dial := func(ctx context.Context) <-chan error {
		done := make(chan error, 1)
		go func() {
			c, err := client.DialRoute(ctx, "tls", "tcp", target, bad)
			if c != nil {
				c.Close()
			}
			done <- err
		}()
		return done
	}
	first := dial(firstCtx)
	select {
	case conn := <-accepted:
		defer conn.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("slow dial did not start")
	}
	client.Pool.mu.Lock()
	var pending *muxEntry
	for _, entry := range client.Pool.sessions {
		pending = entry
	}
	client.Pool.mu.Unlock()
	second := dial(context.Background())
	cancel()
	select {
	case err := <-first:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled waiter: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled waiter stuck behind dial")
	}
	select {
	case <-pending.ready:
		t.Fatal("one cancelled waiter aborted the shared dial")
	default:
	}
	ctx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	c, err := client.DialRoute(ctx, "tls", "tcp", target, good)
	if err != nil {
		t.Fatalf("unrelated healthy endpoint blocked by slow dial: %v", err)
	}
	c.Close()
	client.Pool.Close()
	select {
	case err := <-second:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("pool close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pool close left a blocked waiter")
	}
	select {
	case <-pending.ready:
	case <-time.After(time.Second):
		t.Fatal("pool close did not cancel pending handshake")
	}
	client.Pool.mu.Lock()
	defer client.Pool.mu.Unlock()
	if len(client.Pool.sessions) != 0 {
		t.Fatal("dial repopulated the closed pool")
	}
}

func TestMuxBlockedStreamOpenHonorsCallerDeadline(t *testing.T) {
	a, b := net.Pipe()
	config := muxConfig()
	config.AcceptBacklog = 1
	clientSession, err := yamux.Client(a, config)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()
	serverSession, err := yamux.Server(b, config)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	// Without AcceptStream, the first SYN remains unacknowledged and fills
	// yamux's one-entry SYN queue. The next OpenStream really blocks.
	first, err := clientSession.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	entry := &muxEntry{session: clientSession, ready: make(chan struct{}), cancel: func() {}}
	close(entry.ready)
	spec := contract.Tunnel{Endpoint: "127.0.0.1:1234", ServerName: "localhost", Token: "blocked-stream-test-token", Mux: true}
	raw, _ := json.Marshal([]string{"tls", spec.Endpoint, spec.ServerName, spec.Token})
	pool := &MuxPool{sessions: map[string]*muxEntry{string(raw): entry}, done: make(chan struct{})}
	defer pool.Close()
	client := Client{Pool: pool}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := client.DialRoute(ctx, "tls", "tcp", "127.0.0.1:80", spec)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("blocked stream: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("OpenStream ignored caller deadline")
	}
}
