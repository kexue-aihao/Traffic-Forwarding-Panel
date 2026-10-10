package integration

import (
	"bytes"
	"crypto/tls"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/tunnel"
)

func setAdvanced(t *testing.T, f *fixture, a *contract.GroupAdvanced) {
	t.Helper()
	if a != nil {
		a.PolicyVersion = contract.GroupPolicyVersion
	}
	if a == nil {
		f.group.BlockedProtocols = nil
	}
	f.group.Advanced = a
	f.group = decode[contract.Group](t, f.request("PUT", "/groups/"+f.group.ID, f.group, f.admin, 200))
	f.sync()
}
func denied(t *testing.T, r contract.Rule, p []byte) {
	t.Helper()
	c, e := net.DialTimeout(r.Network, r.Listen, time.Second)
	if e != nil {
		return
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(400 * time.Millisecond))
	c.Write(p)
	if r.Network == "tcp" {
		c.(*net.TCPConn).CloseWrite()
	}
	var b [4096]byte
	if n, _ := c.Read(b[:]); n > 0 {
		t.Fatalf("denied bytes echoed: %q", b[:n])
	}
}
func tlsEcho(t *testing.T, pair tls.Certificate) string {
	t.Helper()
	l, e := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{pair}})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, e := l.Accept()
			if e != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	return l.Addr().String()
}
func tlsTransfer(t *testing.T, r contract.Rule, name string, want bool) {
	t.Helper()
	c, e := tls.DialWithDialer(&net.Dialer{Timeout: 2 * time.Second}, "tcp", r.Listen, &tls.Config{InsecureSkipVerify: true, ServerName: name})
	if !want {
		if e == nil {
			c.Close()
			t.Fatal("denied TLS reached origin")
		}
		return
	}
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	p := []byte("TLS policy replay")
	c.Write(p)
	got := make([]byte, len(p))
	if _, e = io.ReadFull(c, got); e != nil || !bytes.Equal(p, got) {
		t.Fatalf("TLS echo: %q %v", got, e)
	}
}

func TestGroupAdvancedRealForwarding(t *testing.T) {
	for _, carrier := range []string{"direct", "tls"} {
		t.Run(carrier, func(t *testing.T) {
			pair, roots := certificate(t)
			f := newFixture(t, tunnel.Client{TLS: &tls.Config{RootCAs: roots}})
			target, udp := targets(t)
			var tunnelConfig *contract.Tunnel
			if carrier == "tls" {
				l, e := net.Listen("tcp", "127.0.0.1:0")
				if e != nil {
					t.Fatal(e)
				}
				s := &tunnel.Server{TLS: &tls.Config{Certificates: []tls.Certificate{pair}}, Token: "advanced-policy-carrier-token"}
				done := make(chan struct{})
				go func() { defer close(done); s.Serve(l, "tls") }()
				t.Cleanup(func() { s.Close(); <-done })
				tunnelConfig = &contract.Tunnel{Endpoint: l.Addr().String(), ServerName: "localhost", Token: s.Token}
			}
			r := f.rule("tcp", carrier, target, tunnelConfig)
			u := f.rule("udp", carrier, udp, tunnelConfig)
			tr := f.rule("tcp", carrier, tlsEcho(t, pair), tunnelConfig)
			f.sync()
			for _, tc := range []struct {
				name      string
				a         *contract.GroupAdvanced
				bad, good string
			}{
				{"allowed_host", &contract.GroupAdvanced{AllowedHost: []string{"*.allowed.example"}}, "GET / HTTP/1.1\r\nHost: evilexample.com\r\n\r\n", "GET / HTTP/1.1\r\nHost: A.ALLOWED.example.:80\r\n\r\n"},
				{"blocked_host", &contract.GroupAdvanced{BlockedHost: []string{"blocked.example"}}, "GET / HTTP/1.1\r\nHost: BLOCKED.example.:80\r\n\r\n", "GET / HTTP/1.1\r\nHost: permitted.example\r\n\r\n"},
				{"blocked_path", &contract.GroupAdvanced{BlockedPath: []string{"/private*"}}, "GET /%70rivate HTTP/1.1\r\nHost: example.com\r\n\r\n", "GET /public HTTP/1.1\r\nHost: example.com\r\n\r\n"},
				{"blocked_protocol_http", &contract.GroupAdvanced{BlockedProtocol: []string{"http"}}, "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n", "other-business"},
				{"blocked_protocol_socks", &contract.GroupAdvanced{BlockedProtocol: []string{"socks"}}, string([]byte{5, 1, 0}), "other-business"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					setAdvanced(t, f, tc.a)
					denied(t, r, []byte(tc.bad))
					transfer(t, r, []byte(tc.good))
					setAdvanced(t, f, nil)
					transfer(t, r, []byte(tc.bad))
				})
			}
			t.Run("tls_inbound_policy", func(t *testing.T) {
				setAdvanced(t, f, &contract.GroupAdvanced{TLSInboundPolicy: 1})
				denied(t, r, []byte("cleartext denied"))
				denied(t, u, []byte("udp denied"))
				tlsTransfer(t, tr, "localhost", true)
				setAdvanced(t, f, &contract.GroupAdvanced{TLSInboundPolicy: 2})
				denied(t, tr, nil)
				if len(f.store.Config().BlockedRules) == 0 {
					t.Fatal("missing admin port diagnostic")
				}
				setAdvanced(t, f, nil)
			})
			t.Run("empty_sni", func(t *testing.T) {
				setAdvanced(t, f, &contract.GroupAdvanced{TLSRejectEmptySNI: true})
				tlsTransfer(t, tr, "", false)
				tlsTransfer(t, tr, "localhost", true)
				transfer(t, r, []byte("cleartext allowed"))
				setAdvanced(t, f, nil)
				tlsTransfer(t, tr, "", true)
			})
			t.Run("disable_udp", func(t *testing.T) {
				setAdvanced(t, f, &contract.GroupAdvanced{DisableUDP: true})
				denied(t, u, []byte("blocked UDP"))
				transfer(t, r, []byte("TCP allowed"))
				setAdvanced(t, f, nil)
				transfer(t, u, []byte("UDP restored"))
			})
		})
	}
}

func TestGroupPolicyHotUpdateClosesExistingHTTP(t *testing.T) {
	f := newFixture(t, tunnel.Client{})
	target, _ := targets(t)
	r := f.rule("tcp", "direct", target, nil)
	setAdvanced(t, f, &contract.GroupAdvanced{BlockedPath: []string{"/private"}})
	c, e := net.Dial("tcp", r.Listen)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	first := []byte("GET /public HTTP/1.1\r\nHost: example.com\r\n\r\n")
	c.Write(first)
	got := make([]byte, len(first))
	if _, e = io.ReadFull(c, got); e != nil {
		t.Fatal(e)
	}
	// A denied later request must not enter the echo origin or consume quota.
	c.Write([]byte("GET /private HTTP/1.1\r\nHost: example.com\r\n\r\n"))
	var b [1]byte
	if n, _ := c.Read(b[:]); n != 0 {
		t.Fatal("later request bypassed policy")
	}
	statuses := f.agent.Runtime.PolicyStatuses()
	if len(statuses) != 1 || statuses[0].RuleID != r.ID || statuses[0].Rejected != 1 {
		t.Fatalf("later request rejection not reported: %+v", statuses)
	}
	setAdvanced(t, f, &contract.GroupAdvanced{BlockedHost: []string{"example.com"}})
	denied(t, r, first)
}

func TestManagedExitIndependentInspectionBeforeOrigin(t *testing.T) {
	pair, roots := certificate(t)
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	var accepted atomic.Int64
	go func() {
		for {
			c, e := l.Accept()
			if e != nil {
				return
			}
			accepted.Add(1)
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	s := &tunnel.Server{TLS: &tls.Config{Certificates: []tls.Certificate{pair}}, Token: "inspection-stage-test-token", NodeID: "inspection-exit", Policy: &contract.EffectivePolicy{Version: 2, InboundLayers: []contract.InboundPolicy{{BlockedHosts: []string{"blocked.example"}}}}}
	done := make(chan struct{})
	go func() { defer close(done); s.Serve(listener, "tls") }()
	t.Cleanup(func() { s.Close(); <-done })
	client := tunnel.Client{TLS: &tls.Config{RootCAs: roots}, InspectionPrefix: []byte("GET / HTTP/1.1\r\nHost: blocked.example\r\n\r\n")}
	spec := contract.Tunnel{Endpoint: listener.Addr().String(), ServerName: "localhost", Token: s.Token, Inspect: true}
	if c, e := client.DialRoute(t.Context(), "tls", "tcp", l.Addr().String(), spec); e == nil {
		c.Close()
		t.Fatal("exit accepted denied host")
	}
	if accepted.Load() != 0 {
		t.Fatal("denied request dialed business origin")
	}
	client.InspectionPrefix = []byte("GET / HTTP/1.1\r\nHost: good.example\r\n\r\n")
	c, e := client.DialRoute(t.Context(), "tls", "tcp", l.Addr().String(), spec)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	c.Write(client.InspectionPrefix)
	got := make([]byte, len(client.InspectionPrefix))
	if _, e = io.ReadFull(c, got); e != nil || !bytes.Equal(got, client.InspectionPrefix) {
		t.Fatalf("metadata replay %q %v", got, e)
	}
	if accepted.Load() != 1 {
		t.Fatal("inspection copied business twice")
	}
}
