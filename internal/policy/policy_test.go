package policy

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"golang.org/x/net/http2/hpack"
)

func TestHostBoundaries(t *testing.T) {
	for _, v := range []struct{ raw, want string }{{"EXAMPLE.com.:443", "example.com"}, {"bücher.de", "xn--bcher-kva.de"}, {"[::1]:443", "::1"}} {
		got, e := Host(v.raw)
		if e != nil || got != v.want {
			t.Fatalf("%s: %s %v", v.raw, got, e)
		}
	}
	for _, bad := range []string{"example.com:70000", "[example.com]", "a..b", "a:invalid", "x:0"} {
		if _, e := Host(bad); e == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	for _, h := range []string{"example.com", "evilexample.com", "example.com.evil"} {
		if MatchHost("*.example.com", h) {
			t.Fatal(h)
		}
	}
	if !MatchHost("*.example.com", "a.b.example.com") {
		t.Fatal("wildcard failed")
	}
	if CheckHost([]contract.InboundPolicy{{AllowedHosts: []string{"*.example.com"}}, {AllowedHosts: []string{"a.example.com"}}}, "b.example.com") == nil {
		t.Fatal("allowlists were unioned")
	}
}
func TestHTTPStreamBoundaries(t *testing.T) {
	layers := []contract.InboundPolicy{{BlockedPaths: []string{"/admin*"}}}
	first := "POST /ok HTTP/1.1\r\nHost: example.com\r\nContent-Length: 3\r\n\r\nabc"
	chunked := "POST /ok HTTP/1.1\r\nHost: example.com\r\nTransfer-Encoding: chunked\r\n\r\n3\r\nabc\r\n0\r\nX-Foo: bar\r\n\r\n"
	for _, allowed := range []string{first, chunked} {
		for _, path := range []string{"/admin", "/%61dmin", "/a/../admin", "/%2561dmin"} {
			r := newHTTPReader(strings.NewReader(allowed+"GET "+path+" HTTP/1.1\r\nHost: example.com\r\n\r\n"), layers)
			got, e := io.ReadAll(r)
			if e == nil || string(got) != allowed {
				t.Fatalf("rejected request leaked: %q %v", got, e)
			}
		}
	}
	for _, bad := range []string{"GET /ok HTTP/1.1\r\nHost: a\r\nHost: b\r\n\r\n", "POST /ok HTTP/1.1\r\nHost: a\r\nContent-Length: 3\r\nTransfer-Encoding: chunked\r\n\r\n", "GET http://evil/ok HTTP/1.1\r\nHost: a\r\n\r\n", "CONNECT a:443 HTTP/1.1\r\nHost: a\r\n\r\n"} {
		got, e := io.ReadAll(newHTTPReader(strings.NewReader(bad), layers))
		if e == nil || len(got) > 0 {
			t.Fatalf("ambiguous request passed %q", got)
		}
	}
}

func TestHTTPFramingRejectsControlCharacters(t *testing.T) {
	for _, ext := range []string{";signature=abc123", "; foo = \"a;b\\\"c\"; bar"} {
		if !validChunkExtensions(ext) {
			t.Fatalf("valid chunk extension rejected %q", ext)
		}
	}
	for _, ext := range []string{";", ";=a", ";foo=", ";foo=\"unterminated", ";foo=\"a\r\"", ";foo=bar extra", ";foo=(bar)"} {
		if validChunkExtensions(ext) {
			t.Fatalf("ambiguous chunk extension accepted %q", ext)
		}
	}
	for _, wire := range []string{
		"GET / HTTP/1.1\r\nHost: example.com\r\nX-Test: bad\x01value\r\n\r\n",
		"POST / HTTP/1.1\r\nHost: example.com\r\nTransfer-Encoding: chunked\r\n\r\n0;ext=\"bad\r\"\r\n\r\n",
		"POST / HTTP/1.1\r\nHost: example.com\r\nTransfer-Encoding: chunked\r\n\r\n0\r\nX-Test: bad\x01value\r\n\r\n",
	} {
		if _, err := io.ReadAll(newHTTPReader(strings.NewReader(wire), nil)); err == nil {
			t.Fatalf("malformed framing accepted %q", wire)
		}
	}
}

func TestH2DataAndEmptyHeadersCannotBypassAuthorityPolicy(t *testing.T) {
	for _, kind := range []byte{0, 1} {
		frame := make([]byte, 9)
		frame[3], frame[4] = kind, 4
		binary.BigEndian.PutUint32(frame[5:], 1)
		wire := append([]byte(H2Preface), frame...)
		got, err := io.ReadAll(newH2Reader(bytes.NewReader(wire), []contract.InboundPolicy{{AllowedHosts: []string{"example.com"}}}))
		if err == nil || string(got) != H2Preface {
			t.Fatalf("unauthorized initial h2 frame passed: %x %v", got, err)
		}
	}
}
func TestFragmentedInspectionRealSocket(t *testing.T) {
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	done := make(chan Inspection, 1)
	go func() {
		c, e := l.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		v, e := Inspect(c)
		if e != nil {
			t.Error(e)
			return
		}
		done <- v
		got, e := io.ReadAll(v.Conn)
		if e != nil || string(got) != "GET /ok HTTP/1.1\r\nHost: Example.com\r\n\r\n" {
			t.Errorf("replay %q %v", got, e)
		}
	}()
	c, e := net.Dial("tcp", l.Addr().String())
	if e != nil {
		t.Fatal(e)
	}
	for _, b := range []byte("GET /ok HTTP/1.1\r\nHost: Example.com\r\n\r\n") {
		c.Write([]byte{b})
	}
	c.(*net.TCPConn).CloseWrite()
	defer c.Close()
	select {
	case v := <-done:
		if v.Kind != "http" || v.Check([]contract.InboundPolicy{{BlockedApps: []string{"http"}}}) == nil {
			t.Fatal("HTTP bypass")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("inspection stalled")
	}
}
func TestH2LaterStreamCannotBypass(t *testing.T) {
	var encoded bytes.Buffer
	enc := hpack.NewEncoder(&encoded)
	frame := func(id uint32, path string) []byte {
		encoded.Reset()
		for _, f := range []hpack.HeaderField{{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "http"}, {Name: ":authority", Value: "example.com"}, {Name: ":path", Value: path}} {
			if e := enc.WriteField(f); e != nil {
				t.Fatal(e)
			}
		}
		b := make([]byte, 9)
		n := encoded.Len()
		b[0], b[1], b[2] = byte(n>>16), byte(n>>8), byte(n)
		b[3], b[4] = 1, 4
		binary.BigEndian.PutUint32(b[5:], id)
		return append(b, encoded.Bytes()...)
	}
	allowed := append([]byte(H2Preface), frame(1, "/ok")...)
	wire := append(bytes.Clone(allowed), frame(3, "/admin")...)
	got, e := io.ReadAll(newH2Reader(bytes.NewReader(wire), []contract.InboundPolicy{{BlockedPaths: []string{"/admin"}}}))
	if e == nil || !bytes.Equal(got, allowed) {
		t.Fatalf("h2 denied header leaked: %x %v", got, e)
	}
}
