package app

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/payment"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/testdb"
)

func TestLegacyPaymentAdapterAndWorkerShutdown(t *testing.T) {
	opts := Options{Origin: "https://panel.example", EPay: payment.EPay{Gateway: "https://pay.example", PID: "test-merchant", Key: "local-test-key"}}
	a, err := New(context.Background(), testdb.Open(t), opts)
	if err != nil {
		t.Fatal(err)
	}
	channel := a.Commerce.Channels()["epay"]
	if channel.Adapter == nil || !channel.Adapter.Capabilities().Query || channel.NotifyURL != "https://panel.example/api/v1/payments/epay/notify" || channel.ReturnURL != "https://panel.example/#/commerce" {
		t.Fatal("legacy EPay not connected to common adapter")
	}
	// A canceled service never reaches a configured external gateway.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() { a.RunBackground(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("background workers did not stop")
	}
}

func paymentFixtures(t *testing.T) map[string]PaymentConfiguration {
	t.Helper()
	parsed, err := ParsePaymentConfigs(strings.NewReader(`{"epay":{"gateway":"https://pay.example","merchant_id":"test","key":"local-test-key"}}`))
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestPaymentConfigurationPrecedenceAndURLValidation(t *testing.T) {
	for _, origin := range []string{"https://", "https:///missing-host", "https://user:secret@panel.example", "http://panel.example"} {
		if _, err := BuildPaymentChannels(paymentFixtures(t), origin); err == nil {
			t.Fatalf("invalid public origin accepted: %q", origin)
		}
	}
	configs := paymentFixtures(t)
	a, err := New(context.Background(), testdb.Open(t), Options{Origin: "https://panel.example", PaymentConfigs: configs, EPay: payment.EPay{Gateway: "invalid", Key: "invalid"}})
	if err != nil {
		t.Fatal("legacy settings overrode explicit channel", err)
	}
	// 启动参数里的那份配置只是初始值：调用方之后改自己那份，不能影响已生效的通道。
	configs["epay"] = PaymentConfiguration{}
	if a.Commerce.Channels()["epay"].Adapter == nil {
		t.Fatal("caller mutated live payment configuration")
	}
}

// 面板上保存的通道配置要立刻生效，并且密钥永远不回明文。
//
// 处理函数直接调，不套鉴权中间件：这里验的是配置本身的读写与生效，鉴权与 CSRF
// 由 platform.Admin 统一负责，浏览器用例走的是那条完整链路。
func TestPaymentSettingsRoundTripAndRedaction(t *testing.T) {
	a, err := New(context.Background(), testdb.Open(t), Options{Origin: "https://panel.example", PaymentConfigs: paymentFixtures(t)})
	if err != nil {
		t.Fatal(err)
	}
	view, err := a.paymentSettingsView(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// 第一次启动会把启动参数里那份写进数据库：面板上看到的就是生效的那一份。
	if !view.Channels["epay"].Configured || !view.Channels["epay"].KeySet {
		t.Fatalf("启动配置没有落到数据库: %+v", view.Channels["epay"])
	}
	if body, _ := json.Marshal(view); strings.Contains(string(body), "local-test-key") {
		t.Fatal("支付密钥回显到了接口响应里")
	}
	if view.Channels["epay"].Gateway != "https://pay.example" {
		t.Fatalf("网关地址没有回显: %+v", view.Channels["epay"])
	}

	// 保存一条新通道：密钥留空表示沿用已存下来的那一把。
	next := PaymentSettingsView{Version: view.Version, Channels: map[string]PaymentChannelView{
		"epay":     view.Channels["epay"],
		"tokenpay": {PaymentConfiguration: PaymentConfiguration{Gateway: "https://tokenpay.example", Key: "tp-key", CryptoCurrency: "USDT"}, Configured: true, KeySet: true},
	}}
	body, _ := json.Marshal(next)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest("PUT", "https://panel.example/api/v1/payment-settings", strings.NewReader(string(body)))
	a.putPaymentSettings(recorder, request)
	if recorder.Code != 200 {
		t.Fatalf("保存通道配置失败: %d %s", recorder.Code, recorder.Body.String())
	}
	channels := a.Commerce.Channels()
	if channels["epay"].Adapter == nil || channels["tokenpay"].Adapter == nil {
		t.Fatal("保存之后没有立刻生效")
	}
	if channels["epay"].NotifyURL != "https://panel.example/api/v1/payments/epay/notify" {
		t.Fatalf("回调地址不对: %s", channels["epay"].NotifyURL)
	}

	// 校验不过的配置什么都不改：生效的还是上一份。
	broken := PaymentSettingsView{Version: view.Version + 1, Channels: map[string]PaymentChannelView{
		"epay": {PaymentConfiguration: PaymentConfiguration{Gateway: "http://plain.example", Key: "x"}, Configured: true},
	}}
	body, _ = json.Marshal(broken)
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest("PUT", "https://panel.example/api/v1/payment-settings", strings.NewReader(string(body)))
	a.putPaymentSettings(recorder, request)
	if recorder.Code != 400 {
		t.Fatalf("非 HTTPS 网关被接受: %d %s", recorder.Code, recorder.Body.String())
	}
	if a.Commerce.Channels()["tokenpay"].Adapter == nil {
		t.Fatal("失败的保存动到了已生效的通道")
	}

	// 清空一条通道等于不再使用它。
	empty := PaymentSettingsView{Version: view.Version + 1, Channels: map[string]PaymentChannelView{}}
	body, _ = json.Marshal(empty)
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest("PUT", "https://panel.example/api/v1/payment-settings", strings.NewReader(string(body)))
	a.putPaymentSettings(recorder, request)
	if recorder.Code != 200 {
		t.Fatalf("清空配置失败: %d %s", recorder.Code, recorder.Body.String())
	}
	if len(a.Commerce.Channels()) != 0 {
		t.Fatalf("清空之后仍然有通道生效: %v", a.Commerce.Channels())
	}
}

func TestPaymentSettingsConflictRestoresPersistedChannels(t *testing.T) {
	a, err := New(context.Background(), testdb.Open(t), Options{Origin: "https://panel.example", PaymentConfigs: paymentFixtures(t)})
	if err != nil {
		t.Fatal(err)
	}
	view, err := a.paymentSettingsView(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// Simulate another panel instance committing after this editor loaded its version.
	winning := PaymentSettings{Channels: map[string]PaymentConfiguration{
		"epay": {Gateway: "https://winner.example", MerchantID: "winner", Key: "winner-key", FeePercent: "1"},
	}, Telegram: TelegramSettings{WalletAddresses: map[string]string{}}}
	if _, err := savePaymentSettings(context.Background(), a.store, view.Version, winning, nil); err != nil {
		t.Fatal(err)
	}

	stale := PaymentSettingsView{Version: view.Version, Channels: map[string]PaymentChannelView{
		"epay": {PaymentConfiguration: PaymentConfiguration{Gateway: "https://stale.example", MerchantID: "stale", Key: "stale-key", FeePercent: "2"}},
	}}
	body, _ := json.Marshal(stale)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest("PUT", "https://panel.example/api/v1/payment-settings", strings.NewReader(string(body)))
	a.putPaymentSettings(recorder, request)
	if recorder.Code != 409 {
		t.Fatalf("stale save status: %d %s", recorder.Code, recorder.Body.String())
	}
	if got := a.Commerce.Channels()["epay"].FeeBPS; got != 100 {
		t.Fatalf("live channels reverted to stale config: fee=%d", got)
	}
}

func TestPaymentSettingsProtectActiveOrderChannel(t *testing.T) {
	ctx := context.Background()
	a, err := New(ctx, testdb.Open(t), Options{Origin: "https://panel.example", PaymentConfigs: paymentFixtures(t)})
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := a.store.DB.ExecContext(ctx, a.store.Rebind(`INSERT INTO commerce_orders(id,user_id,channel,amount,payable_cents,status,payment_url,created_at,idempotency_key) VALUES(?,?,?,?,?,'pending','',?,?)`), "active-order", "alice", "epay", 1000, 1000, stamp, "active-key"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.DB.ExecContext(ctx, a.store.Rebind(`INSERT INTO commerce_attempts(order_id,state,provider_id,updated_at) VALUES(?,'ready','',?)`), "active-order", stamp); err != nil {
		t.Fatal(err)
	}
	view, err := a.paymentSettingsView(ctx)
	if err != nil {
		t.Fatal(err)
	}
	put := func(in PaymentSettingsView) *httptest.ResponseRecorder {
		body, _ := json.Marshal(in)
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest("PUT", "https://panel.example/api/v1/payment-settings", strings.NewReader(string(body)))
		a.putPaymentSettings(recorder, req)
		return recorder
	}
	rotated := view
	ep := view.Channels["epay"]
	ep.Key = "rotated-key"
	rotated.Channels = map[string]PaymentChannelView{"epay": ep}
	if response := put(rotated); response.Code != 409 {
		t.Fatalf("key rotation with active order status: %d %s", response.Code, response.Body.String())
	}
	changedGateway := view
	ep = view.Channels["epay"]
	ep.Gateway = "https://new-pay.example"
	changedGateway.Channels = map[string]PaymentChannelView{"epay": ep}
	if response := put(changedGateway); response.Code != 409 {
		t.Fatalf("gateway rotation with active order status: %d %s", response.Code, response.Body.String())
	}
	if response := put(PaymentSettingsView{Version: view.Version, Channels: map[string]PaymentChannelView{}}); response.Code != 409 {
		t.Fatalf("channel deletion with active order status: %d %s", response.Code, response.Body.String())
	}
	if _, err := a.store.DB.ExecContext(ctx, a.store.Rebind(`UPDATE commerce_orders SET status='paid' WHERE id=?`), "active-order"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.DB.ExecContext(ctx, a.store.Rebind(`UPDATE commerce_attempts SET state='ready' WHERE order_id=?`), "active-order"); err != nil {
		t.Fatal(err)
	}
	if response := put(PaymentSettingsView{Version: view.Version, Channels: map[string]PaymentChannelView{}}); response.Code != 200 {
		t.Fatalf("channel deletion after terminal order status: %d %s", response.Code, response.Body.String())
	}
}

func TestPersistedPaymentSettingsDisableLegacyEPayFallback(t *testing.T) {
	ctx := context.Background()
	store := testdb.Open(t)
	legacy := payment.EPay{Gateway: "https://legacy.example", PID: "legacy", Key: "legacy-key"}
	a, err := New(ctx, store, Options{Origin: "https://panel.example", EPay: legacy})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Platform.Bootstrap(ctx, "admin", "test-password-long"); err != nil {
		t.Fatal(err)
	}
	settings, err := loadPaymentSettings(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := savePaymentSettings(ctx, store, settings.Version, PaymentSettings{Channels: map[string]PaymentConfiguration{}, Telegram: settings.Telegram}, nil); err != nil {
		t.Fatal(err)
	}

	// A restart must honor the persisted empty channel set even when the old
	// environment-backed EPay settings are still present.
	a, err = New(ctx, store, Options{Origin: "https://panel.example", EPay: legacy})
	if err != nil {
		t.Fatal(err)
	}
	loginBody, _ := json.Marshal(map[string]string{"username": "admin", "password": "test-password-long"})
	login := httptest.NewRecorder()
	loginRequest := httptest.NewRequest("POST", "https://panel.example/api/v1/auth/login", strings.NewReader(string(loginBody)))
	loginRequest.Header.Set("Origin", "https://panel.example")
	loginRequest.Header.Set("X-Requested-With", "fetch")
	a.Handler.ServeHTTP(login, loginRequest)
	if login.Code != 200 || len(login.Result().Cookies()) != 1 {
		t.Fatalf("login failed: %d %s", login.Code, login.Body.String())
	}
	request := httptest.NewRequest("GET", "https://panel.example/api/v1/payment-channels", nil)
	request.AddCookie(login.Result().Cookies()[0])
	response := httptest.NewRecorder()
	a.Handler.ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatalf("payment channels status: %d %s", response.Code, response.Body.String())
	}
	var body struct {
		Items []struct {
			ID      string `json:"id"`
			Enabled bool   `json:"enabled"`
		} `json:"items"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, channel := range body.Items {
		if channel.ID == "epay" && channel.Enabled {
			t.Fatal("legacy EPay fallback remained enabled after persisted channel deletion")
		}
	}
}

func TestDynamicPaymentSettingsDeleteDisablesLegacyEPayFallback(t *testing.T) {
	ctx := context.Background()
	legacy := payment.EPay{Gateway: "https://legacy.example", PID: "legacy", Key: "legacy-key"}
	store := testdb.Open(t)
	a, err := New(ctx, store, Options{Origin: "https://panel.example", EPay: legacy})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Platform.Bootstrap(ctx, "admin", "test-password-long"); err != nil {
		t.Fatal(err)
	}
	view, err := a.paymentSettingsView(ctx)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(PaymentSettingsView{Version: view.Version, Channels: map[string]PaymentChannelView{}})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest("PUT", "https://panel.example/api/v1/payment-settings", strings.NewReader(string(body)))
	a.putPaymentSettings(recorder, request)
	if recorder.Code != 200 || len(a.Commerce.Channels()) != 0 {
		t.Fatalf("dynamic channel deletion failed: %d %s", recorder.Code, recorder.Body.String())
	}
	loginBody, _ := json.Marshal(map[string]string{"username": "admin", "password": "test-password-long"})
	login := httptest.NewRecorder()
	loginRequest := httptest.NewRequest("POST", "https://panel.example/api/v1/auth/login", strings.NewReader(string(loginBody)))
	loginRequest.Header.Set("Origin", "https://panel.example")
	loginRequest.Header.Set("X-Requested-With", "fetch")
	a.Handler.ServeHTTP(login, loginRequest)
	if login.Code != 200 || len(login.Result().Cookies()) != 1 {
		t.Fatalf("login failed: %d %s", login.Code, login.Body.String())
	}
	channelsRequest := httptest.NewRequest("GET", "https://panel.example/api/v1/payment-channels", nil)
	channelsRequest.AddCookie(login.Result().Cookies()[0])
	channels := httptest.NewRecorder()
	a.Handler.ServeHTTP(channels, channelsRequest)
	if channels.Code != 200 {
		t.Fatalf("payment channels status: %d %s", channels.Code, channels.Body.String())
	}
	var response struct {
		Items []struct {
			ID      string `json:"id"`
			Enabled bool   `json:"enabled"`
		} `json:"items"`
	}
	if err := json.Unmarshal(channels.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	for _, channel := range response.Items {
		if channel.ID == "epay" && channel.Enabled {
			t.Fatal("legacy EPay fallback remained enabled after dynamic channel deletion")
		}
	}
}
