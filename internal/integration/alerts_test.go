package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/alerts"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/commerce"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/tunnel"
)

func TestNodeAlertDebounceRestartRevocationAndRollback(t *testing.T) {
	f := newFixture(t, tunnel.Client{})
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	m := f.app.Alerts
	m.Now = func() time.Time { return now }
	f.app.Commerce.Now = m.Now
	setSeen := func(v time.Time) {
		t.Helper()
		if _, err := f.db.DB.Exec(f.db.Rebind("UPDATE cp_nodes SET last_seen=? WHERE id=?"), v.Unix(), f.store.Identity().NodeID); err != nil {
			t.Fatal(err)
		}
	}
	run := func() {
		t.Helper()
		if err := m.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	count := func(user, kind string) int {
		t.Helper()
		var n int
		if err := f.db.DB.QueryRow(f.db.Rebind("SELECT COUNT(*) FROM commerce_events WHERE user_id=? AND kind=?"), user, kind).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	outsider := decode[contract.User](t, f.request("POST", "/users", map[string]string{"username": "outsider", "password": "integration-long-password", "role": "user"}, f.admin, 201))
	if _, _, err := f.app.Commerce.CreateWebhook(ctx, f.owner.ID, "https://hooks.example.test/events", []string{"node.offline", "node.recovered"}); err != nil {
		t.Fatal(err)
	}
	adminToken := decode[map[string]any](t, f.request("POST", "/auth/tokens", map[string]any{"name": "alerts-owner-only", "expires_at": time.Now().Add(time.Hour)}, f.admin, 201))["token"].(string)
	tokenRequest := func(method, path string, body any) []byte {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(method, "http://panel/api/v1"+path, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+adminToken)
		rr := httptest.NewRecorder()
		f.app.Handler.ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatal(rr.Code, rr.Body.String())
		}
		return rr.Body.Bytes()
	}
	tokenRequest("POST", "/webhooks", map[string]any{"url": "https://hooks.example.test/token", "events": []string{"node.recovered"}})
	setSeen(now)
	run()
	now = now.Add(89 * time.Second)
	run()
	if count(f.owner.ID, "node.offline") != 0 {
		t.Fatal("offline fired before grace")
	}
	now = now.Add(time.Second)
	// Real SQL failure must roll back both the transition and event checkpoint.
	if _, err := f.db.DB.Exec("ALTER TABLE commerce_events RENAME TO commerce_events_paused"); err != nil {
		t.Fatal(err)
	}
	err := m.RunOnce(ctx)
	if _, restoreErr := f.db.DB.Exec("ALTER TABLE commerce_events_paused RENAME TO commerce_events"); restoreErr != nil {
		t.Fatal(restoreErr)
	}
	if err == nil {
		t.Fatal("injected event failure ignored")
	}
	run()
	m = alerts.New(f.db, f.app.Commerce)
	m.Now = func() time.Time { return now }
	run()
	var fromToken struct {
		Items []commerce.Event `json:"items"`
	}
	if err := json.Unmarshal(tokenRequest("GET", "/events", nil), &fromToken); err != nil {
		t.Fatal(err)
	}
	for _, e := range fromToken.Items {
		if strings.HasPrefix(e.Kind, "node.") {
			t.Fatal("owner Token acquired administrator node scope")
		}
	}
	var fromAdmin struct {
		Items []commerce.Event `json:"items"`
	}
	if err := json.Unmarshal(f.request("GET", "/events", nil, f.admin, 200), &fromAdmin); err != nil {
		t.Fatal(err)
	}
	adminOffline := 0
	for _, e := range fromAdmin.Items {
		if e.Kind == "node.offline" {
			adminOffline++
		}
	}
	if adminOffline != 1 {
		t.Fatal("administrator lost node alerts", fromAdmin)
	}
	if count(f.owner.ID, "node.offline") != 1 || count(outsider.ID, "node.offline") != 0 {
		t.Fatal("offline dedupe/access failed")
	}
	now = now.Add(10 * time.Second)
	setSeen(now)
	run()
	now = now.Add(5 * time.Second)
	setSeen(now.Add(-100 * time.Second))
	run()
	if count(f.owner.ID, "node.recovered") != 0 {
		t.Fatal("flapping node recovered early")
	}
	now = now.Add(time.Second)
	setSeen(now)
	run()
	now = now.Add(15 * time.Second)
	setSeen(now)
	run()
	run()
	if count(f.owner.ID, "node.recovered") != 1 {
		t.Fatal("recovery not deduplicated")
	}
	f.group.UserIDs = nil
	f.group = decode[contract.Group](t, f.request("PUT", "/groups/"+f.group.ID, f.group, f.admin, 200))
	visible, err := f.app.Commerce.Events(ctx, f.owner.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range visible {
		if strings.HasPrefix(e.Kind, "node.") {
			t.Fatal("revoked node event remained visible")
		}
	}
	if sent, err := f.app.Commerce.RunWebhookDelivery(ctx, 100); err != nil || sent != 0 {
		t.Fatal("revoked alert attempted network delivery", sent, err)
	}
	records, err := f.app.Commerce.WebhookDeliveries(ctx, f.owner.ID)
	if err != nil || len(records) != 2 {
		t.Fatal(records, err)
	}
	for _, d := range records {
		if d["status"] != "access_revoked" {
			t.Fatal(d)
		}
	}
	// A control-plane outage is not immediately classified as node failure on restart.
	now = now.Add(time.Hour)
	m = alerts.New(f.db, f.app.Commerce)
	m.Now = func() time.Time { return now }
	run()
	var state string
	if err := f.db.DB.QueryRow(f.db.Rebind("SELECT status FROM cp_alert_nodes WHERE node_id=?"), f.store.Identity().NodeID).Scan(&state); err != nil || state != "online" {
		t.Fatal("restart skipped heartbeat grace", state, err)
	}
	now = now.Add(90 * time.Second)
	run()
	if err := f.db.DB.QueryRow(f.db.Rebind("SELECT status FROM cp_alert_nodes WHERE node_id=?"), f.store.Identity().NodeID).Scan(&state); err != nil || state != "offline" {
		t.Fatal("persistent loss not reported after startup grace", state, err)
	}

}

func TestEntitlementAlertsPrecisionCycleAndPolicyAuthorization(t *testing.T) {
	f := newFixture(t, tunnel.Client{})
	ctx := context.Background()
	p := decode[alerts.Policy](t, f.request("GET", "/alert-policy", nil, f.user, 200))
	p.RemainingPercent = 20
	f.request("PUT", "/alert-policy", p, f.user, 403)
	saved := decode[alerts.Policy](t, f.request("PUT", "/alert-policy", p, f.admin, 200))
	f.request("PUT", "/alert-policy", p, f.admin, 409)
	if saved.Version != p.Version+1 {
		t.Fatal(saved)
	}
	now := f.ent.ExpiresAt.Add(-time.Hour)
	f.app.Alerts.Now = func() time.Time { return now }
	f.app.Commerce.Now = f.app.Alerts.Now
	const quota = int64(9007199254740993)
	if _, err := f.db.DB.Exec(f.db.Rebind("UPDATE commerce_entitlements SET quota=?,used=? WHERE id=?"), quota, quota-12, f.ent.ID); err != nil {
		t.Fatal(err)
	}
	run := func() {
		t.Helper()
		if err := f.app.Alerts.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		run()
	}
	counts := func() map[string]int {
		t.Helper()
		events, err := f.app.Commerce.Events(ctx, f.owner.ID, 100)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]int{}
		for _, e := range events {
			out[e.Kind]++
			if e.Kind == "entitlement.low_quota" {
				var v struct {
					Data map[string]string `json:"data"`
				}
				if err := json.Unmarshal([]byte(e.Payload), &v); err != nil || v.Data["remaining_bytes"] != "12" || v.Data["quota_bytes"] != "9007199254740993" {
					t.Fatal(e, err)
				}
			}
		}
		return out
	}
	n := counts()
	if n["entitlement.expiring"] != 1 || n["entitlement.low_quota"] != 1 {
		t.Fatal(n)
	}
	now = f.ent.ExpiresAt
	run()
	run()
	n = counts()
	if n["entitlement.expired"] != 1 {
		t.Fatal(n)
	}
	f.ent = f.purchase("alert-new-cycle", 1)
	run()
	n = counts()
	if n["entitlement.expired"] != 1 {
		t.Fatal("new cycle inherited expired alert", n)
	}
	// Alert processing must never modify the purchased entitlement.
	if got, err := f.app.Commerce.Entitlement(ctx, f.owner.ID); err != nil || got.ID != f.ent.ID {
		t.Fatal(got, err)
	}
}
