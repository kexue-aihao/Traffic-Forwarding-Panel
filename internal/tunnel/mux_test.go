package tunnel

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

func TestMuxCarriesIndependentTCPStreams(t *testing.T) {
	pair, roots := testCertificate(t)
	target, _ := echoServers(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{TLS: &tls.Config{Certificates: []tls.Certificate{pair}}, Token: "mux-test-token-1234", Allowed: map[string]bool{"tcp|" + target: true}}
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener, "tls") }()
	t.Cleanup(func() { _ = server.Close(); <-done })
	client := Client{TLS: &tls.Config{RootCAs: roots}, Pool: &MuxPool{}, Timeout: 2 * time.Second}
	t.Cleanup(client.Pool.Close)
	tunnelSpec := contract.Tunnel{Endpoint: listener.Addr().String(), ServerName: "localhost", Token: server.Token, Mux: true}
	first, err := client.DialRoute(context.Background(), "tls", "tcp", target, tunnelSpec)
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.DialRoute(context.Background(), "tls", "tcp", target, tunnelSpec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { first.Close(); second.Close() })
	for _, pair := range []struct {
		c       *Session
		payload []byte
	}{{first, []byte("first")}, {second, bytes.Repeat([]byte("second"), 1000)}} {
		if _, err = pair.c.Write(pair.payload); err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadFull(pair.c, make([]byte, len(pair.payload)))
		if err != nil || got != len(pair.payload) {
			t.Fatalf("mux stream read: %v", err)
		}
	}
}
