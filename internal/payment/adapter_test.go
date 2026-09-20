package payment

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestVendorPublishedSignatureVectors(t *testing.T) {
	epusdt := map[string]any{"order_id": "20220201030210321", "amount": json.Number("42"), "notify_url": "http://example.com/notify", "redirect_url": "http://example.com/redirect"}
	for _, mode := range []string{"fixed", "general"} {
		sig, e := sortedSign(epusdt, "signature", "epusdt_password_xasddawqe", "md5", mode, true)
		if e != nil || sig != "1cd4b52df5587cfb1968b0c0c6e156cd" {
			t.Fatal("EPUSDT API.md published vector", sig, e)
		}
	}
	token := map[string]any{"OutOrderId": "AJIHK72N34BR2CWG", "OrderUserKey": "admin@qq.com", "ActualAmount": json.Number("15"), "Currency": "TRX", "NotifyUrl": "http://localhost:1011/pay/tokenpay/notify_url", "RedirectUrl": "http://localhost:1011/pay/tokenpay/return_url?order_id=AJIHK72N34BR2CWG"}
	for algorithm, want := range map[string]string{"md5": "e9765880db6081496456283678e70152", "hmac-sha256": "c879776795a9e85ce674aa10c8315de323d6f8a20bf157f92595b71ad77f1e12"} {
		sig, e := sortedSign(token, "Signature", "666", algorithm, "exact", true)
		if e != nil || sig != want {
			t.Fatal("TokenPay Wiki/docs.md create vector", algorithm, sig, e)
		}
	}
	sig, e := sortedSign(map[string]any{"Id": "66f9d5a8-d9c7-0224-004f-a16a1c068e08"}, "Signature", "666", "md5", "exact", true)
	if e != nil || sig != "baa261cc6af3f5efbed15e17a285f653" {
		t.Fatal("TokenPay published query vector", sig, e)
	}
}

const tokenNotifyVector = `{"ActualAmount":"15","Amount":"34.91","BaseCurrency":"CNY","BlockChainName":"TRON","BlockTransactionId":"375859c36dc5f5d227b10912b5ec70d36dd34446028064956cb60cdbb74432f5","Currency":"TRX","CurrencyName":"TRX","FromAddress":"TYYjzt6AWhe9hAg9DrhiYXEWKDksyohgQa","Id":"63234df7-55bf-93fc-0010-67be493c0c27","OutOrderId":"E6COE6FGZMO5AXSK","PayTime":"2022-09-15 16:08:39","Status":1,"ToAddress":"TKGTx4pCKiKQbk8evXHTborfZn754TGViP","Signature":"e5eaa888cd9e80b5c09a0698981757c8"}`

func newTestAdapter(t *testing.T, c Configuration) *protocol {
	t.Helper()
	a, e := NewAdapter(c)
	if e != nil {
		t.Fatal(e)
	}
	return a.(*protocol)
}
func TestTokenPayPublishedNotifyAndTamper(t *testing.T) {
	p := newTestAdapter(t, Configuration{Kind: "tokenpay", Gateway: "https://gateway.example", Key: "666", CryptoCurrency: "TRX"})
	for i := 0; i < 2; i++ {
		s, e := p.VerifyNotify([]byte(tokenNotifyVector), "application/json; charset=utf-8")
		if e != nil || s.State != Paid || s.AmountCents != 1500 || s.TransactionID != "63234df7-55bf-93fc-0010-67be493c0c27" {
			t.Fatal(s, e)
		}
		if e = ValidateExpected(s, "E6COE6FGZMO5AXSK", 1500, "CNY"); e != nil {
			t.Fatal(e)
		}
		if ValidateExpected(s, "other-order", 1500, "CNY") == nil || ValidateExpected(s, s.OrderID, 1600, "CNY") == nil || ValidateExpected(s, s.OrderID, 1500, "USD") == nil {
			t.Fatal("settlement binding bypass")
		}
	}
	for _, raw := range []string{strings.Replace(tokenNotifyVector, `"15"`, `"16"`, 1), strings.Replace(tokenNotifyVector, `"CNY"`, `"USD"`, 1), strings.Replace(tokenNotifyVector, `"Status":1`, `"Status":1,"Status":0`, 1)} {
		if _, e := p.VerifyNotify([]byte(raw), "application/json"); e == nil {
			t.Fatal("tamper accepted")
		}
	}
	p.cfg.SignatureAlgorithm = "hmac-sha256"
	if _, e := p.VerifyNotify([]byte(tokenNotifyVector), "application/json"); !errors.Is(e, ErrSignature) {
		t.Fatal("algorithm downgrade accepted", e)
	}
}
func TestUSDTCreateQueryAndNotifyTLS(t *testing.T) {
	for _, kind := range []string{"epusdt", "bepusdt"} {
		t.Run(kind, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				raw, _ := io.ReadAll(r.Body)
				m, e := object(raw)
				if e != nil {
					t.Error(e)
				}
				if r.URL.Path == "/api/v1/pay/info" {
					if kind != "bepusdt" || field(m, "trade_id") != "trade1" {
						t.Error("incorrect query")
					}
					io.WriteString(w, `{"status_code":200,"data":{"trade_id":"trade1","order_id":"order1","money":"12.34","fiat":"CNY","status":2}}`)
					return
				}
				wantPath := "/api/v1/order/create-transaction"
				if kind == "bepusdt" {
					wantPath = "/api/v1/order/create-order"
				}
				if r.Method != "POST" || r.URL.Path != wantPath || field(m, "order_id") != "order1" {
					t.Error("wrong create protocol")
				}
				io.WriteString(w, `{"status_code":200,"data":{"trade_id":"trade1","order_id":"order1","amount":"12.34","fiat":"CNY","payment_url":"https://checkout.example/pay"}}`)
			}))
			defer server.Close()
			p := newTestAdapter(t, Configuration{Kind: kind, Gateway: server.URL, Key: "secret", Client: server.Client()})
			created, e := p.Create(context.Background(), CreateRequest{OrderID: "order1", Currency: "CNY", AmountCents: 1234, NotifyURL: "https://merchant.example/notify", ReturnURL: "https://merchant.example/return"})
			if e != nil || created.TransactionID != "trade1" {
				t.Fatal(created, e)
			}
			q, e := p.Query(context.Background(), QueryRequest{OrderID: "order1", TransactionID: "trade1"})
			if kind == "epusdt" {
				if !errors.Is(e, ErrUnsupported) || p.Capabilities().Query {
					t.Fatal("invented legacy query", e)
				}
			} else if e != nil || q.State != Paid {
				t.Fatal(q, e)
			}
			notify := map[string]any{"trade_id": "trade1", "order_id": "order1", "amount": json.Number("12.34"), "actual_amount": "1.8", "status": json.Number("2"), "token": "address", "block_transaction_id": "hash"}
			mode := "fixed"
			skip := false
			if kind == "bepusdt" {
				mode = "general"
				skip = true
			}
			sig, _ := sortedSign(notify, "signature", "secret", "md5", mode, skip)
			notify["signature"] = sig
			s, e := p.VerifyNotify(encode(notify), "application/json")
			if e != nil || s.AmountCents != 1234 || s.State != Paid {
				t.Fatal(s, e)
			}
			notify["amount"] = json.Number("12.35")
			if _, e = p.VerifyNotify(encode(notify), "application/json"); e == nil {
				t.Fatal("changed amount accepted")
			}
		})
	}
}
func TestTokenPayTLSCreateAndQuery(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/CreateOrder":
			m, e := object(mustRead(t, r.Body))
			if e != nil || field(m, "ActualAmount") != "15.00" || field(m, "Currency") != "TRX" {
				t.Error(m, e)
			}
			io.WriteString(w, `{"success":true,"data":"https://checkout.example/pay","info":{"ActualAmount":"15","Amount":"99.1234","BaseCurrency":"CNY","Id":"66f9d5a8-d9c7-0224-004f-a16a1c068e08","OutOrderId":"order1"}}`)
		case "/Query":
			if r.URL.Query().Get("Signature") != "baa261cc6af3f5efbed15e17a285f653" {
				t.Error("query signature differs from vendor vector")
			}
			io.WriteString(w, `{"success":true,"data":{"Id":"66f9d5a8-d9c7-0224-004f-a16a1c068e08","OutOrderId":"order1","ActualAmount":"15","Amount":"99.1234","BaseCurrency":"CNY","Currency":"TRX","Status":1}}`)
		default:
			t.Error(r.URL.Path)
		}
	}))
	defer server.Close()
	p := newTestAdapter(t, Configuration{Kind: "tokenpay", Gateway: server.URL, Key: "666", CryptoCurrency: "TRX", Client: server.Client()})
	c, e := p.Create(context.Background(), CreateRequest{OrderID: "order1", Currency: "CNY", AmountCents: 1500, NotifyURL: "https://merchant.example/notify", ReturnURL: "https://merchant.example/return"})
	if e != nil || c.AmountCents != 1500 {
		t.Fatal(c, e)
	}
	s, e := p.Query(context.Background(), QueryRequest{OrderID: "order1", TransactionID: c.TransactionID})
	if e != nil || s.AmountCents != 1500 || s.State != Paid {
		t.Fatal(s, e)
	}
}
func mustRead(t *testing.T, r io.Reader) []byte {
	t.Helper()
	b, e := io.ReadAll(r)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func TestEPayTLSMAPIQueryAndNotify(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mapi.php" {
			if r.Method != "POST" {
				t.Error("mapi must POST")
			}
			r.ParseForm()
			if r.Form.Get("clientip") != "192.0.2.1" || r.Form.Get("sign") != EPaySign(r.Form, "secret") {
				t.Error("bad mapi fields")
			}
			io.WriteString(w, `{"code":1,"trade_no":"t1","payurl":"https://checkout.example/pay"}`)
			return
		}
		if r.URL.Path != "/api.php" || r.URL.Query().Get("act") != "order" || r.URL.Query().Get("key") != "secret" {
			t.Error("wrong epay query")
		}
		io.WriteString(w, `{"code":1,"pid":"1000","trade_no":"t1","out_trade_no":"o1","money":"12.34","status":1}`)
	}))
	defer server.Close()
	p := newTestAdapter(t, Configuration{Kind: "epay", Gateway: server.URL, Key: "secret", MerchantID: "1000", EPayMode: "mapi", Client: server.Client()})
	c, e := p.Create(context.Background(), CreateRequest{OrderID: "o1", Currency: "CNY", AmountCents: 1234, ClientIP: "192.0.2.1", NotifyURL: "https://merchant.example/notify", ReturnURL: "https://merchant.example/return"})
	if e != nil || c.TransactionID != "t1" {
		t.Fatal(c, e)
	}
	s, e := p.Query(context.Background(), QueryRequest{OrderID: "o1", TransactionID: "t1"})
	if e != nil || s.State != Paid {
		t.Fatal(s, e)
	}
	v := url.Values{"pid": {"1000"}, "out_trade_no": {"o1"}, "trade_no": {"t1"}, "trade_status": {"TRADE_SUCCESS"}, "money": {"12.34"}, "sign_type": {"MD5"}}
	v.Set("sign", EPaySign(v, "secret"))
	if _, e = p.VerifyNotify([]byte(v.Encode()), "application/x-www-form-urlencoded"); e != nil {
		t.Fatal(e)
	}
}
func TestHTTPBoundsRedirectAndTLSVerification(t *testing.T) {
	for _, mode := range []string{"oversized", "redirect", "untrusted", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "oversized":
					io.WriteString(w, strings.Repeat(" ", 1<<20+1))
				case "redirect":
					http.Redirect(w, r, "https://elsewhere.example/", 302)
				case "timeout":
					<-r.Context().Done()
				case "untrusted":
					io.WriteString(w, `{"code":1}`)
				}
			}))
			defer server.Close()
			client := server.Client()
			if mode == "untrusted" {
				client = nil
			}
			if mode == "timeout" {
				client.Timeout = 10 * time.Millisecond
			}
			p := newTestAdapter(t, Configuration{Kind: "epay", Gateway: server.URL, MerchantID: "1000", Key: "never-log-me", Client: client})
			_, e := p.Query(context.Background(), QueryRequest{OrderID: "o1"})
			if e == nil {
				t.Fatal("unsafe HTTP response accepted")
			}
			if strings.Contains(e.Error(), "never-log-me") {
				t.Fatal("key leaked through error")
			}
		})
	}
	if _, e := NewAdapter(Configuration{Kind: "epay", Gateway: "http://example.com", MerchantID: "1", Key: "secret"}); e == nil {
		t.Fatal("HTTP accepted")
	}
}
