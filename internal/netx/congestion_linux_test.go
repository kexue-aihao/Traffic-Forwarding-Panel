//go:build linux

package netx

import (
	"context"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func socketCongestion(t *testing.T, socket interface {
	SyscallConn() (syscall.RawConn, error)
}) string {
	t.Helper()
	raw, err := socket.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var algorithm string
	var optionErr error
	if err := raw.Control(func(fd uintptr) {
		algorithm, optionErr = unix.GetsockoptString(int(fd), unix.IPPROTO_TCP, unix.TCP_CONGESTION)
	}); err != nil {
		t.Fatal(err)
	}
	if optionErr != nil {
		t.Fatal(optionErr)
	}
	return algorithm
}

// Verify real client, listener and accepted sockets without changing sysctl or
// requiring BBR to be installed. A BBR-configured runner must report "bbr".
func TestTCPInheritsSystemCongestionControl(t *testing.T) {
	data, err := os.ReadFile("/proc/sys/net/ipv4/tcp_congestion_control")
	if err != nil {
		t.Skipf("kernel TCP congestion setting unavailable: %v", err)
	}
	want := strings.TrimSpace(string(data))
	if want == "" {
		t.Fatal("empty kernel TCP congestion setting")
	}
	for _, network := range []string{"tcp4", "tcp6"} {
		for _, prefer := range []bool{false, true} {
			name := network + "/default"
			if prefer {
				name = network + "/prefer-ipv6"
			}
			t.Run(name, func(t *testing.T) {
				address := "127.0.0.1:0"
				if network == "tcp6" {
					address = "[::1]:0"
				}
				listener, err := net.Listen(network, address)
				if err != nil {
					if network == "tcp6" {
						t.Skipf("IPv6 loopback unavailable: %v", err)
					}
					t.Fatal(err)
				}
				defer listener.Close()
				tcpListener := listener.(*net.TCPListener)
				if got := socketCongestion(t, tcpListener); got != want {
					t.Fatalf("listener congestion = %q, system default = %q", got, want)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				client, err := Dial(ctx, "tcp", listener.Addr().String(), prefer)
				if err != nil {
					t.Fatal(err)
				}
				defer client.Close()
				if got := socketCongestion(t, client.(*net.TCPConn)); got != want {
					t.Fatalf("outbound congestion = %q, system default = %q", got, want)
				}
				if err := tcpListener.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
					t.Fatal(err)
				}
				accepted, err := tcpListener.AcceptTCP()
				if err != nil {
					t.Fatal(err)
				}
				defer accepted.Close()
				if got := socketCongestion(t, accepted); got != want {
					t.Fatalf("inbound congestion = %q, system default = %q", got, want)
				}
			})
		}
	}
}
