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

func TestReverseCarrierRoutesAndReconnects(t *testing.T) {
	pair, roots := testCertificate(t)
	target, _ := echoServers(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{TLS: &tls.Config{Certificates: []tls.Certificate{pair}}, Token: "reverse-test-token", NodeID: "exit-reverse", Allowed: map[string]bool{"tcp|" + target: true, "reverse|exit-reverse": true}}
	// Listener and outbound exit have independent lifecycles, as in separate
	// Agent processes. Sharing one instance races their context/slot setup.
	exit := &Server{Token: server.Token, NodeID: "exit-reverse", Allowed: map[string]bool{"tcp|" + target: true}}
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener, "tls") }()
	ctx, cancel := context.WithCancel(context.Background())
	reverseDone := make(chan struct{})
	go func() {
		defer close(reverseDone)
		_ = exit.RunReverse(ctx, Client{TLS: &tls.Config{RootCAs: roots}}, "tls", listener.Addr().String(), "localhost", "exit-reverse")
	}()
	t.Cleanup(func() {
		cancel()
		_ = server.Close()
		for _, stopped := range []<-chan struct{}{done, reverseDone} {
			select {
			case <-stopped:
			case <-time.After(5 * time.Second):
				t.Error("reverse carrier did not stop")
			}
		}
	})
	client := Client{TLS: &tls.Config{RootCAs: roots}, Timeout: 3 * time.Second}
	spec := contract.Tunnel{Endpoint: listener.Addr().String(), ServerName: "localhost", Token: server.Token, Reverse: "exit-reverse"}
	roundTrip := func(payload string) {
		t.Helper()
		var session *Session
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			session, err = client.DialRoute(ctx, "tls", "tcp", target, spec)
			if err == nil {
				break
			}
			time.Sleep(25 * time.Millisecond)
		}
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		_ = session.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err = io.WriteString(session, payload); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, len(payload))
		if _, err = io.ReadFull(session, got); err != nil {
			t.Fatal(err)
		}
		if string(got) != payload {
			t.Fatalf("got %q", got)
		}
	}
	roundTrip("reverse")
	server.mu.Lock()
	carrier := server.reverse["exit-reverse"]
	server.mu.Unlock()
	if carrier == nil {
		t.Fatal("reverse carrier missing after successful transfer")
	}
	_ = carrier.Close()
	roundTrip("reconnected")
}
