package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type resetTransport struct {
	mu    sync.Mutex
	code  string
	sends int
	fail  bool
}

func (m *resetTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return &http.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader("{}")), Header: make(http.Header)}, nil
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return nil, err
	}
	m.code = regexp.MustCompile(`[0-9]{8}`).FindString(body.Text)
	m.sends++
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true}`)), Header: make(http.Header)}, nil
}

func setupPasswordReset(t *testing.T) (*App, *resetTransport, *http.Cookie) {
	t.Helper()
	a, cookie := telegramTestApp(t)
	settings := PaymentSettings{Channels: map[string]PaymentConfiguration{}, Telegram: TelegramSettings{BotToken: "123456:" + strings.Repeat("a", 24)}}
	if _, err := savePaymentSettings(context.Background(), a.store, 0, settings, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.DB.Exec("INSERT INTO cp_telegram_bindings(chat_id,user_id,updated_at) VALUES('101','bootstrap-admin',1)"); err != nil {
		t.Fatal(err)
	}
	m := &resetTransport{}
	a.telegramClient = &http.Client{Transport: m}
	return a, m, cookie
}

func resetCall(t *testing.T, a *App, path, body string) *httptest.ResponseRecorder {
	return resetCallFrom(t, a, path, body, "192.0.2.1:1234")
}

func resetCallFrom(t *testing.T, a *App, path, body, remoteAddr string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "https://panel.example/api/v1/auth/password-reset/"+path, strings.NewReader(body))
	r.RemoteAddr = remoteAddr
	r.Header.Set("X-Requested-With", "fetch")
	r.Header.Set("Origin", "https://panel.example")
	w := httptest.NewRecorder()
	a.Handler.ServeHTTP(w, r)
	return w
}

func TestTelegramPasswordResetLifecycle(t *testing.T) {
	a, transport, cookie := setupPasswordReset(t)
	if _, err := a.store.DB.Exec("INSERT INTO cp_tokens(id,token_hash,user_id,name,expires_at) VALUES('reset-token','test-hash','bootstrap-admin','test',0)"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.DB.Exec(a.store.Rebind("INSERT INTO cp_telegram_login_tokens(token_hash,chat_id,user_id,expires_at,used) VALUES('reset-link','101','bootstrap-admin',?,0)"), time.Now().Add(time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	request := `{"username":"admin"}`
	if w := resetCall(t, a, "request", request); w.Code != 200 || transport.sends != 1 || len(transport.code) != 8 {
		t.Fatalf("request: %d %s", w.Code, w.Body.String())
	}
	if w := resetCall(t, a, "request", request); w.Code != 200 || transport.sends != 1 {
		t.Fatal("send cooldown failed")
	}
	code := transport.code
	confirm := func(code string) *httptest.ResponseRecorder {
		return resetCall(t, a, "confirm", `{"username":"admin","code":"`+code+`","password":"replacement-password"}`)
	}
	if w := confirm("00000000"); w.Code != 400 {
		t.Fatal("wrong code accepted")
	}
	if w := confirm(code); w.Code != 204 {
		t.Fatalf("reset failed: %d %s", w.Code, w.Body.String())
	}
	for _, table := range []string{"cp_tokens", "cp_telegram_login_tokens"} {
		var count int
		if err := a.store.DB.QueryRow("SELECT COUNT(*) FROM " + table + " WHERE user_id='bootstrap-admin'").Scan(&count); err != nil || count != 0 {
			t.Fatal("credential survived reset", table, count, err)
		}
	}
	if w := confirm(code); w.Code != 400 {
		t.Fatal("code replay accepted")
	}
	r := httptest.NewRequest("GET", "https://panel.example/api/v1/auth/session", nil)
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	a.Handler.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("old session survived reset")
	}
	r = httptest.NewRequest("POST", "https://panel.example/api/v1/auth/login", strings.NewReader(`{"username":"admin","password":"replacement-password"}`))
	r.Header.Set("X-Requested-With", "fetch")
	w = httptest.NewRecorder()
	a.Handler.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("new password rejected: %d %s", w.Code, w.Body.String())
	}
}

func TestTelegramPasswordResetLimitsAndEligibility(t *testing.T) {
	a, transport, _ := setupPasswordReset(t)
	if w := resetCall(t, a, "request", `{"username":"unknown"}`); w.Code != 200 || transport.sends != 0 {
		t.Fatal("unknown account response")
	}
	if w := resetCall(t, a, "request", `{"username":"admin"}`); w.Code != 200 || transport.sends != 1 {
		t.Fatal("bound account request")
	}
	code := transport.code
	for i := 0; i < 5; i++ {
		w := resetCallFrom(t, a, "confirm", `{"username":"admin","code":"99999999","password":"replacement-password"}`, fmt.Sprintf("203.0.113.%d:1234", i+1))
		if w.Code != 400 {
			t.Fatal("invalid attempt", i, w.Code)
		}
	}
	if w := resetCallFrom(t, a, "confirm", `{"username":"admin","code":"`+code+`","password":"replacement-password"}`, "203.0.113.6:1234"); w.Code != 400 {
		t.Fatal("attempt limit bypass")
	}
	if _, err := a.store.DB.Exec(a.store.Rebind("UPDATE cp_telegram_password_resets SET attempts=0,expires_at=?"), time.Now().Add(-time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	if w := resetCallFrom(t, a, "confirm", `{"username":"admin","code":"`+code+`","password":"replacement-password"}`, "203.0.113.7:1234"); w.Code != 400 {
		t.Fatal("expired code accepted")
	}
	if _, err := a.store.DB.Exec(a.store.Rebind("UPDATE cp_telegram_password_resets SET last_sent_at=?"), time.Now().Add(-2*time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	transport.fail = true
	if w := resetCall(t, a, "request", `{"username":"admin"}`); w.Code != 200 {
		t.Fatal("send failure leaked")
	}
	var hash string
	if err := a.store.DB.QueryRow("SELECT code_hash FROM cp_telegram_password_resets WHERE user_id='bootstrap-admin'").Scan(&hash); err != nil || hash != "" {
		t.Fatal("failed send left usable code", err)
	}
	if _, err := a.store.DB.Exec("UPDATE cp_users SET disabled=1 WHERE id='bootstrap-admin'"); err != nil {
		t.Fatal(err)
	}
	transport.fail = false
	if w := resetCall(t, a, "request", `{"username":"admin"}`); w.Code != 200 || transport.sends != 1 {
		t.Fatal("disabled account received code")
	}
	if w := resetCallFrom(t, a, "confirm", `{"username":"admin","code":"`+code+`","password":"replacement-password"}`, "203.0.113.8:1234"); w.Code != 400 {
		t.Fatal("disabled account reset")
	}
	if _, err := a.store.DB.Exec("DELETE FROM cp_telegram_bindings WHERE user_id='bootstrap-admin'"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.DB.Exec("UPDATE cp_users SET disabled=0 WHERE id='bootstrap-admin'"); err != nil {
		t.Fatal(err)
	}
	if w := resetCall(t, a, "request", `{"username":"admin"}`); w.Code != 200 || transport.sends != 1 {
		t.Fatal("unbound account received code")
	}
}

func TestTelegramPasswordResetHourlyLimitSurvivesSuccess(t *testing.T) {
	a, transport, _ := setupPasswordReset(t)
	for i := 0; i < 5; i++ {
		if i > 0 {
			if _, err := a.store.DB.Exec(a.store.Rebind("UPDATE cp_telegram_password_resets SET last_sent_at=?"), time.Now().Add(-2*time.Minute).Unix()); err != nil {
				t.Fatal(err)
			}
		}
		if w := resetCallFrom(t, a, "request", `{"username":"admin"}`, fmt.Sprintf("198.51.100.%d:1234", i+1)); w.Code != 200 || transport.sends != i+1 {
			t.Fatal("allowed send failed", i, w.Code)
		}
		if i == 0 {
			w := resetCall(t, a, "confirm", `{"username":"admin","code":"`+transport.code+`","password":"replacement-password"}`)
			if w.Code != 204 {
				t.Fatal("first reset failed", w.Code, w.Body.String())
			}
		}
	}
	if _, err := a.store.DB.Exec(a.store.Rebind("UPDATE cp_telegram_password_resets SET last_sent_at=?"), time.Now().Add(-2*time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	if w := resetCallFrom(t, a, "request", `{"username":"admin"}`, "198.51.100.6:1234"); w.Code != 200 || transport.sends != 5 {
		t.Fatal("hourly send limit failed")
	}
}

func TestTelegramPasswordResetCSRF(t *testing.T) {
	a, _, _ := setupPasswordReset(t)
	r := httptest.NewRequest("POST", "https://panel.example/api/v1/auth/password-reset/request", strings.NewReader(`{"username":"admin"}`))
	r.Header.Set("X-Requested-With", "fetch")
	r.Header.Set("Origin", "https://other.example")
	w := httptest.NewRecorder()
	a.Handler.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("cross-origin request accepted")
	}
}

func TestTelegramPasswordResetIPLimitPerEndpointAndProxyIP(t *testing.T) {
	a, _, _ := setupPasswordReset(t)
	a.trustProxy = true

	request := func(remoteAddr, clientIP, username string) int {
		r := httptest.NewRequest("POST", "https://panel.example/api/v1/auth/password-reset/request", strings.NewReader(`{"username":"`+username+`"}`))
		r.RemoteAddr = remoteAddr
		r.Header.Set("X-Requested-With", "fetch")
		r.Header.Set("Origin", "https://panel.example")
		r.Header.Set("X-Real-IP", clientIP)
		w := httptest.NewRecorder()
		a.Handler.ServeHTTP(w, r)
		return w.Code
	}
	for i := 0; i < passwordResetIPLimit; i++ {
		if code := request(fmt.Sprintf("127.0.0.%d:1000", i+1), "198.51.100.20", fmt.Sprintf("missing-%d", i)); code != http.StatusOK {
			t.Fatalf("request %d: got %d", i, code)
		}
	}
	r := httptest.NewRequest("POST", "https://panel.example/api/v1/auth/password-reset/request", strings.NewReader(`{"username":"missing"}`))
	r.RemoteAddr = "127.0.0.99:1000"
	r.Header.Set("X-Requested-With", "fetch")
	r.Header.Set("Origin", "https://panel.example")
	r.Header.Set("X-Real-IP", "198.51.100.20")
	w := httptest.NewRecorder()
	a.Handler.ServeHTTP(w, r)
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != strconv.Itoa(int(passwordResetIPPeriod.Seconds())) {
		t.Fatalf("request IP limit: got %d retry-after=%q", w.Code, w.Header().Get("Retry-After"))
	}
	// A different trusted client IP and the confirm bucket are independent.
	if code := request("127.0.0.1:1000", "198.51.100.21", "missing-other-ip"); code != http.StatusOK {
		t.Fatalf("different IP request: got %d", code)
	}

	confirm := func(remoteAddr, clientIP string) int {
		r := httptest.NewRequest("POST", "https://panel.example/api/v1/auth/password-reset/confirm", strings.NewReader(`{"username":"missing","code":"00000000","password":"replacement-password"}`))
		r.RemoteAddr = remoteAddr
		r.Header.Set("X-Requested-With", "fetch")
		r.Header.Set("Origin", "https://panel.example")
		r.Header.Set("X-Real-IP", clientIP)
		w := httptest.NewRecorder()
		a.Handler.ServeHTTP(w, r)
		return w.Code
	}
	for i := 0; i < passwordResetIPLimit; i++ {
		if code := confirm(fmt.Sprintf("127.0.0.%d:2000", i+1), "198.51.100.20"); code != http.StatusBadRequest {
			t.Fatalf("confirm %d: got %d", i, code)
		}
	}
	if code := confirm("127.0.0.99:2000", "198.51.100.20"); code != http.StatusTooManyRequests {
		t.Fatalf("confirm IP limit: got %d", code)
	}
}

func TestTelegramPasswordResetIPLimitConcurrent(t *testing.T) {
	a, _, _ := setupPasswordReset(t)
	const total = passwordResetIPLimit * 4
	statuses := make(chan int, total)
	var wg sync.WaitGroup
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := httptest.NewRequest("POST", "https://panel.example/api/v1/auth/password-reset/request", strings.NewReader(fmt.Sprintf(`{"username":"missing-%d"}`, i)))
			r.RemoteAddr = "198.51.100.30:3000"
			r.Header.Set("X-Requested-With", "fetch")
			r.Header.Set("Origin", "https://panel.example")
			w := httptest.NewRecorder()
			a.Handler.ServeHTTP(w, r)
			statuses <- w.Code
		}(i)
	}
	wg.Wait()
	close(statuses)
	var allowed, limited int
	for code := range statuses {
		switch code {
		case http.StatusOK:
			allowed++
		case http.StatusTooManyRequests:
			limited++
		default:
			t.Fatalf("concurrent request status: %d", code)
		}
	}
	if allowed != passwordResetIPLimit || limited != total-passwordResetIPLimit {
		t.Fatalf("concurrent request limit: allowed=%d limited=%d", allowed, limited)
	}
}
