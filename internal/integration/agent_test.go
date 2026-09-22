// These tests exercise the real HTTP application, SQL control/commerce stores,
// Agent and sockets. Payment notifications are signed local fixtures, NOT live
// merchant payments or evidence that a provider account is production verified.
package integration

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/agent"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/app"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/commerce"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/payment"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/probe"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/storage"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/testdb"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/tunnel"
)

type fixture struct {
	t           *testing.T
	app         *app.App
	db          *storage.Store
	http        *httptest.Server
	admin, user *http.Cookie
	group       contract.Group
	owner       contract.User
	plan        commerce.Plan
	ent         commerce.Entitlement
	store       *agent.Store
	agent       *agent.Agent
	statePath   string
	client      tunnel.Client
}

func (f *fixture) request(method, path string, body any, cookie *http.Cookie, status int) []byte {
	f.t.Helper()
	var b []byte
	var e error
	if body != nil {
		b, e = json.Marshal(body)
		if e != nil {
			f.t.Fatal(e)
		}
	}
	r, e := http.NewRequest(method, f.http.URL+"/api/v1"+path, bytes.NewReader(b))
	if e != nil {
		f.t.Fatal(e)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", f.http.URL)
	r.Header.Set("X-Requested-With", "fetch")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	res, e := f.http.Client().Do(r)
	if e != nil {
		f.t.Fatal(e)
	}
	defer res.Body.Close()
	data, e := io.ReadAll(res.Body)
	if e != nil {
		f.t.Fatal(e)
	}
	if res.StatusCode != status {
		f.t.Fatalf("%s %s: status %d expected %d: %s", method, path, res.StatusCode, status, data)
	}
	return data
}
func decode[T any](t *testing.T, b []byte) T {
	t.Helper()
	var v T
	if e := json.Unmarshal(b, &v); e != nil {
		t.Fatal(e)
	}
	return v
}
func (f *fixture) login(name string) *http.Cookie {
	f.t.Helper()
	b, _ := json.Marshal(map[string]string{"username": name, "password": "integration-long-password"})
	req, _ := http.NewRequest("POST", f.http.URL+"/api/v1/auth/login", bytes.NewReader(b))
	req.Header.Set("X-Requested-With", "fetch")
	req.Header.Set("Origin", f.http.URL)
	res, e := f.http.Client().Do(req)
	if e != nil {
		f.t.Fatal(e)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		data, _ := io.ReadAll(res.Body)
		f.t.Fatalf("login %d %s", res.StatusCode, data)
	}
	for _, c := range res.Cookies() {
		if c.Name == "tfp_session" {
			return c
		}
	}
	f.t.Fatal("session cookie missing")
	return nil
}

func newFixture(t *testing.T, tc tunnel.Client) *fixture {
	t.Helper()
	db := testdb.Open(t)
	a, e := app.New(context.Background(), db, app.Options{EPay: payment.EPay{Gateway: "https://fixture.invalid", PID: "fixture-merchant", Key: "fixture-secret", NotifyURL: "https://panel.invalid/api/v1/payments/epay/notify", ReturnURL: "https://panel.invalid/#/commerce"}})
	if e != nil {
		t.Fatal(e)
	}
	if e = a.Platform.Bootstrap(context.Background(), "admin", "integration-long-password"); e != nil {
		t.Fatal(e)
	}
	probeSeen := make(chan struct{}, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.Handler.ServeHTTP(w, r)
		if r.URL.Path == "/api/v1/agent/probe" {
			select {
			case probeSeen <- struct{}{}:
			default:
			}
		}
	}))
	t.Cleanup(server.Close)
	f := &fixture{t: t, app: a, db: db, http: server, client: tc}
	f.admin = f.login("admin")
	f.owner = decode[contract.User](t, f.request("POST", "/users", map[string]string{"username": "alice", "password": "integration-long-password", "role": "user"}, f.admin, 201))
	f.user = f.login("alice")
	f.group = decode[contract.Group](t, f.request("POST", "/groups", map[string]any{"name": "integration", "user_ids": []string{f.owner.ID}, "multiplier": "1", "port_min": 1024, "port_max": 65535}, f.admin, 201))
	enrollment := decode[map[string]string](t, f.request("POST", "/nodes/enrollment", map[string]any{"name": "integration-node", "group_ids": []string{f.group.ID}}, f.admin, 201))
	f.statePath = filepath.Join(t.TempDir(), "agent.json")
	f.store, e = agent.OpenStore(f.statePath)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { f.agent.Runtime.Close(); f.store.Close() })
	f.agent = &agent.Agent{URL: server.URL, EnrollmentToken: enrollment["token"], Name: "integration-node", HTTP: server.Client(), Store: f.store, Runtime: agent.NewRuntime(f.store, tc), Probe: &probe.Collector{}, PollInterval: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.agent.Run(ctx) }()
	select {
	case <-probeSeen:
	case err := <-done:
		cancel()
		t.Fatalf("agent stopped registering: %v", err)
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("registration/config/ack/probe timed out")
	}
	cancel()
	if e = <-done; e != nil {
		t.Fatal(e)
	}
	f.agent.Runtime = agent.NewRuntime(f.store, tc)
	// Real order and signed HTTP callback exercise the verified-payment path.
	// fixture.invalid is never contacted and no real money changes hands.
	order := decode[commerce.Order](t, f.request("POST", "/orders", map[string]any{"channel": "epay", "amount_cents": "10000", "idempotency_key": "fixture-topup"}, f.user, 200))
	callback := url.Values{"pid": {"fixture-merchant"}, "out_trade_no": {order.ID}, "trade_no": {"fixture-transaction"}, "money": {"100.00"}, "trade_status": {"TRADE_SUCCESS"}, "sign_type": {"MD5"}}
	callback.Set("sign", payment.EPaySign(callback, "fixture-secret"))
	for i := 0; i < 2; i++ {
		f.request("GET", "/payments/epay/notify?"+callback.Encode(), nil, nil, 200)
	}
	wallet := decode[commerce.Wallet](t, f.request("GET", "/wallet", nil, f.user, 200))
	if wallet.Balance != 10000 {
		t.Fatalf("duplicate callback balance %d", wallet.Balance)
	}
	f.plan = decode[commerce.Plan](t, f.request("POST", "/plans", commerce.Plan{Name: "integration", Price: 1000, Quota: 64 << 20, Months: 1}, f.admin, 200))
	f.ent = f.purchase("first", 0)
	return f
}
func (f *fixture) purchase(key string, version int64) commerce.Entitlement {
	return decode[commerce.Entitlement](f.t, f.request("POST", "/purchases", map[string]any{"plan_id": f.plan.ID, "idempotency_key": key, "expected_version": version}, f.user, 200))
}
func (f *fixture) sync() {
	f.t.Helper()
	if e := f.agent.Step(context.Background()); e != nil {
		f.t.Fatalf("real Agent sync failed: %v", e)
	}
}
func freeAddress(t *testing.T, network string) string {
	t.Helper()
	if network == "udp" {
		c, e := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
		if e != nil {
			t.Fatal(e)
		}
		a := c.LocalAddr().String()
		c.Close()
		return a
	}
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	a := l.Addr().String()
	l.Close()
	return a
}
func targets(t *testing.T) (string, string) {
	t.Helper()
	l, e := net.Listen("tcp", "127.0.0.1:0")
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
	u, e := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { u.Close() })
	go func() {
		p := make([]byte, 65535)
		for {
			n, peer, e := u.ReadFromUDP(p)
			if e != nil {
				return
			}
			u.WriteToUDP(p[:n], peer)
		}
	}()
	return l.Addr().String(), u.LocalAddr().String()
}
func (f *fixture) rule(network, transport, target string, params *contract.Tunnel) contract.Rule {
	return decode[contract.Rule](f.t, f.request("POST", "/rules", contract.Rule{Name: transport + "-" + network, NodeID: f.store.Identity().NodeID, GroupID: f.group.ID, Network: network, Transport: transport, Listen: freeAddress(f.t, network), Target: target, Enabled: true, Tunnel: params}, f.user, 201))
}
func transfer(t *testing.T, rule contract.Rule, p []byte) {
	t.Helper()
	c, e := net.DialTimeout(rule.Network, rule.Listen, 2*time.Second)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, e = c.Write(p); e != nil {
		t.Fatal(e)
	}
	if rule.Network == "tcp" {
		c.(*net.TCPConn).CloseWrite()
		got, e := io.ReadAll(c)
		if e != nil || !bytes.Equal(got, p) {
			t.Fatalf("TCP echo %q %v", got, e)
		}
	} else {
		buf := make([]byte, 65535)
		n, e := c.Read(buf)
		if e != nil || !bytes.Equal(buf[:n], p) {
			t.Fatalf("UDP echo %q %v", buf[:n], e)
		}
	}
}
func (f *fixture) lease(rule string) *contract.Lease {
	for _, r := range f.store.Config().Rules {
		if r.ID == rule {
			return r.Lease
		}
	}
	f.t.Fatal("rule absent from applied configuration")
	return nil
}
func (f *fixture) number(query string, args ...any) int64 {
	f.t.Helper()
	var v int64
	if e := f.db.DB.QueryRow(f.db.Rebind(query), args...).Scan(&v); e != nil {
		f.t.Fatal(e)
	}
	return v
}

func TestRealControlPlaneDirectRenewalRetirementAndRevocation(t *testing.T) {
	f := newFixture(t, tunnel.Client{})
	tcp, udp := targets(t)
	r1 := f.rule("tcp", "direct", tcp, nil)
	r2 := f.rule("udp", "direct", udp, nil)
	f.sync()
	first := *f.lease(r1.ID)
	transfer(t, r1, []byte("tcp-cycle-one"))
	transfer(t, r2, []byte("udp-cycle-one"))
	f.sync()
	want := int64(2 * (len("tcp-cycle-one") + len("udp-cycle-one")))
	if got := f.number("SELECT used FROM commerce_entitlements WHERE id=?", f.ent.ID); got != want {
		t.Fatalf("raw billing %d want %d", got, want)
	}
	if f.number("SELECT applied_version FROM cp_nodes WHERE id=?", f.store.Identity().NodeID) != f.agent.Runtime.Version() {
		t.Fatal("ACK not persisted")
	}
	probes := decode[struct {
		Items []contract.Probe `json:"items"`
	}](t, f.request("GET", "/probes", nil, f.user, 200))
	if len(probes.Items) != 1 || probes.Items[0].NodeID != f.store.Identity().NodeID {
		t.Fatal("authorized probe missing")
	}
	// Retire a partially used allocation, confirm usage, and obtain a fresh grant.
	if e := f.store.Retire(first.ID); e != nil {
		t.Fatal(e)
	}
	f.agent.Runtime.StopLease(first.ID)
	f.sync()
	if f.lease(r1.ID).ID == first.ID {
		t.Fatal("retired lease was reissued")
	}
	if f.number("SELECT closed FROM commerce_lease_reservations WHERE lease_id=?", first.ID) != 1 {
		t.Fatal("allocation not returned")
	}
	transfer(t, r1, []byte("after-return"))
	f.sync()
	oldEnt := f.ent
	before := f.number("SELECT used FROM commerce_entitlements WHERE id=?", oldEnt.ID)
	f.ent = f.purchase("renew", oldEnt.Version)
	if f.ent.ID == oldEnt.ID || f.ent.Used != 0 || !f.ent.StartsAt.After(oldEnt.StartsAt) {
		t.Fatal("renewal did not immediately open a fresh cycle")
	}
	f.sync()
	if f.lease(r1.ID).EntitlementID != f.ent.ID {
		t.Fatal("old-cycle lease survived renewal")
	}
	transfer(t, r1, []byte("new-cycle"))
	f.sync()
	if f.number("SELECT used FROM commerce_entitlements WHERE id=?", oldEnt.ID) != before {
		t.Fatal("new transfer charged old cycle")
	}
	if f.number("SELECT used FROM commerce_entitlements WHERE id=?", f.ent.ID) != 18 {
		t.Fatal("new-cycle count incorrect")
	}
	// Group membership removal closes both listeners and excludes probe visibility.
	f.group.UserIDs = nil
	f.group = decode[contract.Group](t, f.request("PUT", "/groups/"+f.group.ID, f.group, f.admin, 200))
	f.sync()
	if len(f.store.Config().Rules) != 0 {
		t.Fatal("revoked rules remain applied")
	}
	if c, e := net.DialTimeout("tcp", r1.Listen, time.Second); e == nil {
		c.Close()
		t.Fatal("revoked listener still accepts")
	}
	probes = decode[struct {
		Items []contract.Probe `json:"items"`
	}](t, f.request("GET", "/probes", nil, f.user, 200))
	if len(probes.Items) != 0 {
		t.Fatal("revoked user still sees probe")
	}
	// Restart from the real persisted Agent state and reconnect to control plane.
	f.agent.Runtime.Close()
	f.store.Close()
	var e error
	f.store, e = agent.OpenStore(f.statePath)
	if e != nil {
		t.Fatal(e)
	}
	f.agent.Store = f.store
	f.agent.Runtime = agent.NewRuntime(f.store, f.client)
	f.sync()
	if len(f.store.Config().Rules) != 0 {
		t.Fatal("restart revived revoked rules")
	}
	f.request("DELETE", fmt.Sprintf("/rules/%s?version=%d", r1.ID, r1.Version), nil, f.admin, 204)
	f.request("DELETE", fmt.Sprintf("/rules/%s?version=%d", r2.ID, r2.Version), nil, f.admin, 204)
	f.sync()
	if f.number("SELECT COUNT(*) FROM cp_ports WHERE node_id=?", f.store.Identity().NodeID) != 0 {
		t.Fatal("ACK did not release deleted ports")
	}
	f.sync()
	if len(f.store.Config().Rules) != 0 {
		t.Fatal("reconnect revived deleted rules")
	}
}

func certificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	der, e := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if e != nil {
		t.Fatal(e)
	}
	kb, e := x509.MarshalPKCS8PrivateKey(key)
	if e != nil {
		t.Fatal(e)
	}
	cp := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	pair, e := tls.X509KeyPair(cp, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}))
	if e != nil {
		t.Fatal(e)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(cp)
	return pair, roots
}
func TestRealControlPlaneAllEncryptedCarriers(t *testing.T) {
	pair, roots := certificate(t)
	for _, transport := range []string{"tls", "ws", "wss", "http"} {
		t.Run(transport, func(t *testing.T) {
			f := newFixture(t, tunnel.Client{TLS: &tls.Config{RootCAs: roots}})
			tcp, udp := targets(t)
			l, e := net.Listen("tcp", "127.0.0.1:0")
			if e != nil {
				t.Fatal(e)
			}
			exit := &tunnel.Server{TLS: &tls.Config{Certificates: []tls.Certificate{pair}}, Token: "integration-exit-secret", Allowed: map[string]bool{"tcp|" + tcp: true, "udp|" + udp: true}}
			done := make(chan struct{})
			go func() { defer close(done); exit.Serve(l, transport) }()
			t.Cleanup(func() { exit.Close(); <-done })
			endpoint := l.Addr().String()
			if transport == "ws" || transport == "wss" {
				endpoint = transport + "://" + endpoint + "/tunnel"
			}
			params := &contract.Tunnel{Endpoint: endpoint, ServerName: "localhost", Token: exit.Token}
			r1 := f.rule("tcp", transport, tcp, params)
			r2 := f.rule("udp", transport, udp, params)
			f.sync()
			transfer(t, r1, []byte("encrypted-tcp"))
			transfer(t, r2, []byte("encrypted-udp"))
			f.sync()
			if got := f.number("SELECT used FROM commerce_entitlements WHERE id=?", f.ent.ID); got != 52 {
				t.Fatalf("%s control-to-wire-to-billing count %d", transport, got)
			}
			t.Logf("real application + SQL + Agent + %s TCP/UDP + settled raw 52 bytes", transport)
		})
	}
}

type outageTransport struct {
	base    http.RoundTripper
	offline atomic.Bool
}

func (o *outageTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if o.offline.Load() {
		return nil, errors.New("injected network outage")
	}
	return o.base.RoundTrip(r)
}

func TestControlOutageRestartAndLateUsageExactlyOnce(t *testing.T) {
	f := newFixture(t, tunnel.Client{})
	target, _ := targets(t)
	rule := f.rule("tcp", "direct", target, nil)
	f.sync()
	lease := *f.lease(rule.ID)
	network := &outageTransport{base: http.DefaultTransport}
	f.agent.HTTP = &http.Client{Transport: network, Timeout: time.Second}
	network.offline.Store(true)
	transfer(t, rule, []byte("before-restart"))
	if e := f.agent.Step(context.Background()); e == nil {
		t.Fatal("network failure not observed")
	}
	pendingBefore := len(f.store.Pending())
	if pendingBefore == 0 {
		t.Fatal("usage was lost during control outage")
	}
	if f.number("SELECT used FROM commerce_entitlements WHERE id=?", f.ent.ID) != 0 {
		t.Fatal("offline data unexpectedly settled")
	}
	f.agent.Runtime.Close()
	f.store.Close()
	var e error
	f.store, e = agent.OpenStore(f.statePath)
	if e != nil {
		t.Fatal(e)
	}
	f.agent.Store = f.store
	f.agent.Runtime = agent.NewRuntime(f.store, f.client)
	if e = f.agent.Runtime.Apply(f.store.Config(), false); e != nil {
		t.Fatal(e)
	}
	if len(f.store.Pending()) != pendingBefore {
		t.Fatal("restart dropped durable spool")
	}
	transfer(t, rule, []byte("after-restart"))
	if f.lease(rule.ID).ID != lease.ID {
		t.Fatal("offline restart changed lease identity")
	}
	network.offline.Store(false)
	f.sync()
	expected := int64(2 * (len("before-restart") + len("after-restart")))
	if got := f.number("SELECT used FROM commerce_entitlements WHERE id=?", f.ent.ID); got != expected {
		t.Fatalf("late usage %d expected %d", got, expected)
	}
	if len(f.store.Pending()) != 0 {
		t.Fatal("confirmed spool retained")
	}
	f.sync()
	if got := f.number("SELECT used FROM commerce_entitlements WHERE id=?", f.ent.ID); got != expected {
		t.Fatal("reconnect double-charged")
	}
	if f.lease(rule.ID).ID != lease.ID {
		t.Fatal("reconnect silently refilled allocation")
	}
}

func TestSeparateGroupPoliciesReachRealDataPlane(t *testing.T) {
	f := newFixture(t, tunnel.Client{})
	target, udp := targets(t)
	f.group.BlockedProtocols = []string{"app:http", "app:socks"}
	f.group.DisabledNetworks = []string{"udp"}
	f.group.DisabledTransports = []string{"tls"}
	f.group = decode[contract.Group](t, f.request("PUT", "/groups/"+f.group.ID, f.group, f.admin, 200))
	f.request("POST", "/rules", contract.Rule{Name: "blocked-udp", NodeID: f.store.Identity().NodeID, GroupID: f.group.ID, Network: "udp", Transport: "direct", Listen: freeAddress(t, "udp"), Target: udp, Enabled: true}, f.user, 409)
	rule := f.rule("tcp", "direct", target, nil)
	f.sync()
	for _, p := range f.store.Config().Rules[0].BlockedProtocols {
		if p != "http" && p != "socks" {
			t.Fatalf("control restriction leaked into detector %q", p)
		}
	}
	transfer(t, rule, []byte("unknown-allowed"))
	for _, payload := range [][]byte{[]byte("GET /private HTTP/1.1\r\n\r\n"), {5, 1, 0}} {
		c, e := net.DialTimeout("tcp", rule.Listen, time.Second)
		if e != nil {
			t.Fatal(e)
		}
		c.SetDeadline(time.Now().Add(2 * time.Second))
		c.Write(payload)
		c.(*net.TCPConn).CloseWrite()
		buf := make([]byte, 100)
		n, e := c.Read(buf)
		c.Close()
		if n != 0 || e == nil {
			t.Fatal("blocked plaintext forwarded")
		}
	}
	f.sync()
	if used := f.number("SELECT used FROM commerce_entitlements WHERE id=?", f.ent.ID); used != 30 {
		t.Fatalf("blocked payload was billed or allowed payload missing: %d", used)
	}
}
