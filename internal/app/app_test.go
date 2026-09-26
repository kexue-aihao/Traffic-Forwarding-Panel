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
