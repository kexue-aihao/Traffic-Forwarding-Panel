package netx

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestIPv6PreferenceAndIPv4FallbackRealSockets(t *testing.T) {
	v6, e := net.Listen("tcp6", "[::1]:0")
	if e != nil {
		t.Skipf("IPv6 loopback unavailable: %v", e)
	}
	defer v6.Close()
	_, port, _ := net.SplitHostPort(v6.Addr().String())
	v4, e := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", port))
	if e != nil {
		t.Fatal(e)
	}
	defer v4.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	addresses, e := Addresses(ctx, net.JoinHostPort("localhost", port), true)
	if e != nil {
		t.Fatal(e)
	}
	h, _, _ := net.SplitHostPort(addresses[0])
	if net.ParseIP(h).To4() != nil {
		t.Fatal("resolver did not prefer IPv6")
	}
	c, e := Dial(ctx, "tcp", net.JoinHostPort("localhost", port), true)
	if e != nil {
		t.Fatal(e)
	}
	h, _, _ = net.SplitHostPort(c.RemoteAddr().String())
	if net.ParseIP(h).To4() != nil {
		t.Fatal("connected over IPv4 while IPv6 was available")
	}
	c.Close()
	v6.Close()
	c, e = Dial(ctx, "tcp", net.JoinHostPort("localhost", port), true)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	h, _, _ = net.SplitHostPort(c.RemoteAddr().String())
	if net.ParseIP(h).To4() == nil {
		t.Fatal("IPv4 fallback did not connect")
	}
}
