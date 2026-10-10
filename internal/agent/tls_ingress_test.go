package agent

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"math/big"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/policy/detect"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/tunnel"
)

func ingressTestCertificate(t testing.TB) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	k, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	c := &x509.Certificate{SerialNumber: big.NewInt(55), DNSNames: []string{"a.example.com", "b.example.com", "c.example.com"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	for i := 0; i < 64; i++ {
		c.DNSNames = append(c.DNSNames, fmt.Sprintf("route%d.example.com", i))
	}
	der, e := x509.CreateCertificate(rand.Reader, c, c, &k.PublicKey, k)
	if e != nil {
		t.Fatal(e)
	}
	cert, e := x509.ParseCertificate(der)
	if e != nil {
		t.Fatal(e)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k}, roots
}

func TestTLSIngressAddressChangeRebindAndRollback(t *testing.T) {
	_, r, cfg := setup(t)
	pair, roots := ingressTestCertificate(t)
	address := ingressTestAddress(t)
	_, port, _ := net.SplitHostPort(address)
	ingress := contract.SharedTLSIngress{ID: "shared", NodeID: "node", GroupID: "group", Listen: net.JoinHostPort("0.0.0.0", port)}
	v := testRule(ingressTestOrigin(t, pair, 'a'))
	v.GroupID, v.UserID, v.Listen = "group", "alice", ingress.Listen
	v.SharedTLS = &contract.SharedTLS{IngressID: "shared", ServerName: "a.example.com"}
	cfg.TLSIngresses, cfg.Rules = []contract.SharedTLSIngress{ingress}, []contract.Rule{v}
	if e := r.Apply(cfg, true); e != nil {
		t.Fatal(e)
	}
	dial := func() net.Conn {
		t.Helper()
		c, e := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", address, &tls.Config{RootCAs: roots, ServerName: "a.example.com"})
		if e != nil {
			t.Fatal(e)
		}
		c.SetDeadline(time.Now().Add(5 * time.Second))
		if _, e = io.ReadFull(c, make([]byte, 1)); e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	c := dial()
	// Invalid replacement must restore the wildcard listener after staging.
	bad := cfg
	bad.Version++
	bad.TLSIngresses = []contract.SharedTLSIngress{ingress}
	bad.TLSIngresses[0].Listen = address
	broken := v
	broken.Listen, broken.Target = address, "invalid"
	bad.Rules = []contract.Rule{broken}
	if e := r.Apply(bad, true); e == nil {
		t.Fatal("invalid replacement accepted")
	}
	restored := dial()
	if e := exchange(restored, "restored"); e != nil {
		t.Fatal(e)
	}
	cfg.Version++
	cfg.TLSIngresses[0].Listen = address
	cfg.Rules[0].Listen = address
	if e := r.Apply(cfg, true); e != nil {
		t.Fatal("same-port IP change could not rebind", e)
	}
	if exchange(c, "old") == nil || exchange(restored, "previous-socket") == nil {
		t.Fatal("address change retained old socket connections")
	}
	fresh := dial()
	if e := exchange(fresh, "new-socket"); e != nil {
		t.Fatal(e)
	}
}

func TestTLSIngressConcurrentRoutes(t *testing.T) {
	_, r, cfg := setup(t)
	pair, roots := ingressTestCertificate(t)
	ingress := contract.SharedTLSIngress{ID: "shared", NodeID: "node", GroupID: "group", Listen: ingressTestAddress(t)}
	targets := []string{ingressTestOrigin(t, pair, 'a'), ingressTestOrigin(t, pair, 'b')}
	for i := 0; i < 64; i++ {
		v := testRule(targets[i%2])
		v.ID, v.GroupID, v.UserID, v.Listen = fmt.Sprintf("route%d", i), "group", fmt.Sprintf("owner%d", i), ingress.Listen
		lease := *v.Lease
		lease.ID = fmt.Sprintf("lease%d", i)
		v.Lease = &lease
		v.SharedTLS = &contract.SharedTLS{IngressID: "shared", ServerName: fmt.Sprintf("route%d.example.com", i)}
		cfg.Rules = append(cfg.Rules, v)
	}
	cfg.TLSIngresses = []contract.SharedTLSIngress{ingress}
	if e := r.Apply(cfg, true); e != nil {
		t.Fatal(e)
	}
	type connected struct {
		index int
		conn  net.Conn
		err   error
	}
	ready := make(chan connected, 64)
	for i := 0; i < 64; i++ {
		go func(i int) {
			c, e := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", ingress.Listen, &tls.Config{RootCAs: roots, ServerName: fmt.Sprintf("route%d.example.com", i)})
			if e == nil {
				c.SetDeadline(time.Now().Add(10 * time.Second))
				tag := make([]byte, 1)
				_, e = io.ReadFull(c, tag)
				if e == nil && tag[0] != byte('a'+i%2) {
					e = fmt.Errorf("route %d reached wrong backend", i)
				}
			}
			ready <- connected{i, c, e}
		}(i)
	}
	clients := make([]net.Conn, 64)
	for i := 0; i < 64; i++ {
		v := <-ready
		if v.conn != nil {
			t.Cleanup(func() { v.conn.Close() })
		}
		if v.err != nil {
			t.Error(v.err)
		}
		clients[v.index] = v.conn
	}
	if t.Failed() {
		return
	}
	cfg.Version++
	cfg.Rules = cfg.Rules[1:]
	if e := r.Apply(cfg, true); e != nil {
		t.Fatal(e)
	}
	if exchange(clients[0], "removed") == nil {
		t.Fatal("removed route retained connection")
	}
	for i, c := range clients[1:] {
		if e := exchange(c, fmt.Sprintf("route%d", i+1)); e != nil {
			t.Fatal("concurrent route was interrupted", i+1, e)
		}
	}
}
func ingressTestAddress(t testing.TB) string {
	t.Helper()
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	a := l.Addr().String()
	l.Close()
	return a
}
func ingressTestOrigin(t testing.TB, pair tls.Certificate, tag byte) string {
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
			go func() {
				defer c.Close()
				c.SetDeadline(time.Now().Add(15 * time.Second))
				if _, e := c.Write([]byte{tag}); e == nil {
					io.Copy(c, c)
				}
			}()
		}
	}()
	return l.Addr().String()
}

// Loopback comparisons include TLS and the production quota/accounting path.
// They are regression measurements, not WAN throughput or latency promises.
func BenchmarkTLSIngressRelay(b *testing.B) {
	for _, mode := range []string{"legacy64", "managed1", "managed64"} {
		b.Run(mode, func(b *testing.B) {
			store, e := OpenStore(filepath.Join(b.TempDir(), "state.json"))
			if e != nil {
				b.Fatal(e)
			}
			b.Cleanup(func() { store.Close() })
			store.SetIdentity(contract.Registered{NodeID: "node", Token: "benchmark"})
			r := NewRuntime(store, tunnel.Client{})
			b.Cleanup(r.Close)
			pair, roots := ingressTestCertificate(b)
			target := ingressTestOrigin(b, pair, 'a')
			address := ingressTestAddress(b)
			cfg := contract.Config{ContractVersion: 1, NodeID: "node", Version: 1, ValidUntil: time.Now().Add(time.Hour)}
			count := 64
			if mode == "managed1" {
				count = 1
			}
			for i := 0; i < count; i++ {
				v := testRule(target)
				v.ID, v.GroupID, v.UserID, v.Listen = fmt.Sprintf("route%d", i), "group", "owner", address
				v.SharedTLS = &contract.SharedTLS{ServerName: fmt.Sprintf("route%d.example.com", i)}
				if i == 0 {
					v.SharedTLS.ServerName = "a.example.com"
				}
				if mode == "legacy64" {
					if i > 0 {
						v.SharedTLS.ParentID = "route0"
					}
				} else {
					v.SharedTLS.IngressID = "shared"
				}
				lease := *v.Lease
				lease.ID = fmt.Sprintf("lease%d", i)
				lease.Bytes = 1 << 40
				lease.ExpiresAt = cfg.ValidUntil
				v.Lease = &lease
				cfg.Rules = append(cfg.Rules, v)
			}
			if mode != "legacy64" {
				cfg.TLSIngresses = []contract.SharedTLSIngress{{ID: "shared", NodeID: "node", GroupID: "group", Listen: address}}
			}
			if e = r.Apply(cfg, false); e != nil {
				b.Fatal(e)
			}
			c, e := tls.Dial("tcp", address, &tls.Config{RootCAs: roots, ServerName: "a.example.com"})
			if e != nil {
				b.Fatal(e)
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(time.Minute))
			if _, e = io.ReadFull(c, make([]byte, 1)); e != nil {
				b.Fatal(e)
			}
			payload := make([]byte, 32*1024)
			received := make([]byte, len(payload))
			b.SetBytes(int64(len(payload)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, e = c.Write(payload); e != nil {
					b.Fatal(e)
				}
				if _, e = io.ReadFull(c, received); e != nil {
					b.Fatal(e)
				}
			}
			b.StopTimer()
		})
	}
}

func TestTLSIngressIsolationAndHotRoutes(t *testing.T) {
	for _, exit := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "exit"}[exit], func(t *testing.T) {
			store, r, cfg := setup(t)
			pair, roots := ingressTestCertificate(t)
			ingress := contract.SharedTLSIngress{ID: "shared", NodeID: "node", GroupID: "group", Listen: ingressTestAddress(t)}
			a := testRule(ingressTestOrigin(t, pair, 'a'))
			a.ID, a.UserID, a.GroupID, a.Listen = "a", "alice", ingress.GroupID, ingress.Listen
			a.SharedTLS = &contract.SharedTLS{IngressID: ingress.ID, ServerName: "a.example.com"}
			b := testRule(ingressTestOrigin(t, pair, 'b'))
			b.ID, b.UserID, b.GroupID, b.Listen = "b", "bob", ingress.GroupID, ingress.Listen
			b.SharedTLS = &contract.SharedTLS{IngressID: ingress.ID, ServerName: "b.example.com"}
			lease := *b.Lease
			lease.ID = "bob-lease"
			b.Lease = &lease
			if exit {
				pair, exitRoots := chainCertificate(t)
				r.Client.TLS = &tls.Config{RootCAs: exitRoots}
				l, e := net.Listen("tcp", "127.0.0.1:0")
				if e != nil {
					t.Fatal(e)
				}
				s := &tunnel.Server{TLS: &tls.Config{Certificates: []tls.Certificate{pair}}, Token: "shared-ingress-exit-token"}
				done := make(chan struct{})
				go func() { defer close(done); s.Serve(l, "tls") }()
				t.Cleanup(func() { s.Close(); <-done })
				for _, v := range []*contract.Rule{&a, &b} {
					v.Transport = "tls"
					v.Tunnel = &contract.Tunnel{Endpoint: l.Addr().String(), ServerName: "localhost", Token: s.Token}
				}
			}
			cfg.TLSIngresses, cfg.Rules = []contract.SharedTLSIngress{ingress}, []contract.Rule{a}
			if e := r.Apply(cfg, true); e != nil {
				t.Fatal(e)
			}
			dial := func(name string, tag byte) net.Conn {
				t.Helper()
				c, e := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", ingress.Listen, &tls.Config{RootCAs: roots, ServerName: name})
				if e != nil {
					t.Fatal(e)
				}
				t.Cleanup(func() { c.Close() })
				c.SetDeadline(time.Now().Add(5 * time.Second))
				buf := make([]byte, 1)
				if _, e = io.ReadFull(c, buf); e != nil || buf[0] != tag {
					t.Fatalf("wrong backend %q: %v", buf, e)
				}
				return c
			}
			ac := dial("a.example.com", 'a')
			if e := exchange(ac, "before"); e != nil {
				t.Fatal(e)
			}
			cfg.Version++
			cfg.Rules = []contract.Rule{a, b}
			if e := r.Apply(cfg, true); e != nil {
				t.Fatal(e)
			}
			if e := exchange(ac, "after-add"); e != nil {
				t.Fatal("adding route disconnected another account", e)
			}
			r.InspectionProfiles = detect.Profiles{"unused": detect.Profile{Protocol: "http"}}
			cfg.Version++
			if e := r.Apply(cfg, true); e != nil {
				t.Fatal(e)
			}
			if e := exchange(ac, "unrelated-profile-change"); e != nil {
				t.Fatal("unselected local profile rotation disconnected route", e)
			}
			bc := dial("b.example.com", 'b')
			if e := exchange(bc, "bob"); e != nil {
				t.Fatal(e)
			}
			renewed := *a.Lease
			renewed.ID = "alice-renewed"
			renewed.ExpiresAt = renewed.ExpiresAt.Add(time.Minute)
			a.Lease = &renewed
			cfg.Version++
			cfg.Rules = []contract.Rule{a, b}
			if e := r.Apply(cfg, true); e != nil {
				t.Fatal(e)
			}
			if e := exchange(ac, "renewed"); e != nil {
				t.Fatal(e)
			}
			if used(store, renewed.ID) == 0 || used(store, b.Lease.ID) == 0 {
				t.Fatal("missing per-route metering")
			}
			r.StopLease(b.Lease.ID)
			if exchange(bc, "stopped") == nil {
				t.Fatal("retired route lease remained open")
			}
			if e := exchange(ac, "other-lease-stopped"); e != nil {
				t.Fatal("retiring another rule lease closed this stream", e)
			}
			renewedBob := *b.Lease
			renewedBob.ID = "bob-renewed"
			b.Lease = &renewedBob
			cfg.Version++
			cfg.Rules = []contract.Rule{a, b}
			if e := r.Apply(cfg, true); e != nil {
				t.Fatal(e)
			}
			bc = dial("b.example.com", 'b')
			limited := *b.Lease
			limited.Limits.MaxConnectionsPerNode = 1
			b.Lease = &limited
			cfg.Version++
			cfg.Rules = []contract.Rule{a, b}
			if e := r.Apply(cfg, true); e != nil {
				t.Fatal(e)
			}
			if exchange(bc, "downgraded") == nil {
				t.Fatal("limit change left route connection open")
			}
			if e := exchange(ac, "other-limit-changed"); e != nil {
				t.Fatal("other account limit change closed this stream", e)
			}
			bc = dial("b.example.com", 'b')
			cfg.Version++
			cfg.Rules = []contract.Rule{a}
			if e := r.Apply(cfg, true); e != nil {
				t.Fatal(e)
			}
			if e := exchange(bc, "revoked"); e == nil {
				t.Fatal("removed route remained open")
			}
			if e := exchange(ac, "after-remove"); e != nil {
				t.Fatal("removal disconnected another account", e)
			}
			cfg.Version++
			cfg.Rules = nil
			if e := r.Apply(cfg, true); e != nil {
				t.Fatal(e)
			}
			if len(r.listeners) != 1 || r.TLSIngressStatuses()[0].Routes != 0 {
				t.Fatal("last rule owned listener lifecycle")
			}
			raw, e := net.DialTimeout("tcp", ingress.Listen, time.Second)
			if e != nil {
				t.Fatal("empty administrator ingress stopped listening", e)
			}
			raw.Close()
		})
	}
}

type fragmentIngressConn struct{ net.Conn }

func (c fragmentIngressConn) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		size := min(17, len(p))
		written, err := c.Conn.Write(p[:size])
		n += written
		p = p[written:]
		if err != nil {
			return n, err
		}
		if written == 0 {
			return n, io.ErrShortWrite
		}
	}
	return n, nil
}

func TestTLSIngressFragmentedHelloAndCapacity(t *testing.T) {
	_, r, cfg := setup(t)
	pair, roots := ingressTestCertificate(t)
	ingress := contract.SharedTLSIngress{ID: "shared", NodeID: "node", GroupID: "group", Listen: ingressTestAddress(t)}
	target := ingressTestOrigin(t, pair, 'a')
	for i := 0; i < 64; i++ {
		v := testRule(target)
		v.ID, v.GroupID, v.UserID, v.Listen = fmt.Sprintf("route%d", i), "group", "owner", ingress.Listen
		v.Lease.ID = fmt.Sprintf("lease%d", i)
		v.SharedTLS = &contract.SharedTLS{IngressID: "shared", ServerName: fmt.Sprintf("route%d.example.com", i)}
		if i == 0 {
			v.SharedTLS.ServerName = "a.example.com"
		}
		cfg.Rules = append(cfg.Rules, v)
	}
	cfg.TLSIngresses = []contract.SharedTLSIngress{ingress}
	if e := r.Apply(cfg, true); e != nil {
		t.Fatal(e)
	}
	raw, e := net.DialTimeout("tcp", ingress.Listen, time.Second)
	if e != nil {
		t.Fatal(e)
	}
	raw.SetDeadline(time.Now().Add(5 * time.Second))
	c := tls.Client(fragmentIngressConn{raw}, &tls.Config{RootCAs: roots, ServerName: "a.example.com"})
	defer c.Close()
	if e = c.Handshake(); e != nil {
		t.Fatalf("fragmented ClientHello failed: %v; ingress: %+v", e, r.TLSIngressStatuses())
	}
	if _, e = io.ReadFull(c, make([]byte, 1)); e != nil {
		t.Fatal(e)
	}
	if e = exchange(c, "fragmented"); e != nil {
		t.Fatal(e)
	}
	extra := cfg.Rules[0]
	extra.ID = "extra"
	extra.SharedTLS = &contract.SharedTLS{IngressID: "shared", ServerName: "extra.example.com"}
	cfg.Version++
	cfg.Rules = append(cfg.Rules, extra)
	if e = r.Apply(cfg, true); e == nil {
		t.Fatal("accepted 65 routes")
	}
	if e = exchange(c, "rollback"); e != nil {
		t.Fatal("invalid update changed live socket", e)
	}
}

func TestTLSIngressRejectsAndExpires(t *testing.T) {
	_, r, cfg := setup(t)
	pair, roots := ingressTestCertificate(t)
	ingress := contract.SharedTLSIngress{ID: "shared", NodeID: "node", GroupID: "group", Listen: ingressTestAddress(t)}
	a := testRule(ingressTestOrigin(t, pair, 'a'))
	a.ID, a.UserID, a.GroupID, a.Listen = "a", "alice", "group", ingress.Listen
	a.SharedTLS = &contract.SharedTLS{IngressID: ingress.ID, ServerName: "a.example.com"}
	cfg.TLSIngresses, cfg.Rules = []contract.SharedTLSIngress{ingress}, []contract.Rule{a}
	if e := r.Apply(cfg, true); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"", "unknown.example.com"} {
		c, e := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", ingress.Listen, &tls.Config{RootCAs: roots, ServerName: name})
		if c != nil {
			c.Close()
		}
		if e == nil {
			t.Fatal("unroutable handshake accepted", name)
		}
	}
	c, e := net.DialTimeout("tcp", ingress.Listen, time.Second)
	if e != nil {
		t.Fatal(e)
	}
	c.SetDeadline(time.Now().Add(time.Second))
	c.Write([]byte("GET / HTTP/1.1\r\nHost: a.example.com\r\n\r\n"))
	buf := make([]byte, 1)
	if _, e = c.Read(buf); e == nil {
		t.Fatal("plaintext accepted")
	}
	c.Close()
	if r.TLSIngressStatuses()[0].Rejected < 3 {
		t.Fatal("handshake rejection status missing")
	}
	ac, e := tls.Dial("tcp", ingress.Listen, &tls.Config{RootCAs: roots, ServerName: "a.example.com"})
	if e != nil {
		t.Fatal(e)
	}
	defer ac.Close()
	ac.SetDeadline(time.Now().Add(3 * time.Second))
	if _, e = io.ReadFull(ac, buf); e != nil {
		t.Fatal(e)
	}
	cfg.Version++
	cfg.ValidUntil = time.Now().Add(100 * time.Millisecond)
	if e := r.Apply(cfg, true); e != nil {
		t.Fatal(e)
	}
	// An idle established connection must close when configuration expires.
	if _, e = ac.Read(buf); e == nil {
		t.Fatal("expired configuration kept idle connection alive")
	}
	if r.TLSIngressStatuses()[0].State != "expired" {
		t.Fatal("missing expired status")
	}
}
