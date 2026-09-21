package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/testdb"
)

func TestDomainFreeProxyLoginAndMutations(t *testing.T) {
	store := testdb.Open(t)
	a, err := New(context.Background(), store, Options{TrustProxy: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Platform.Bootstrap(context.Background(), "admin", "test-password-long"); err != nil {
		t.Fatal(err)
	}
	loginBody := `{"username":"admin","password":"test-password-long"}`
	request := func(handler http.Handler, host, origin, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest("POST", "http://"+host+"/api/v1"+path, strings.NewReader(body))
		r.Header.Set("Origin", origin)
		r.Header.Set("X-Forwarded-Proto", "https")
		r.Header.Set("X-Forwarded-Host", "attacker.example")
		r.Header.Set("X-Requested-With", "fetch")
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	for _, host := range []string{"first.example", "second.example:8443"} {
		origin := "https://" + host
		w := request(a.Handler, host, origin, "/auth/login", loginBody, nil)
		if w.Code != 200 || len(w.Result().Cookies()) != 1 || !w.Result().Cookies()[0].Secure {
			t.Fatalf("proxy login %s: %d %s", host, w.Code, w.Body.String())
		}
		cookie := w.Result().Cookies()[0]
		for _, path := range []string{"/auth/login", "/auto-renew"} {
			if denied := request(a.Handler, host, "https://attacker.example", path, loginBody, cookie); denied.Code != 403 {
				t.Fatalf("cross-site request accepted at %s: %d", path, denied.Code)
			}
		}
		w = request(a.Handler, host, origin, "/auto-renew", `{"enabled":false,"plan_id":""}`, cookie)
		if w.Code != 200 {
			t.Fatalf("commerce behind proxy: %d %s", w.Code, w.Body.String())
		}
		w = request(a.Handler, host, origin, "/auth/logout", `{}`, cookie)
		if w.Code != 204 || !w.Result().Cookies()[0].Secure || w.Result().Cookies()[0].MaxAge != -1 {
			t.Fatal("proxy logout did not clear secure cookie")
		}
	}
	direct, err := New(context.Background(), store, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if w := request(direct.Handler, "panel.example", "https://panel.example", "/auth/login", loginBody, nil); w.Code != 403 {
		t.Fatal("proxy protocol header trusted without explicit opt-in")
	}
	fixed, err := New(context.Background(), store, Options{TrustProxy: true, Origin: "https://fixed.example"})
	if err != nil {
		t.Fatal(err)
	}
	if w := request(fixed.Handler, "other.example", "https://other.example", "/auth/login", loginBody, nil); w.Code != 403 {
		t.Fatal("explicit canonical origin was bypassed")
	}
}
