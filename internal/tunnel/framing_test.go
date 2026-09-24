package tunnel

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func wireFrame(kind byte, payload []byte) []byte {
	wire := make([]byte, len(payload)+5)
	wire[0] = kind
	binary.BigEndian.PutUint32(wire[1:5], uint32(len(payload)))
	copy(wire[5:], payload)
	return wire
}

func TestStreamReadPreservesFrameBoundariesWithSmallBuffers(t *testing.T) {
	first := bytes.Repeat([]byte("a"), maxFrame)
	last := []byte("last")
	wire := append(wireFrame(dataFrame, first), wireFrame(dataFrame, nil)...)
	wire = append(wire, wireFrame(dataFrame, last)...)
	wire = append(wire, wireFrame(endFrame, nil)...)
	s := &Session{Conn: benchmarkConn{r: bytes.NewReader(wire)}}
	var out bytes.Buffer
	buf := make([]byte, 17)
	for {
		n, err := s.Read(buf)
		out.Write(buf[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(out.Bytes(), append(first, last...)) {
		t.Fatal("small reads lost or mixed frame bytes")
	}
	if _, err := s.Read(buf); err != io.EOF {
		t.Fatalf("close was not retained: %v", err)
	}
}

func TestStreamReadDoesNotWaitForRemainderOfLargeFrame(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	a.SetDeadline(time.Now().Add(2 * time.Second))
	s := &Session{Conn: a}
	release := make(chan struct{})
	defer close(release)
	go func() {
		wire := wireFrame(dataFrame, []byte("first-last"))
		b.Write(wire[:10])
		<-release
		b.Write(wire[10:])
	}()
	buf := make([]byte, 5)
	if _, err := io.ReadFull(s, buf); err != nil || string(buf) != "first" {
		t.Fatalf("partial frame read: %q, %v", buf, err)
	}
}

func TestMalformedStreamFramesRemainRejected(t *testing.T) {
	for _, tc := range []struct {
		name string
		wire []byte
	}{
		{"oversized", []byte{dataFrame, 0, 1, 0, 0}},
		{"truncated header", []byte{dataFrame, 0}},
		{"truncated payload", wireFrame(dataFrame, []byte("abcd"))[:7]},
		{"invalid close", wireFrame(endFrame, []byte("x"))},
		{"invalid type", wireFrame(99, nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Session{Conn: benchmarkConn{r: bytes.NewReader(tc.wire)}}
			_, err := s.Read(make([]byte, 16))
			if err == nil || err == io.EOF {
				t.Fatalf("malformed frame accepted: %v", err)
			}
			if n, next := s.Read(make([]byte, 16)); n != 0 || next != err {
				t.Fatalf("read continued after broken frame: %d, %v", n, next)
			}
		})
	}
}

type shortFrameWriter struct{ bytes.Buffer }

func (w *shortFrameWriter) Write(p []byte) (int, error) {
	return w.Buffer.Write(p[:min(3, len(p))])
}

func TestCombinedFrameHandlesShortWritesAndWireCompatibility(t *testing.T) {
	for _, payload := range [][]byte{nil, []byte("small"), bytes.Repeat([]byte("z"), maxFrame)} {
		var w shortFrameWriter
		if err := writeFrame(&w, dataFrame, payload); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(w.Bytes(), wireFrame(dataFrame, payload)) {
			t.Fatal("wire format changed on short write")
		}
	}
}

func TestWSSFrameUsesOneBinaryMessage(t *testing.T) {
	got := make(chan []byte, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up := websocket.Upgrader{}
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for range 2 {
			kind, p, err := c.ReadMessage()
			if err != nil || kind != websocket.BinaryMessage {
				return
			}
			got <- p
		}
	}))
	defer server.Close()
	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s := &Session{Conn: &wsConn{Conn: c}}
	payload := bytes.Repeat([]byte("x"), maxFrame)
	if _, err := s.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	for _, want := range [][]byte{wireFrame(dataFrame, payload), wireFrame(endFrame, nil)} {
		select {
		case p := <-got:
			if !bytes.Equal(p, want) {
				t.Fatalf("expected complete frame in a single message: got %d, want %d", len(p), len(want))
			}
		case <-time.After(2 * time.Second):
			t.Fatal("message missing")
		}
	}
}

func TestTLSResumptionStillAuthenticatesEveryCarrier(t *testing.T) {
	pair, roots := testCertificate(t)
	target, _ := echoServers(t)
	for _, transport := range []string{"tls", "ws", "wss", "http"} {
		t.Run(transport, func(t *testing.T) {
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			s := &Server{TLS: &tls.Config{Certificates: []tls.Certificate{pair}}, Token: "resumption-test-token", Allowed: map[string]bool{"tcp|" + target: true}}
			done := make(chan struct{})
			go func() { defer close(done); _ = s.Serve(l, transport) }()
			defer func() { s.Close(); <-done }()
			endpoint := l.Addr().String()
			if transport == "ws" || transport == "wss" {
				endpoint = transport + "://" + endpoint + "/tunnel"
			}
			states := make(chan tls.ConnectionState, 4)
			client := Client{Timeout: 2 * time.Second, TLS: &tls.Config{RootCAs: roots, ClientSessionCache: tls.NewLRUClientSessionCache(16), VerifyConnection: func(state tls.ConnectionState) error {
				states <- state
				return nil
			}}}
			for i := 0; i < 3; i++ {
				token := s.Token
				if i == 2 {
					token = "invalid-token"
				}
				c, err := client.Dial(context.Background(), transport, endpoint, "localhost", token, "tcp", target)
				if c != nil {
					c.Close()
				}
				if (err == nil) != (i != 2) {
					t.Fatalf("authorization on dial %d: %v", i, err)
				}
				var state tls.ConnectionState
				select {
				case state = <-states:
				case <-time.After(2 * time.Second):
					t.Fatalf("dial %d did not complete certificate verification", i)
				}
				if state.DidResume != (i > 0) || state.Version != tls.VersionTLS13 || len(state.VerifiedChains) == 0 {
					t.Fatalf("dial %d: resumed=%t, version=%x, verified=%d", i, state.DidResume, state.Version, len(state.VerifiedChains))
				}
			}
			// Verification callbacks are still enforced on resumed handshakes.
			rejected := errors.New("updated trust policy")
			client.TLS.VerifyConnection = func(tls.ConnectionState) error { return rejected }
			c, err := client.Dial(context.Background(), transport, endpoint, "localhost", s.Token, "tcp", target)
			if c != nil {
				c.Close()
			}
			if !errors.Is(err, rejected) {
				t.Fatalf("resumption bypassed verification callback: %v", err)
			}
		})
	}
}
