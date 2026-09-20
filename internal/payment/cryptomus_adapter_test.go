package payment

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Independent cross-runtime fixture: generated using Node crypto MD5 and the
// exact documented PHP-compatible encoding, not this package's signing code.
const cryptoNotify = `{"type":"payment","uuid":"invoice-1","order_id":"order1","amount":"15.00","currency":"CNY","status":"paid","is_final":true,"additional_data":"中文/a","convert":{"to_currency":"USDT","commission":null},"sign":"9ccd6cb8f25b8220033ecfaddcd1b402"}`

func TestCryptomusIndependentVectorAndOrderedPHPEncoding(t *testing.T) {
	if got := cryptomusSign([]byte(`{"amount":"15.00","currency":"CNY","order_id":"order1"}`), "independent-test-key"); got != "b2a404cd68fcd04910ba950f0a8b4295" {
		t.Fatal(got)
	}
	p := newTestAdapter(t, Configuration{Kind: "cryptomus", MerchantID: "merchant-1", Key: "independent-test-key"})
	for i := 0; i < 2; i++ {
		s, e := p.VerifyNotify([]byte(cryptoNotify), "application/json")
		if e != nil || s.State != Paid || s.AmountCents != 1500 || s.TransactionID != "invoice-1" {
			t.Fatal(s, e)
		}
	}
	for _, raw := range []string{strings.Replace(cryptoNotify, `"CNY"`, `"USD"`, 1), strings.Replace(cryptoNotify, `"15.00"`, `"15.01"`, 1), strings.Replace(cryptoNotify, `"currency":"CNY"`, `"currency":"CNY","currency":"USD"`, 1), strings.Replace(cryptoNotify, `"type":"payment","uuid":"invoice-1"`, `"uuid":"invoice-1","type":"payment"`, 1)} {
		if _, e := p.VerifyNotify([]byte(raw), "application/json"); e == nil {
			t.Fatal("tampered or duplicate callback accepted")
		}
	}
	raw := strings.Replace(cryptoNotify, "中文/a", `\u4e2d\u6587\/a`, 1)
	if _, e := p.VerifyNotify([]byte(raw), "application/json"); e != nil {
		t.Fatal("PHP unicode/slash equivalence lost", e)
	}
}
func TestCryptomusTLSCreateQuery(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := mustRead(t, r.Body)
		if r.Method != "POST" || r.Header.Get("merchant") != "merchant-1" || r.Header.Get("sign") != cryptomusSign(body, "key") {
			t.Error("request auth mismatch")
		}
		m, e := object(body)
		if e != nil {
			t.Error(e)
		}
		if r.URL.Path == "/v1/payment" {
			if field(m, "currency") != "CNY" || field(m, "amount") != "15.00" || field(m, "accuracy_payment_percent") != "0" {
				t.Error("wrong invoice denomination")
			}
			io.WriteString(w, `{"state":0,"result":{"uuid":"invoice-1","order_id":"order1","amount":"15.00","currency":"CNY","url":"https://pay.cryptomus.com/pay/invoice-1","payment_status":"check"}}`)
			return
		}
		if r.URL.Path != "/v1/payment/info" || field(m, "order_id") != "order1" {
			t.Error("wrong query")
		}
		io.WriteString(w, `{"state":0,"result":{"uuid":"invoice-1","order_id":"order1","amount":"15.00000000","currency":"CNY","payment_status":"paid_over","is_final":true}}`)
	}))
	defer server.Close()
	p := newTestAdapter(t, Configuration{Kind: "cryptomus", MerchantID: "merchant-1", Key: "key", Client: server.Client()})
	p.gateway = server.URL // package-private injection, public configuration pins official host
	c, e := p.Create(context.Background(), CreateRequest{OrderID: "order1", Currency: "CNY", AmountCents: 1500, NotifyURL: "https://merchant.example/notify", ReturnURL: "https://merchant.example/return"})
	if e != nil || c.TransactionID != "invoice-1" {
		t.Fatal(c, e)
	}
	s, e := p.Query(context.Background(), QueryRequest{OrderID: "order1", TransactionID: "invoice-1"})
	if e != nil || s.State != Paid || s.AmountCents != 1500 {
		t.Fatal(s, e)
	}
	if _, e = NewAdapter(Configuration{Kind: "cryptomus", MerchantID: "m", Key: "k", Gateway: server.URL}); e == nil {
		t.Fatal("arbitrary Cryptomus endpoint accepted")
	}
}
func TestStrictMoneyAndCurrencies(t *testing.T) {
	for _, v := range []string{"-1", "1e2", "NaN", "0", "0.001", "9223372036854775808.00", "1/2", "1.2.3"} {
		if _, e := parseCents(v); e == nil {
			t.Fatal("bad money accepted", v)
		}
	}
	for _, v := range []string{"1", "1.00", "1.00000000"} {
		if c, e := parseCents(v); e != nil || c != 100 {
			t.Fatal(c, e)
		}
	}
	p := newTestAdapter(t, Configuration{Kind: "tokenpay", Gateway: "https://example.com", Key: "k", CryptoCurrency: "TRX"})
	for _, currency := range []string{"", "USD", "CNY"} {
		m := map[string]any{"BaseCurrency": currency, "Currency": "BTC", "ActualAmount": "1", "OutOrderId": "o", "Id": "i", "Status": "1"}
		if _, e := p.tokenStatus(m); e == nil {
			t.Fatal("wrong fiat/crypto currency accepted")
		}
	}
}
