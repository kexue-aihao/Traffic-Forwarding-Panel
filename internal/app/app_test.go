package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/commerce"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/payment"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/testdb"
)

func TestLegacyPaymentAdapterAndWorkerShutdown(t *testing.T) {
	opts := Options{Origin: "https://panel.example", EPay: payment.EPay{Gateway: "https://pay.example", PID: "test-merchant", Key: "local-test-key"}}
	a, err := New(context.Background(), testdb.Open(t), opts)
	if err != nil {
		t.Fatal(err)
	}
	channel := a.channels["epay"]
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

func TestPaymentConfigurationPrecedenceAndURLValidation(t *testing.T) {
	for _, origin := range []string{"https://", "https:///missing-host", "https://user:secret@panel.example", "http://panel.example"} {
		if _, err := PaymentChannels(strings.NewReader(`{"epay":{"gateway":"https://pay.example","merchant_id":"test","key":"local-test-key"}}`), origin); err == nil {
			t.Fatalf("invalid public origin accepted: %q", origin)
		}
	}
	channels, err := PaymentChannels(strings.NewReader(`{"epay":{"gateway":"https://pay.example","merchant_id":"test","key":"local-test-key"}}`), "https://panel.example")
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(context.Background(), testdb.Open(t), Options{Channels: channels, EPay: payment.EPay{Gateway: "invalid", Key: "invalid"}})
	if err != nil {
		t.Fatal("legacy settings overrode explicit channel", err)
	}
	channels["epay"] = commerce.Channel{}
	if a.channels["epay"].Adapter == nil {
		t.Fatal("caller mutated live payment configuration")
	}
}
