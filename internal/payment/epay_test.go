package payment

import (
	"net/url"
	"testing"
)

func TestEPayVerification(t *testing.T) {
	e := EPay{PID: "merchant", Key: "secret"}
	v := url.Values{"pid": {"merchant"}, "out_trade_no": {"o1"}, "trade_no": {"t1"}, "trade_status": {"TRADE_SUCCESS"}, "money": {"12.34"}, "sign_type": {"MD5"}}
	v.Set("sign", EPaySign(v, e.Key))
	o, tx, amount, err := e.Verify(v)
	if err != nil || o != "o1" || tx != "t1" || amount != 1234 {
		t.Fatal(o, tx, amount, err)
	}
	v.Set("money", "99.99")
	if _, _, _, err = e.Verify(v); err == nil {
		t.Fatal("tampering accepted")
	}
	v.Set("sign", EPaySign(v, e.Key))
	v.Add("money", "99.99")
	if _, _, _, err = e.Verify(v); err == nil {
		t.Fatal("duplicate accepted")
	}
}
