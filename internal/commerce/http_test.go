package commerce

import (
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
