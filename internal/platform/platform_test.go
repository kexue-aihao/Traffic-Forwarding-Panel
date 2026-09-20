package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/testdb"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type fixture struct {
	t      *testing.T
	s      *Server
	m      *http.ServeMux
	cookie *http.Cookie
}

func setup(t *testing.T) *fixture {
	t.Helper()
	s := New(testdb.Open(t), Options{AdminTestBytes: 1024})
	if e := s.Bootstrap(context.Background(), "admin", "test-password-long"); e != nil {
		t.Fatal(e)
	}
	mux := http.NewServeMux()
	s.Register(mux)
	f := &fixture{t: t, s: s, m: mux}
	rr := f.req("POST", "/auth/login", map[string]any{"username": "admin", "password": "test-password-long"}, "")
	if rr.Code != 200 {
		t.Fatal(rr.Code, rr.Body.String())
	}
	f.cookie = rr.Result().Cookies()[0]
	return f
}
func (f *fixture) req(method, path string, v any, bearer string) *httptest.ResponseRecorder {
	var b []byte
	if v != nil {
		b, _ = json.Marshal(v)
	}
	r := httptest.NewRequest(method, "http://panel/api/v1"+path, bytes.NewReader(b))
	r.Header.Set("X-Requested-With", "fetch")
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	} else if f.cookie != nil {
		r.AddCookie(f.cookie)
	}
	rr := httptest.NewRecorder()
	f.m.ServeHTTP(rr, r)
	return rr
}
func read[T any](t *testing.T, r *httptest.ResponseRecorder, status int) T {
	t.Helper()
	if r.Code != status {
		t.Fatalf("status %d wanted %d: %s", r.Code, status, r.Body.String())
	}
	var v T
	if e := json.Unmarshal(r.Body.Bytes(), &v); e != nil {
		t.Fatal(e)
	}
	return v
}
func (f *fixture) node() (contract.Group, contract.Registered) {
	g := read[contract.Group](f.t, f.req("POST", "/groups", map[string]any{"name": "test", "port_min": 20000, "port_max": 21000, "multiplier": "1"}, ""), 201)
	en := read[map[string]string](f.t, f.req("POST", "/nodes/enrollment", map[string]any{"name": "node", "group_ids": []string{g.ID}}, ""), 201)
	node := read[contract.Registered](f.t, f.req("POST", "/agent/register", contract.Registration{Token: en["token"], Name: "node"}, ""), 201)
	if rr := f.req("POST", "/agent/register", contract.Registration{Token: en["token"]}, ""); rr.Code != 401 {
		f.t.Fatal("enrollment reused", rr.Code)
	}
	return g, node
}
func ruleFor(g contract.Group, n contract.Registered) contract.Rule {
	return contract.Rule{Name: "test", NodeID: n.NodeID, GroupID: g.ID, Network: "tcp", Transport: "direct", Listen: "0.0.0.0:20001", Target: "127.0.0.1:8080", Enabled: true}
}
func TestConcurrentPortAndACKRelease(t *testing.T) {
	f := setup(t)
	g, n := f.node()
	var wg sync.WaitGroup
	out := make(chan *httptest.ResponseRecorder, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); out <- f.req("POST", "/rules", ruleFor(g, n), "") }()
	}
	wg.Wait()
	close(out)
	var created contract.Rule
	success := 0
	for rr := range out {
		if rr.Code == 201 {
			created = read[contract.Rule](t, rr, 201)
			success++
		} else if rr.Code != 409 {
			t.Fatal(rr.Code, rr.Body.String())
		}
	}
	if success != 1 {
		t.Fatalf("port allocated %d times", success)
	}
	cfg := read[contract.Config](t, f.req("GET", "/agent/config", nil, n.Token), 200)
	if len(cfg.Rules) != 1 || cfg.Rules[0].Lease == nil {
		t.Fatal("missing finite config")
	}
	if r := f.req("DELETE", fmt.Sprintf("/rules/%s?version=%d", created.ID, created.Version), nil, ""); r.Code != 204 {
		t.Fatal(r.Code, r.Body.String())
	}
	if r := f.req("POST", "/rules", ruleFor(g, n), ""); r.Code != 409 {
		t.Fatal("port released before ACK")
	}
	if r := f.req("POST", "/agent/ack", contract.Ack{Version: cfg.Version, AppliedVersion: cfg.Version}, n.Token); r.Code != 409 {
		t.Fatal("stale ACK accepted")
	}
	latest := read[contract.Config](t, f.req("GET", "/agent/config", nil, n.Token), 200)
	if len(latest.Rules) != 0 {
		t.Fatal("deleted rule revived")
	}
	if r := f.req("POST", "/agent/ack", contract.Ack{Version: latest.Version, AppliedVersion: latest.Version}, n.Token); r.Code != 204 {
		t.Fatal(r.Code, r.Body.String())
	}
	read[contract.Rule](t, f.req("POST", "/rules", ruleFor(g, n), ""), 201)
}
func TestUsageAtomicDedupAndOverrun(t *testing.T) {
	f := setup(t)
	g, n := f.node()
	read[contract.Rule](t, f.req("POST", "/rules", ruleFor(g, n), ""), 201)
	cfg := read[contract.Config](t, f.req("GET", "/agent/config", nil, n.Token), 200)
	rule := cfg.Rules[0]
	u := contract.UsageRecord{ID: "usage-a", NodeID: n.NodeID, RuleID: rule.ID, LeaseID: rule.Lease.ID, EntitlementID: rule.Lease.EntitlementID, StartedAt: time.Now().UTC().Add(-time.Second), EndedAt: time.Now().UTC(), UploadBytes: 100}
	batch := contract.UsageBatch{Records: []contract.UsageRecord{u}}
	for i := 0; i < 2; i++ {
		read[map[string]any](t, f.req("POST", "/agent/usage", batch, n.Token), 200)
	}
	var used int64
	if e := f.s.Store.DB.QueryRow(f.s.q(`SELECT bytes_used FROM cp_rule_leases WHERE id=?`), rule.Lease.ID).Scan(&used); e != nil || used != 100 {
		t.Fatal("duplicate charged", used, e)
	}
	u.ID = "usage-b"
	u.UploadBytes = 1000
	if rr := f.req("POST", "/agent/usage", contract.UsageBatch{Records: []contract.UsageRecord{u}}, n.Token); rr.Code != 409 {
		t.Fatal("overrun accepted")
	}
	u.ID = "usage-a"
	u.UploadBytes = 90
	if rr := f.req("POST", "/agent/usage", contract.UsageBatch{Records: []contract.UsageRecord{u}}, n.Token); rr.Code != 409 {
		t.Fatal("changed duplicate accepted")
	}
}
func TestAuthorizationCSRFAndTokenRevocation(t *testing.T) {
	f := setup(t)
	g, n := f.node()
	read[contract.Rule](t, f.req("POST", "/rules", ruleFor(g, n), ""), 201)
	u := read[contract.User](t, f.req("POST", "/users", map[string]any{"username": "alice", "password": "test-password-long", "role": "user"}, ""), 201)
	adminCookie := f.cookie
	rr := f.req("POST", "/auth/login", map[string]any{"username": "alice", "password": "test-password-long"}, "")
	read[map[string]any](t, rr, 200)
	f.cookie = rr.Result().Cookies()[0]
	if r := f.req("GET", "/users", nil, ""); r.Code != 403 {
		t.Fatal("admin boundary failed")
	}
	list := read[struct {
		Items []contract.Rule `json:"items"`
	}](t, f.req("GET", "/rules", nil, ""), 200)
	if len(list.Items) != 0 {
		t.Fatal("cross-tenant disclosure")
	}
	if r := f.req("POST", "/rules", ruleFor(g, n), ""); r.Code != 409 {
		t.Fatal("group authorization failed")
	}
	raw := httptest.NewRequest("POST", "http://panel/api/v1/auth/logout", bytes.NewBufferString("{}"))
	raw.AddCookie(f.cookie)
	raw.Header.Set("Origin", "https://evil.test")
	raw.Header.Set("X-Requested-With", "fetch")
	res := httptest.NewRecorder()
	f.m.ServeHTTP(res, raw)
	if res.Code != 403 {
		t.Fatal("CSRF accepted")
	}
	tok := read[map[string]any](t, f.req("POST", "/auth/tokens", map[string]any{"name": "test", "expires_at": time.Now().Add(time.Hour)}, ""), 201)
	read[map[string]any](t, f.req("GET", "/auth/session", nil, tok["token"].(string)), 200)
	f.cookie = adminCookie
	if rr := f.req("PUT", "/users/"+u.ID+"/status", map[string]bool{"disabled": true}, ""); rr.Code != 204 {
		t.Fatal(rr.Code, rr.Body.String())
	}
	if rr := f.req("GET", "/auth/session", nil, tok["token"].(string)); rr.Code != 401 {
		t.Fatal("disabled token remained valid")
	}
}

func TestRetirementRequiresSettledUsageAndNeverReplaysGrant(t *testing.T) {
	f := setup(t)
	g, n := f.node()
	read[contract.Rule](t, f.req("POST", "/rules", ruleFor(g, n), ""), 201)
	cfg := read[contract.Config](t, f.req("GET", "/agent/config", nil, n.Token), 200)
	rule := cfg.Rules[0]
	bad := f.req("POST", "/agent/leases/retire", map[string]any{"lease_id": rule.Lease.ID, "used_bytes": "1"}, n.Token)
	if bad.Code != 409 {
		t.Fatal("retired unmatched usage")
	}
	for i := 0; i < 2; i++ {
		rr := f.req("POST", "/agent/leases/retire", map[string]any{"lease_id": rule.Lease.ID, "used_bytes": "0"}, n.Token)
		if rr.Code != 204 {
			t.Fatal(rr.Code, rr.Body.String())
		}
	}
	cfg = read[contract.Config](t, f.req("GET", "/agent/config", nil, n.Token), 200)
	if len(cfg.Rules) != 0 {
		t.Fatal("test allowance silently reissued")
	}
	u := contract.UsageRecord{ID: "after-retire", NodeID: n.NodeID, RuleID: rule.ID, LeaseID: rule.Lease.ID, EntitlementID: rule.Lease.EntitlementID, StartedAt: time.Now().Add(-time.Second), EndedAt: time.Now(), UploadBytes: 1}
	if rr := f.req("POST", "/agent/usage", contract.UsageBatch{Records: []contract.UsageRecord{u}}, n.Token); rr.Code != 409 {
		t.Fatal("new usage after retire accepted")
	}
}

func TestRecoveryAndNodeRotationRevokeCredentials(t *testing.T) {
	f := setup(t)
	_, n := f.node()
	rotation := read[map[string]string](t, f.req("POST", "/nodes/"+n.NodeID+"/rotate-token", map[string]any{}, ""), 200)
	if rr := f.req("GET", "/agent/config", nil, n.Token); rr.Code != 401 {
		t.Fatal("old node credential accepted")
	}
	read[contract.Config](t, f.req("GET", "/agent/config", nil, rotation["token"]), 200)
	if e := f.s.ResetPassword(context.Background(), "admin", "replacement-password"); e != nil {
		t.Fatal(e)
	}
	if rr := f.req("GET", "/auth/session", nil, ""); rr.Code != 401 {
		t.Fatal("old session survived recovery")
	}
	read[map[string]any](t, f.req("POST", "/auth/login", map[string]any{"username": "admin", "password": "replacement-password"}, ""), 200)
}

func TestTokenListOwnerIsolationAndNoSecrets(t *testing.T) {
	f := setup(t)
	admin := f.cookie
	token := read[map[string]any](t, f.req("POST", "/auth/tokens", map[string]any{"name": "private-admin-token", "expires_at": time.Now().Add(time.Hour)}, ""), 201)
	read[contract.User](t, f.req("POST", "/users", map[string]any{"username": "other", "password": "test-password-long", "role": "user"}, ""), 201)
	res := f.req("POST", "/auth/login", map[string]any{"username": "other", "password": "test-password-long"}, "")
	f.cookie = res.Result().Cookies()[0]
	other := read[struct {
		Total int `json:"total"`
	}](t, f.req("GET", "/auth/tokens", nil, ""), 200)
	if other.Total != 0 {
		t.Fatal("cross-user token list")
	}
	if rr := f.req("DELETE", "/auth/tokens/"+token["id"].(string), nil, ""); rr.Code != 204 {
		t.Fatal(rr.Code)
	}
	f.cookie = admin
	rr := f.req("GET", "/auth/tokens", nil, "")
	list := read[struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}](t, rr, 200)
	if list.Total != 1 || len(list.Items) != 1 {
		t.Fatal("cross-user revocation")
	}
	if _, ok := list.Items[0]["token"]; ok {
		t.Fatal("secret disclosure")
	}
	if _, ok := list.Items[0]["token_hash"]; ok {
		t.Fatal("hash disclosure")
	}
	if bytes.Contains(rr.Body.Bytes(), []byte(token["token"].(string))) {
		t.Fatal("secret in response")
	}
}
