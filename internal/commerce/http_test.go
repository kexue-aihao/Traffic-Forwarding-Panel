package commerce

import (
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
