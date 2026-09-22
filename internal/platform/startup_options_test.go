package platform

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

func TestConfiguredRequestLimitsUseUserAndProxyIP(t *testing.T) {
	f := setup(t)
	f.s.opts.TrustProxy = true
	f.s.opts.DefaultRateLimit = &RateLimit{Period: time.Minute, Limit: 1}
	f.s.opts.UserRateLimit = &RateLimit{Period: time.Minute, Limit: 2}
	handler := f.s.RequestLimits(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	request := func(ip string, cookie bool, path string) int {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("X-Real-IP", ip)
		if cookie {
			r.AddCookie(f.cookie)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}
	if request("203.0.113.1", false, "/api/v1/site") != 204 || request("203.0.113.1", false, "/api/v1/site") != 429 || request("203.0.113.2", false, "/api/v1/site") != 204 {
		t.Fatal("client IP limits not isolated")
	}
	if request("203.0.113.1", true, "/api/v1/groups") != 204 || request("203.0.113.2", true, "/api/v1/plans") != 204 || request("203.0.113.3", true, "/online/device/ip") != 429 {
		t.Fatal("account limits not shared across IP/routes")
	}
	for _, path := range []string{"/api/v1/health", "/api/v1/agent/config", "/api/v1/payments/epay/notify", "/assets/app.js", "/download/agent-install.sh"} {
		if request("203.0.113.1", false, path) != 204 {
			t.Fatalf("browser limit applied to %s", path)
		}
	}
	f.s.mu.Lock()
	f.s.limits["request-ip:203.0.113.1"] = limit{since: time.Now().Add(-2 * time.Minute), period: time.Minute, count: 100}
	f.s.mu.Unlock()
	if request("203.0.113.1", false, "/api/v1/site") != 204 {
		t.Fatal("rate window did not reset")
	}
}

func TestProbeOfflineWindowAndRetention(t *testing.T) {
	f := setup(t)
	_, node := f.node()
	f.s.opts.OfflineNodeTime = 20 * time.Second
	f.s.opts.OfflineNodeRetention = 600 * time.Second
	// No external geo query is needed for this fixture.
	f.s.probes[node.NodeID] = contract.Probe{NodeID: node.NodeID, SampledAt: time.Now().Add(time.Hour)}
	for _, tc := range []struct {
		age    time.Duration
		count  int
		online bool
	}{
		{5 * time.Second, 1, true}, {30 * time.Second, 1, false}, {601 * time.Second, 0, false},
	} {
		at := time.Now().Add(-tc.age)
		if _, err := f.s.Store.DB.Exec(f.s.q("UPDATE cp_nodes SET last_seen=? WHERE id=?"), at.Unix(), node.NodeID); err != nil {
			t.Fatal(err)
		}
		f.s.lastContact[node.NodeID] = at
		items, err := f.s.visibleProbes(context.Background(), contract.User{Role: "admin"}, "")
		if err != nil || len(items) != tc.count {
			t.Fatal("retention mismatch", len(items), err)
		}
		if tc.count > 0 && (items[0].Online == nil || *items[0].Online != tc.online) {
			t.Fatal("offline state used node's sample clock")
		}
	}
	// The node record remains available after disappearing from the live list.
	read[struct{ Items []contract.Node }](t, f.req("GET", "/nodes", nil, ""), 200)
}
