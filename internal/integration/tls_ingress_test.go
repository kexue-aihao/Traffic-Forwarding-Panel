package integration

import (
	"bytes"
	"crypto/tls"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/tunnel"
)

func TestGroupTLSIngressRealControlPlane(t *testing.T) {
	for _, carrier := range []string{"direct", "tls"} {
		t.Run(carrier, func(t *testing.T) {
			pair, roots := certificate(t)
			f := newFixture(t, tunnel.Client{TLS: &tls.Config{RootCAs: roots}})
			_, p, _ := net.SplitHostPort(freeAddress(t, "tcp"))
			port, _ := strconv.Atoi(p)
			advanced := &contract.GroupAdvanced{TLSInboundPolicy: 2, SharedTLSIngress: &contract.SharedTLSIngressSettings{Enabled: true, ListenIP: "127.0.0.1", Port: port}}
			setAdvanced(t, f, advanced)
			var params *contract.Tunnel
			if carrier == "tls" {
				l, e := net.Listen("tcp", "127.0.0.1:0")
				if e != nil {
					t.Fatal(e)
				}
				s := &tunnel.Server{TLS: &tls.Config{Certificates: []tls.Certificate{pair}}, Token: "shared-real-control-plane-exit"}
				done := make(chan struct{})
				go func() { defer close(done); s.Serve(l, "tls") }()
				t.Cleanup(func() { s.Close(); <-done })
				params = &contract.Tunnel{Endpoint: l.Addr().String(), ServerName: "localhost", Token: s.Token}
			}
			rule := decode[contract.Rule](t, f.request("POST", "/rules", contract.Rule{Name: "shared", NodeID: f.store.Identity().NodeID, GroupID: f.group.ID, Network: "tcp", Transport: carrier, Target: tlsEcho(t, pair), Enabled: true, Tunnel: params, SharedTLS: &contract.SharedTLS{IngressID: "group", ServerName: "localhost"}}, f.user, 201))
			f.sync()
			cfg := f.store.Config()
			if len(cfg.TLSIngresses) != 1 || len(cfg.Rules) != 1 || cfg.Rules[0].EffectivePolicy == nil || !cfg.Rules[0].EffectivePolicy.InboundLayers[0].TLSRequired {
				t.Fatal("missing compiled shared ingress/TLS policy", cfg)
			}
			dial := func() net.Conn {
				t.Helper()
				c, e := tls.DialWithDialer(&net.Dialer{Timeout: 2 * time.Second}, "tcp", rule.Listen, &tls.Config{RootCAs: roots, ServerName: "localhost"})
				if e != nil {
					t.Fatal(e)
				}
				c.SetDeadline(time.Now().Add(3 * time.Second))
				t.Cleanup(func() { c.Close() })
				return c
			}
			exchange := func(c net.Conn) error {
				data := []byte("real compiled shared TLS")
				if _, e := c.Write(data); e != nil {
					return e
				}
				received := make([]byte, len(data))
				if _, e := io.ReadFull(c, received); e != nil {
					return e
				}
				if !bytes.Equal(data, received) {
					t.Fatal("altered business data")
				}
				return nil
			}
			c := dial()
			if e := exchange(c); e != nil {
				t.Fatal(e)
			}
			f.sync()
			if e := exchange(c); e != nil {
				t.Fatal("normal control-plane refresh interrupted connection", e)
			}
			advanced.BlockedHost = []string{"localhost"}
			setAdvanced(t, f, advanced)
			if exchange(c) == nil {
				t.Fatal("compiled host restriction left connection open")
			}
			tlsTransfer(t, rule, "localhost", false)
			advanced.BlockedHost = nil
			setAdvanced(t, f, advanced)
			if e := exchange(dial()); e != nil {
				t.Fatal(e)
			}
			f.sync()
			nodes := decode[struct {
				Items []contract.Node `json:"items"`
			}](t, f.request("GET", "/nodes", nil, f.admin, 200))
			if len(nodes.Items) != 1 || len(nodes.Items[0].TLSIngressStatuses) != 1 || nodes.Items[0].TLSIngressStatuses[0].Routes != 1 {
				t.Fatal("missing real Agent ingress report", nodes)
			}
			advanced.SharedTLSIngress.Enabled = false
			setAdvanced(t, f, advanced)
			if len(f.store.Config().TLSIngresses) != 0 || len(f.store.Config().Rules) != 0 {
				t.Fatal("disabled ingress still active")
			}
			var ports int
			if e := f.db.DB.QueryRow("SELECT COUNT(*) FROM cp_ingress_ports").Scan(&ports); e != nil || ports != 0 {
				t.Fatal("real successful ACK did not release ingress port", ports, e)
			}
		})
	}
}
