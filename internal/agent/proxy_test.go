package agent

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

func TestProxyProtocolRelayAndMeter(t *testing.T) {
	for _, version := range []string{"v1", "v2"} {
		t.Run(version, func(t *testing.T) {
			store, runtime, cfg := setup(t)
			target, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer target.Close()
			source := make(chan string, 1)
			go func() {
				c, e := target.Accept()
				if e != nil {
					return
				}
				defer c.Close()
				c.SetDeadline(time.Now().Add(3 * time.Second))
				p, e := receiveProxy(c, &contract.ProxyProtocol{Accept: version, TrustedCIDRs: []string{"127.0.0.0/8"}})
				if e != nil {
					source <- e.Error()
					return
				}
				source <- p.RemoteAddr().String()
				io.Copy(p, p)
			}()
			rule := testRule(target.Addr().String())
			rule.ProxyProtocol = &contract.ProxyProtocol{Accept: version, Send: version, TrustedCIDRs: []string{"127.0.0.0/8"}}
			cfg.Rules = []contract.Rule{rule}
			if err = runtime.Apply(cfg, true); err != nil {
				t.Fatal(err)
			}
			conn, err := net.Dial("tcp", runtime.listeners[key(rule)].tcp.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(3 * time.Second))
			identity := &proxyConn{Conn: conn, source: &net.TCPAddr{IP: net.ParseIP("203.0.113.8"), Port: 1234}, destination: &net.TCPAddr{IP: net.ParseIP("198.51.100.9"), Port: 443}}
			if err = sendProxy(conn, identity, rule.ProxyProtocol); err != nil {
				t.Fatal(err)
			}
			conn.Write([]byte("ping"))
			conn.(*net.TCPConn).CloseWrite()
			body, err := io.ReadAll(conn)
			if err != nil || string(body) != "ping" {
				t.Fatal(string(body), err)
			}
			if got := <-source; got != "203.0.113.8:1234" {
				t.Fatal(got)
			}
			var total int64
			for _, u := range store.Pending() {
				total += u.UploadBytes + u.DownloadBytes
			}
			if total != 8 {
				t.Fatalf("proxy headers counted as business payload: %d", total)
			}
		})
	}
}
func TestProxyProtocolRejectsUntrustedAndUDP(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	if _, err := receiveProxy(a, &contract.ProxyProtocol{Accept: "v1", TrustedCIDRs: []string{"192.0.2.0/24"}}); err == nil {
		t.Fatal("untrusted peer accepted")
	}
	r := testRule("127.0.0.1:1")
	r.Network = "udp"
	r.ProxyProtocol = &contract.ProxyProtocol{Send: "v2"}
	if validate(r) == nil {
		t.Fatal("UDP silently accepted")
	}
}
func TestDiagnosticChecksRealTargetAndStaleConfiguration(t *testing.T) {
	store, runtime, cfg := setup(t)
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	go func() {
		c, e := target.Accept()
		if e == nil {
			c.Close()
		}
	}()
	r := testRule(target.Addr().String())
	r.Version = 1
	cfg.Rules = []contract.Rule{r}
	if err = runtime.Apply(cfg, true); err != nil {
		t.Fatal(err)
	}
	a := Agent{Store: store, Runtime: runtime}
	checks := a.diagnosticChecks(context.Background(), contract.Diagnostic{RuleID: r.ID, RuleVersion: 1})
	if len(checks) != 3 || !checks[2].OK {
		t.Fatal(checks)
	}
	checks = a.diagnosticChecks(context.Background(), contract.Diagnostic{RuleID: r.ID, RuleVersion: 2})
	if len(checks) != 1 || checks[0].OK {
		t.Fatal(checks)
	}
}
