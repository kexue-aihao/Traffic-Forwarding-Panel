package payment

import (
	"context"
	"encoding/json"
	"errors"
	"mime"
	"net"
	"net/http"
	"net/url"
)

func (p *protocol) createEPay(ctx context.Context, r CreateRequest) (Created, error) {
	method := r.Method
	if method == "" {
		method = "alipay"
	}
	v := url.Values{"pid": {p.cfg.MerchantID}, "type": {method}, "out_trade_no": {r.OrderID}, "notify_url": {r.NotifyURL}, "return_url": {r.ReturnURL}, "name": {r.Name}, "money": {money(r.AmountCents)}, "sign_type": {"MD5"}}
	if r.Name == "" {
		v.Set("name", "Wallet recharge")
	}
	if p.cfg.EPayMode == "mapi" {
		if net.ParseIP(r.ClientIP) == nil {
			return Created{}, errors.New("EPay mapi requires a valid client IP")
		}
		v.Set("clientip", r.ClientIP)
	}
	v.Set("sign", EPaySign(v, p.cfg.Key))
	created := Created{OrderID: r.OrderID, Currency: "CNY", AmountCents: r.AmountCents}
	if p.cfg.EPayMode == "submit" {
		created.PaymentURL = p.gateway + "/submit.php?" + v.Encode()
		return created, nil
	}
	m, e := p.call(ctx, http.MethodPost, "/mapi.php", nil, []byte(v.Encode()), "application/x-www-form-urlencoded", nil)
	if e != nil {
		return Created{}, e
	}
	if field(m, "code") != "1" {
		return Created{}, ErrProtocol
	}
	created.TransactionID = field(m, "trade_no")
	created.PaymentURL = field(m, "payurl")
	if created.PaymentURL == "" {
		created.PaymentURL = field(m, "qrcode")
	}
	if created.TransactionID == "" || !validHTTPS(created.PaymentURL) {
		return Created{}, ErrProtocol
	}
	return created, nil
}
func (p *protocol) queryEPay(ctx context.Context, r QueryRequest) (Status, error) {
	v := url.Values{"act": {"order"}, "pid": {p.cfg.MerchantID}, "key": {p.cfg.Key}}
	if r.TransactionID != "" {
		v.Set("trade_no", r.TransactionID)
	} else {
		v.Set("out_trade_no", r.OrderID)
	}
	m, e := p.call(ctx, http.MethodGet, "/api.php", v, nil, "application/json", nil)
	if e != nil {
		return Status{}, e
	}
	if field(m, "code") != "1" || field(m, "pid") != p.cfg.MerchantID {
		return Status{}, ErrProtocol
	}
	c, e := parseCents(field(m, "money"))
	if e != nil {
		return Status{}, e
	}
	s := Status{OrderID: field(m, "out_trade_no"), TransactionID: field(m, "trade_no"), Currency: "CNY", AmountCents: c, State: Pending}
	switch field(m, "status") {
	case "1":
		s.State = Paid
	case "0":
	default:
		return Status{}, ErrProtocol
	}
	if s.OrderID == "" || s.TransactionID == "" {
		return Status{}, ErrProtocol
	}
	return s, matchQuery(s, r)
}
func (p *protocol) notifyEPay(raw []byte, ct string) (Status, error) {
	mt, _, e := mime.ParseMediaType(ct)
	if e != nil || mt != "application/x-www-form-urlencoded" || len(raw) > 1<<20 {
		return Status{}, ErrProtocol
	}
	v, e := url.ParseQuery(string(raw))
	if e != nil {
		return Status{}, ErrProtocol
	}
	old := EPay{PID: p.cfg.MerchantID, Key: p.cfg.Key}
	o, t, c, e := old.Verify(v)
	if e != nil {
		return Status{}, e
	}
	return Status{OrderID: o, TransactionID: t, Currency: "CNY", AmountCents: c, State: Paid}, nil
}
func (p *protocol) createUSDT(ctx context.Context, r CreateRequest) (Created, error) {
	m := map[string]any{"order_id": r.OrderID, "amount": json.Number(money(r.AmountCents)), "notify_url": r.NotifyURL, "redirect_url": r.ReturnURL}
	path := "/api/v1/order/create-transaction"
	numbers := "fixed"
	skip := false
	if p.cfg.Kind == "bepusdt" {
		path = "/api/v1/order/create-order"
		numbers = "general"
		skip = true
		m["fiat"] = "CNY"
		if p.cfg.CryptoCurrency != "" {
			m["currencies"] = p.cfg.CryptoCurrency
		}
		if r.Name != "" {
			m["name"] = r.Name
		}
	}
	sig, e := sortedSign(m, "signature", p.cfg.Key, "md5", numbers, skip)
	if e != nil {
		return Created{}, e
	}
	m["signature"] = sig
	res, e := p.call(ctx, http.MethodPost, path, nil, encode(m), "application/json", nil)
	if e != nil {
		return Created{}, e
	}
	if field(res, "status_code") != "200" {
		return Created{}, ErrProtocol
	}
	data, e := child(res, "data")
	if e != nil {
		return Created{}, e
	}
	c, e := parseCents(field(data, "amount"))
	if e != nil {
		return Created{}, e
	}
	if field(data, "order_id") != r.OrderID || c != r.AmountCents || (p.cfg.Kind == "bepusdt" && field(data, "fiat") != "CNY") || field(data, "trade_id") == "" || !validHTTPS(field(data, "payment_url")) {
		return Created{}, ErrProtocol
	}
	return Created{OrderID: r.OrderID, TransactionID: field(data, "trade_id"), AmountCents: c, Currency: "CNY", PaymentURL: field(data, "payment_url")}, nil
}
func (p *protocol) queryUSDT(ctx context.Context, r QueryRequest) (Status, error) {
	if r.TransactionID == "" {
		return Status{}, errors.New("Bepusdt query requires trade ID")
	}
	m, e := p.call(ctx, http.MethodPost, "/api/v1/pay/info", nil, encode(map[string]any{"trade_id": r.TransactionID}), "application/json", nil)
	if e != nil {
		return Status{}, e
	}
	if field(m, "status_code") != "200" {
		return Status{}, ErrProtocol
	}
	data, e := child(m, "data")
	if e != nil {
		return Status{}, e
	}
	s, e := p.usdtStatus(data, true)
	if e != nil {
		return Status{}, e
	}
	return s, matchQuery(s, r)
}
func (p *protocol) usdtStatus(m map[string]any, query bool) (Status, error) {
	amountField := "amount"
	if query {
		amountField = "money"
		if field(m, "fiat") != "CNY" {
			return Status{}, ErrProtocol
		}
	}
	if fiat := field(m, "fiat"); fiat != "" && fiat != "CNY" {
		return Status{}, ErrProtocol
	}
	c, e := parseCents(field(m, amountField))
	if e != nil {
		return Status{}, e
	}
	s := Status{OrderID: field(m, "order_id"), TransactionID: field(m, "trade_id"), Currency: "CNY", AmountCents: c}
	switch field(m, "status") {
	case "1":
		s.State = Pending
	case "2":
		s.State = Paid
	case "3":
		s.State = Expired
	case "4":
		if p.cfg.Kind != "bepusdt" {
			return Status{}, ErrProtocol
		}
		s.State = Expired
	case "5":
		if p.cfg.Kind != "bepusdt" {
			return Status{}, ErrProtocol
		}
		s.State = Pending
	case "6":
		if p.cfg.Kind != "bepusdt" {
			return Status{}, ErrProtocol
		}
		s.State = Failed
	default:
		return Status{}, ErrProtocol
	}
	if s.OrderID == "" || s.TransactionID == "" {
		return Status{}, ErrProtocol
	}
	return s, nil
}
