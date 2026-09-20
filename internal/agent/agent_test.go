package agent

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/probe"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/tunnel"
)

func setup(t *testing.T) (*Store, *Runtime, contract.Config) {
	t.Helper()
	s, e := OpenStore(filepath.Join(t.TempDir(), "state.json"))
	if e != nil {
		t.Fatal(e)
	}
	if e = s.SetIdentity(contract.Registered{NodeID: "node", Token: "secret"}); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	r := NewRuntime(s, tunnel.Client{})
	t.Cleanup(r.Close)
	return s, r, contract.Config{ContractVersion: 1, NodeID: "node", Version: 1, ValidUntil: time.Now().Add(time.Hour)}
}
func testRule(target string) contract.Rule {
	return contract.Rule{ID: "rule", NodeID: "node", Network: "tcp", Transport: "direct", Listen: "127.0.0.1:0", Target: target, Enabled: true, Lease: &contract.Lease{ID: "lease", EntitlementID: "ent", Bytes: 1000000, ExpiresAt: time.Now().Add(5 * time.Minute)}}
}
func TestPersistenceBudgetRetirementAndSpool(t *testing.T) {
	s, _, c := setup(t)
	r := testRule("127.0.0.1:1")
	r.Lease.Bytes = 20
	if e := s.Charge(r, c.ValidUntil, true, 12); e != nil {
		t.Fatal(e)
	}
	s.Close()
	restarted, e := OpenStore(s.path)
	if e != nil {
		t.Fatal(e)
	}
	if e = restarted.Charge(r, c.ValidUntil, false, 9); e == nil {
		t.Fatal("restart refilled lease")
	}
	if len(restarted.Pending()) != 1 {
		t.Fatal("pending record lost")
	}
	if e = restarted.Retire(r.Lease.ID); e != nil {
		t.Fatal(e)
	}
	restarted.Close()
	again, e := OpenStore(s.path)
	if e != nil {
		t.Fatal(e)
	}
	defer again.Close()
	if e = again.Charge(r, c.ValidUntil, false, 1); e == nil {
		t.Fatal("retired lease revived")
	}
	if again.Retirements()[r.Lease.ID] != 12 {
		t.Fatal("incorrect final amount")
	}
	if e = again.Confirm([]string{again.Pending()[0].ID}); e != nil {
		t.Fatal(e)
	}
	if len(again.Pending()) != 0 {
		t.Fatal("ack not persisted")
	}
}
func TestDiskFailureAndFullSpoolFailClosed(t *testing.T) {
	s, _, c := setup(t)
	r := testRule("127.0.0.1:1")
	// The hot path now appends to the already-open WAL, so inject a real
	// closed-file write failure rather than changing the snapshot pathname.
	s.mu.Lock()
	s.wal.Close()
	s.mu.Unlock()
	if e := s.Charge(r, c.ValidUntil, true, 10); e == nil {
		t.Fatal("disk failure ignored")
	}
	if e := s.Available(r, c.ValidUntil); e == nil {
		t.Fatal("disk failure did not stop transfer")
	}
	s, _, c = setup(t)
	s.state.Pending = make([]contract.UsageRecord, MaxPendingRecords)
	if e := s.Charge(r, c.ValidUntil, true, 1); e == nil {
		t.Fatal("unbounded spool")
	}
}
func TestAtomicApplyDirectHalfCloseAndMeter(t *testing.T) {
	s, r, c := setup(t)
	target, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer target.Close()
	go func() {
		conn, e := target.Accept()
		if e != nil {
			return
		}
		defer conn.Close()
		io.Copy(conn, conn)
	}()
	rule := testRule(target.Addr().String())
	c.Rules = []contract.Rule{rule}
	if e = r.Apply(c, true); e != nil {
		t.Fatal(e)
	}
	addr := r.listeners[key(rule)].tcp.Addr().String()
	conn, e := net.DialTimeout("tcp", addr, time.Second)
	if e != nil {
		t.Fatal(e)
	}
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	conn.Write([]byte("hello"))
	conn.(*net.TCPConn).CloseWrite()
	got, e := io.ReadAll(conn)
	conn.Close()
	if e != nil || string(got) != "hello" {
		t.Fatalf("half-close failed: %q %v", got, e)
	}
	var up, down int64
	for _, u := range s.Pending() {
		up += u.UploadBytes
		down += u.DownloadBytes
	}
	if up != 5 || down != 5 {
		t.Fatalf("meter %d/%d", up, down)
	}
	occupied, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer occupied.Close()
	bad := c
	bad.Version = 2
	other := rule
	other.ID = "other"
	other.Listen = occupied.Addr().String()
	bad.Rules = []contract.Rule{rule, other}
	if e = r.Apply(bad, true); e == nil {
		t.Fatal("conflict accepted")
	}
	if r.Version() != 1 || s.Config().Version != 1 {
		t.Fatal("failed apply replaced old config")
	}
	if _, ok := r.listeners[key(rule)]; !ok {
		t.Fatal("old listener lost")
	}
}
func TestProtocolDetection(t *testing.T) {
	for _, v := range []struct {
		p    []byte
		want bool
	}{{[]byte("GET / HTTP/1.1"), true}, {[]byte{5, 1, 0}, true}, {[]byte{0x16, 3, 3, 0}, false}, {[]byte("unknown"), false}} {
		if got := blocked(v.p, []string{"http", "socks"}); got != v.want {
			t.Fatalf("%x: %v", v.p, got)
		}
	}
}
func TestUsageConfirmedBeforeRetire(t *testing.T) {
	s, r, c := setup(t)
	rule := testRule("127.0.0.1:1")
	if e := s.Charge(rule, c.ValidUntil, true, 12); e != nil {
		t.Fatal(e)
	}
	if e := s.Retire(rule.Lease.ID); e != nil {
		t.Fatal(e)
	}
	var mu sync.Mutex
	events := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if req.Header.Get("Authorization") != "Bearer secret" {
			t.Error("identity not sent")
		}
		switch req.URL.Path {
		case "/api/v1/agent/usage":
			events = append(events, "usage")
			var batch contract.UsageBatch
			json.NewDecoder(req.Body).Decode(&batch)
			ids := []string{}
			for _, u := range batch.Records {
				ids = append(ids, u.ID)
			}
			json.NewEncoder(w).Encode(map[string]any{"accepted": ids})
		case "/api/v1/agent/leases/retire":
			events = append(events, "retire")
			var body struct {
				Used int64 `json:"used_bytes,string"`
			}
			json.NewDecoder(req.Body).Decode(&body)
			if body.Used != 12 {
				t.Error("wrong retired count")
			}
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected %s", req.URL.Path)
		}
	}))
	defer srv.Close()
	a := Agent{URL: srv.URL, HTTP: srv.Client(), Store: s, Runtime: r, Probe: &probe.Collector{}}
	if e := a.retire(context.Background()); e != nil {
		t.Fatal(e)
	}
	if len(events) != 0 {
		t.Fatal("retired with pending bytes")
	}
	if e := a.flush(context.Background()); e != nil {
		t.Fatal(e)
	}
	if e := a.retire(context.Background()); e != nil {
		t.Fatal(e)
	}
	if len(events) != 2 || events[0] != "usage" || events[1] != "retire" {
		t.Fatalf("events %v", events)
	}
	if len(s.Retirements()) != 0 {
		t.Fatal("retirement not durable")
	}
}

func TestDirectUDPBoundariesAndStateProcessLock(t *testing.T) {
	s, r, c := setup(t)
	if duplicate, e := OpenStore(s.path); e == nil {
		duplicate.Close()
		t.Fatal("two processes can share node budget")
	}
	udp, e := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if e != nil {
		t.Fatal(e)
	}
	defer udp.Close()
	go func() {
		buf := make([]byte, 65535)
		for {
			n, p, e := udp.ReadFromUDP(buf)
			if e != nil {
				return
			}
			udp.WriteToUDP(buf[:n], p)
		}
	}()
	rule := testRule(udp.LocalAddr().String())
	rule.Network = "udp"
	c.Rules = []contract.Rule{rule}
	if e = r.Apply(c, true); e != nil {
		t.Fatal(e)
	}
	conn, e := net.Dial("udp", r.listeners[key(rule)].udp.LocalAddr().String())
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	for _, p := range [][]byte{[]byte("first"), {}, []byte("second-longer")} {
		if _, e = conn.Write(p); e != nil {
			t.Fatal(e)
		}
		buf := make([]byte, 128)
		n, e := conn.Read(buf)
		if e != nil || string(buf[:n]) != string(p) {
			t.Fatalf("UDP %q %v", buf[:n], e)
		}
	}
	var up, down int64
	for _, u := range s.Pending() {
		up += u.UploadBytes
		down += u.DownloadBytes
	}
	if up != 18 || down != 18 {
		t.Fatalf("UDP raw counts %d/%d", up, down)
	}
}

func TestRegistrationConfigAckProbeAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.json")
	var mu sync.Mutex
	registrations, acks, probes := 0, 0, 0
	observed := make(chan struct{}, 2)
	cfg := contract.Config{ContractVersion: 1, NodeID: "registered-node", Version: 1, ValidUntil: time.Now().Add(time.Hour), Rules: []contract.Rule{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if req.URL.Path != "/api/v1/agent/register" && req.Header.Get("Authorization") != "Bearer persisted-secret" {
			t.Error("missing persisted identity")
		}
		switch req.URL.Path {
		case "/api/v1/agent/register":
			registrations++
			var body contract.Registration
			json.NewDecoder(req.Body).Decode(&body)
			if body.Token != "one-time" {
				t.Error("enrollment not sent")
			}
			json.NewEncoder(w).Encode(contract.Registered{NodeID: "registered-node", Token: "persisted-secret"})
		case "/api/v1/agent/config":
			json.NewEncoder(w).Encode(cfg)
		case "/api/v1/agent/ack":
			acks++
			var ack contract.Ack
			json.NewDecoder(req.Body).Decode(&ack)
			if ack.AppliedVersion != 1 || ack.Error != "" {
				t.Errorf("ack %+v", ack)
			}
			w.WriteHeader(204)
		case "/api/v1/agent/probe":
			probes++
			w.WriteHeader(204)
			observed <- struct{}{}
		default:
			t.Errorf("unexpected request %s", req.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	for i := 0; i < 2; i++ {
		s, e := OpenStore(path)
		if e != nil {
			t.Fatal(e)
		}
		r := NewRuntime(s, tunnel.Client{})
		a := Agent{URL: srv.URL, EnrollmentToken: "one-time", Store: s, Runtime: r, HTTP: srv.Client(), PollInterval: time.Hour}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- a.Run(ctx) }()
		select {
		case <-observed:
		case <-time.After(5 * time.Second):
			cancel()
			t.Fatal("no probe")
		}
		cancel()
		if e = <-done; e != nil {
			t.Fatal(e)
		}
		s.Close()
	}
	mu.Lock()
	defer mu.Unlock()
	if registrations != 1 || acks != 2 || probes != 2 {
		t.Fatalf("lifecycle %d/%d/%d", registrations, acks, probes)
	}
}
