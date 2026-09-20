package payment

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
)

func (p *protocol) createTokenPay(ctx context.Context, r CreateRequest) (Created, error) {
	if r.UserKey == "" {
		r.UserKey = r.OrderID
	}
	m := map[string]any{"OutOrderId": r.OrderID, "OrderUserKey": r.UserKey, "ActualAmount": json.Number(money(r.AmountCents)), "Currency": p.cfg.CryptoCurrency, "NotifyUrl": r.NotifyURL, "RedirectUrl": r.ReturnURL}
	sig, e := sortedSign(m, "Signature", p.cfg.Key, p.cfg.SignatureAlgorithm, "exact", true)
	if e != nil {
		return Created{}, e
	}
	m["Signature"] = sig
	res, e := p.call(ctx, http.MethodPost, "/CreateOrder", nil, encode(m), "application/json", nil)
	if e != nil {
		return Created{}, e
	}
	if field(res, "success") != "true" {
		return Created{}, ErrProtocol
	}
	info, e := child(res, "info")
	if e != nil {
		return Created{}, e
	}
	c, e := parseCents(field(info, "ActualAmount"))
	if e != nil {
		return Created{}, e
	}
	if field(info, "BaseCurrency") != "CNY" || field(info, "OutOrderId") != r.OrderID || c != r.AmountCents || field(info, "Id") == "" || !validHTTPS(field(res, "data")) {
		return Created{}, ErrProtocol
	}
	return Created{OrderID: r.OrderID, TransactionID: field(info, "Id"), PaymentURL: field(res, "data"), Currency: "CNY", AmountCents: c}, nil
}
func (p *protocol) queryTokenPay(ctx context.Context, r QueryRequest) (Status, error) {
	if r.TransactionID == "" {
		return Status{}, ErrProtocol
	}
	m := map[string]any{"Id": r.TransactionID}
	sig, e := sortedSign(m, "Signature", p.cfg.Key, p.cfg.SignatureAlgorithm, "exact", true)
	if e != nil {
		return Status{}, e
	}
	res, e := p.call(ctx, http.MethodGet, "/Query", url.Values{"Id": {r.TransactionID}, "Signature": {sig}}, nil, "application/json", nil)
	if e != nil {
		return Status{}, e
	}
	if field(res, "success") != "true" {
		return Status{}, ErrProtocol
	}
	data, e := child(res, "data")
	if e != nil {
		return Status{}, e
	}
	s, e := p.tokenStatus(data)
	if e != nil {
		return Status{}, e
	}
	return s, matchQuery(s, r)
}
func (p *protocol) tokenStatus(m map[string]any) (Status, error) {
	if field(m, "BaseCurrency") != "CNY" || field(m, "Currency") != p.cfg.CryptoCurrency {
		return Status{}, ErrProtocol
	}
	c, e := parseCents(field(m, "ActualAmount"))
	if e != nil {
		return Status{}, e
	}
	s := Status{OrderID: field(m, "OutOrderId"), TransactionID: field(m, "Id"), Currency: "CNY", AmountCents: c}
	switch field(m, "Status") {
	case "0":
		s.State = Pending
	case "1":
		s.State = Paid
	case "2":
		s.State = Expired
	default:
		return Status{}, ErrProtocol
	}
	if s.OrderID == "" || s.TransactionID == "" {
		return Status{}, ErrProtocol
	}
	return s, nil
}
