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
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener, "tls") }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = server.RunReverse(ctx, Client{TLS: &tls.Config{RootCAs: roots}}, "tls", listener.Addr().String(), "localhost", "exit-reverse")
	}()
	t.Cleanup(func() { _ = server.Close(); <-done })
	client := Client{TLS: &tls.Config{RootCAs: roots}, Timeout: 3 * time.Second}
	spec := contract.Tunnel{Endpoint: listener.Addr().String(), ServerName: "localhost", Token: server.Token, Reverse: "exit-reverse"}
	var session *Session
	for attempt := 0; attempt < 20; attempt++ {
		session, err = client.DialRoute(context.Background(), "tls", "tcp", target, spec)
		if err == nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	payload := []byte("reverse")
	if _, err = session.Write(payload); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(payload))
	if _, err = io.ReadFull(session, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("got %q", got)
	}
}
