package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/commerce"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/testdb"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func telegramTestApp(t *testing.T) (*App, *http.Cookie) {
	t.Helper()
	a, err := New(context.Background(), testdb.Open(t), Options{Origin: "https://panel.example"})
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Platform.Bootstrap(context.Background(), "admin", "test-password-long"); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "https://panel.example/api/v1/auth/login", strings.NewReader("{\"username\":\"admin\",\"password\":\"test-password-long\"}"))
	r.Header.Set("X-Requested-With", "fetch")
	w := httptest.NewRecorder()
	a.Handler.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	return a, w.Result().Cookies()[0]
}
func TestTelegramSingleUseBindingAndLogin(t *testing.T) {
	a, cookie := telegramTestApp(t)
	ctx := context.Background()
	seed := func(raw, chat, user string) {
		t.Helper()
		_, e := a.store.DB.ExecContext(ctx, a.store.Rebind("INSERT INTO cp_telegram_login_tokens(token_hash,chat_id,user_id,expires_at,used) VALUES(?,?,?,?,0)"), telegramTokenHash(raw), chat, user, time.Now().Add(time.Minute).Unix())
		if e != nil {
			t.Fatal(e)
		}
	}
	request := func(method, raw, origin string, c *http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		body := url.Values{"token": {raw}}.Encode()
		r := httptest.NewRequest(method, "https://panel.example/api/v1/telegram/login?token="+raw, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", origin)
		if c != nil {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		a.Handler.ServeHTTP(w, r)
		return w
	}
	raw := strings.Repeat("a", 64)
	seed(raw, "101", "")
	if w := request("GET", strings.Repeat("b", 64), "", nil); w.Code != 410 {
		t.Fatal("invalid token accepted", w.Code)
	}
	if w := request("POST", raw, "https://panel.example", nil); w.Code != 401 {
		t.Fatal("unauthenticated binding", w.Code)
	}
	if w := request("POST", raw, "https://evil.example", cookie); w.Code != 403 {
		t.Fatal("cross-origin binding", w.Code)
	}
	if w := request("POST", raw, "https://panel.example", cookie); w.Code != 303 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request("POST", raw, "https://panel.example", cookie); w.Code != 410 {
		t.Fatal("token replay", w.Code)
	}
	raw = strings.Repeat("c", 64)
	seed(raw, "101", "bootstrap-admin")
	w := request("GET", raw, "", nil)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("handoff headers")
	}
	w = request("POST", raw, "https://panel.example", nil)
	if w.Code != 303 || len(w.Result().Cookies()) != 1 {
		t.Fatal("bound passwordless login failed", w.Code, w.Body.String())
	}
	raw = strings.Repeat("d", 64)
	seed(raw, "101", "bootstrap-admin")
	if _, e := a.store.DB.ExecContext(ctx, "UPDATE cp_users SET disabled=1 WHERE id='bootstrap-admin'"); e != nil {
		t.Fatal(e)
	}
	if w := request("POST", raw, "https://panel.example", nil); w.Code != 410 {
		t.Fatal("disabled account logged in")
	}
	var used int
	if e := a.store.DB.QueryRowContext(ctx, a.store.Rebind("SELECT used FROM cp_telegram_login_tokens WHERE token_hash=?"), telegramTokenHash(raw)).Scan(&used); e != nil || used != 0 {
		t.Fatal("failed login was not rolled back", e)
	}
}
func TestTelegramQuoteAndPrivateChat(t *testing.T) {
	for _, raw := range []string{"-1", "+1", "1.-1", "1.", "1e3", "0.001", "1000001"} {
		if _, e := parseTelegramAmount(raw); e == nil {
			t.Fatal("accepted", raw)
		}
	}
	for _, network := range TelegramNetworks {
		v, e := telegramCoinAmount(100, "3", network)
		if e != nil || !strings.HasPrefix(v, "0.33333") {
			t.Fatal(network, v, e)
		}
	}
	if _, e := telegramCoinAmount(100, "0", "trx"); e == nil {
		t.Fatal("zero rate accepted")
	}
	for _, entry := range []struct {
		payload string
		want    bool
	}{
		{"{\"message\":{\"chat\":{\"id\":1,\"type\":\"private\"},\"from\":{\"id\":1}}}", true},
		{"{\"message\":{\"chat\":{\"id\":-1,\"type\":\"group\"},\"from\":{\"id\":1}}}", false},
		{"{\"message\":{\"chat\":{\"id\":1,\"type\":\"private\"},\"from\":{\"id\":2}}}", false},
	} {
		var u telegramUpdate
		if e := json.Unmarshal([]byte(entry.payload), &u); e != nil {
			t.Fatal(e)
		}
		if telegramPrivateUpdate(u) != entry.want {
			t.Fatal(entry.payload)
		}
	}
}

func TestTelegramAccountQueries(t *testing.T) {
	a, _ := telegramTestApp(t)
	ctx := context.Background()
	if got := a.telegramAccountInfo(ctx, "101", "/balance"); !strings.Contains(got, "请先发送 /login") {
		t.Fatal(got)
	}
	if _, err := a.store.DB.ExecContext(ctx, "INSERT INTO cp_telegram_bindings(chat_id,user_id,updated_at) VALUES('101','bootstrap-admin',0)"); err != nil {
		t.Fatal(err)
	}
	if got := a.telegramAccountInfo(ctx, "101", "/traffic"); !strings.Contains(got, "暂无套餐流量") {
		t.Fatal(got)
	}
	if _, err := a.store.DB.ExecContext(ctx, "INSERT INTO commerce_wallets(user_id,balance,version) VALUES('bootstrap-admin',12345,0)"); err != nil {
		t.Fatal(err)
	}
	if got := a.telegramAccountInfo(ctx, "101", "/balance"); !strings.Contains(got, "123.45 元") || !strings.Contains(got, "admin") {
		t.Fatal(got)
	}
	plan, err := a.Commerce.CreatePlan(ctx, commerce.Plan{Name: "monthly", Price: 100, Quota: 2 << 30, Months: 1})
	if err != nil {
		t.Fatal(err)
	}
	ent, err := a.Commerce.Purchase(ctx, "bootstrap-admin", plan.ID, "telegram-query", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.DB.ExecContext(ctx, a.store.Rebind("UPDATE commerce_entitlements SET used=? WHERE id=?"), 1<<30, ent.ID); err != nil {
		t.Fatal(err)
	}
	if got := a.telegramAccountInfo(ctx, "101", "/traffic"); !strings.Contains(got, "1.00 GiB") || !strings.Contains(got, "2.00 GiB") || !strings.Contains(got, "有效") {
		t.Fatal(got)
	}
	if _, err := a.store.DB.ExecContext(ctx, "UPDATE cp_users SET disabled=1 WHERE id='bootstrap-admin'"); err != nil {
		t.Fatal(err)
	}
	if got := a.telegramAccountInfo(ctx, "101", "/balance"); !strings.Contains(got, "请先发送 /login") {
		t.Fatal(got)
	}
}
func TestTelegramPaymentReplayAndConfirmation(t *testing.T) {
	a, cookie := telegramTestApp(t)
	ctx := context.Background()
	if _, e := a.store.DB.ExecContext(ctx, "INSERT INTO cp_telegram_bindings(chat_id,user_id,updated_at) VALUES('101','bootstrap-admin',0)"); e != nil {
		t.Fatal(e)
	}
	// Enable funding using the real site settings API.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "https://panel.example/api/v1/site", nil)
	r.AddCookie(cookie)
	a.Handler.ServeHTTP(w, r)
	var site map[string]any
	if e := json.Unmarshal(w.Body.Bytes(), &site); e != nil {
		t.Fatal(e)
	}
	site["payments_enabled"] = true
	site["minimum_recharge"] = "0.01"
	site["maximum_recharge"] = "1000000"
	b, _ := json.Marshal(site)
	r = httptest.NewRequest("PUT", "https://panel.example/api/v1/site", strings.NewReader(string(b)))
	r.AddCookie(cookie)
	r.Header.Set("X-Requested-With", "fetch")
	w = httptest.NewRecorder()
	a.Handler.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	settings := TelegramSettings{WalletAddresses: map[string]string{"trx": "test-tron-address", "trc20-usdt": "test-tron-address"}, Rates: map[string]string{"trx": "1", "trc20-usdt": "7"}}
	for i := 0; i < 2; i++ {
		a.handleTelegramPay(ctx, nil, settings, "101", []string{"trx", "100"}, "bot-1", func(s string) {
			if strings.Contains(s, "失败") || strings.Contains(s, "无效") {
				t.Fatal(s)
			}
		})
	}
	var count int
	var id string
	if e := a.store.DB.QueryRowContext(ctx, "SELECT COUNT(*),MIN(id) FROM cp_telegram_payment_intents").Scan(&count, &id); e != nil || count != 1 {
		t.Fatal("duplicate intent", count, e)
	}
	confirm := func(id, body string) int {
		r := httptest.NewRequest("POST", "https://panel.example/api/v1/admin/telegram/payments/"+id+"/confirm", strings.NewReader(body))
		r.AddCookie(cookie)
		r.Header.Set("X-Requested-With", "fetch")
		w := httptest.NewRecorder()
		a.Handler.ServeHTTP(w, r)
		return w.Code
	}
	tx := strings.Repeat("a", 64)
	body := "{\"transaction_id\":\"" + tx + "\"}"
	if confirm(id, "{\"transaction_id\":\"\"}") != 400 || confirm(id, body+"{}") != 400 {
		t.Fatal("malformed confirmation accepted")
	}
	if confirm(id, body) != 200 || confirm(id, body) != 200 {
		t.Fatal("confirmation replay failed")
	}
	var balance int64
	if e := a.store.DB.QueryRowContext(ctx, "SELECT balance FROM commerce_wallets WHERE user_id='bootstrap-admin'").Scan(&balance); e != nil || balance != 10000 {
		t.Fatal(balance, e)
	}
	a.handleTelegramPay(ctx, nil, settings, "101", []string{"trc20-usdt", "100"}, "bot-2", func(s string) {})
	var next string
	if e := a.store.DB.QueryRowContext(ctx, a.store.Rebind("SELECT id FROM cp_telegram_payment_intents WHERE id<>?"), id).Scan(&next); e != nil {
		t.Fatal(e)
	}
	if confirm(next, body) != 409 {
		t.Fatal("chain transaction reused across tokens")
	}
	var status string
	if e := a.store.DB.QueryRowContext(ctx, a.store.Rebind("SELECT status FROM commerce_orders WHERE id=?"), next).Scan(&status); e != nil || status != "pending" {
		t.Fatal("failed confirmation changed order", status, e)
	}
	if e := a.store.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM commerce_events WHERE kind='payment.received'").Scan(&count); e != nil || count != 1 {
		t.Fatal("missing financial notification", count, e)
	}
}
func TestSavedEmptyPaymentsSurviveRestart(t *testing.T) {
	ctx := context.Background()
	st := testdb.Open(t)
	a, e := New(ctx, st, Options{Origin: "https://panel.example", PaymentConfigs: paymentFixtures(t)})
	if e != nil {
		t.Fatal(e)
	}
	saved, e := loadPaymentSettings(ctx, st)
	if e != nil {
		t.Fatal(e)
	}
	_, e = savePaymentSettings(ctx, st, saved.Version, PaymentSettings{Channels: map[string]PaymentConfiguration{}}, func(context.Context, *sql.Tx) error { return nil })
	if e != nil {
		t.Fatal(e)
	}
	a, e = New(ctx, st, Options{Origin: "https://panel.example", PaymentConfigs: paymentFixtures(t)})
	if e != nil {
		t.Fatal(e)
	}
	if len(a.Commerce.Channels()) != 0 {
		t.Fatal("startup resurrected removed channel")
	}
}
