package commerce

import (
	"context"
	"encoding/json"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPOriginAndTrailingJSON(t *testing.T) {
	s := fixture(t)
	mux := http.NewServeMux()
	s.Register(mux, HTTPOptions{Authenticate: func(*http.Request) (contract.User, error) { return contract.User{ID: "admin", Role: "admin"}, nil }})
	for _, tc := range []struct {
		origin, body string
		want         int
	}{{"https://example.test", `{"name":"p","price_cents":"100","quota_bytes":"1000","months":1}`, 403}, {"http://example.test", `{"name":"p","price_cents":"100","quota_bytes":"1000","months":1} {}`, 400}, {"http://example.test", `{"name":"p","price_cents":"100","quota_bytes":"1000","months":1}`, 200}} {
		r := httptest.NewRequest("POST", "http://example.test/api/v1/plans", strings.NewReader(tc.body))
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("X-Requested-With", "fetch")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("%d: %s", w.Code, w.Body.String())
		}
	}
}

func TestHTTPPlanDuration(t *testing.T) {
	s := fixture(t)
	mux := http.NewServeMux()
	s.Register(mux, HTTPOptions{Authenticate: func(*http.Request) (contract.User, error) { return contract.User{ID: "admin", Role: "admin"}, nil }})
	for _, tc := range []struct {
		unit, count string
		want        int
	}{
		{"day", "1", 200}, {"week", "2", 200}, {"month", "3", 200}, {"year", "1", 200},
		{"week", "1.5", 400}, {"year", "11", 400}, {"decade", "1", 400}, {"month", "0", 400},
	} {
		body := `{"name":"p","price_cents":"100","quota_bytes":"1000","duration_unit":"` + tc.unit + `","duration_value":` + tc.count + `}`
		r := httptest.NewRequest("POST", "http://example.test/api/v1/plans", strings.NewReader(body))
		r.Header.Set("Origin", "http://example.test")
		r.Header.Set("X-Requested-With", "fetch")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("%s %s: %d %s", tc.unit, tc.count, w.Code, w.Body.String())
		}
		if tc.want == 200 {
			var p Plan
			if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil || p.DurationUnit != tc.unit || p.DurationValue < 1 {
				t.Fatal(p, err)
			}
		}
	}
}

func TestHTTPPlanHumanUnitsAndUnlimitedQuota(t *testing.T) {
	s := fixture(t)
	price := "1"
	quota := "1"
	if _, err := s.CreatePlan(context.Background(), Plan{Name: "direct-addon", Kind: "addon", PriceYuan: &price, QuotaGB: &quota}); err != nil {
		t.Fatal("direct human addon:", err)
	}
	mux := http.NewServeMux()
	s.Register(mux, HTTPOptions{Authenticate: func(*http.Request) (contract.User, error) {
		return contract.User{ID: "admin", Role: "admin"}, nil
	}})
	for _, tc := range []struct {
		body  string
		want  int
		price int64
		quota int64
	}{
		{`{"name":"human","price_yuan":"12.50","quota_gb":"1.5","months":1}`, 200, 1250, 1610612736},
		{`{"name":"unlimited","price_yuan":"1","quota_gb":"","months":1}`, 200, 100, 0},
		{`{"name":"human addon","kind":"addon","price_yuan":"1","quota_gb":"1","duration_unit":"","duration_value":0}`, 200, 100, 1073741824},
		{`{"name":"bad price","price_yuan":"0.001","quota_gb":"1","months":1}`, 400, 0, 0},
		{`{"name":"bad quota","price_yuan":"1","quota_gb":"0.0000000001","months":1}`, 400, 0, 0},
	} {
		r := httptest.NewRequest("POST", "http://example.test/api/v1/plans", strings.NewReader(tc.body))
		r.Header.Set("Origin", "http://example.test")
		r.Header.Set("X-Requested-With", "fetch")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("%s: %d %s", tc.body, w.Code, w.Body.String())
		}
		if tc.want == 200 {
			var p Plan
			if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil || p.Price != tc.price || p.Quota != tc.quota {
				t.Fatal(p, err)
			}
		}
	}
}
