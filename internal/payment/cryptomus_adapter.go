package payment

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

func cryptomusSign(body []byte, key string) string {
	return md5hex(base64.StdEncoding.EncodeToString(body) + key)
}
func (p *protocol) cryptomusCall(ctx context.Context, path string, m map[string]any) (map[string]any, error) {
	body := encode(m)
	res, e := p.call(ctx, http.MethodPost, path, nil, body, "application/json", map[string]string{"merchant": p.cfg.MerchantID, "sign": cryptomusSign(body, p.cfg.Key)})
	if e != nil {
		return nil, e
	}
	if field(res, "state") != "0" {
		return nil, ErrProtocol
	}
	return child(res, "result")
}

var cryptomusOrder = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func (p *protocol) createCryptomus(ctx context.Context, r CreateRequest) (Created, error) {
	if !cryptomusOrder.MatchString(r.OrderID) {
		return Created{}, ErrProtocol
	}
	m := map[string]any{"amount": money(r.AmountCents), "currency": "CNY", "order_id": r.OrderID, "url_callback": r.NotifyURL, "url_return": r.ReturnURL, "accuracy_payment_percent": 0}
	if p.cfg.CryptoCurrency != "" {
		m["to_currency"] = p.cfg.CryptoCurrency
	}
	res, e := p.cryptomusCall(ctx, "/v1/payment", m)
	if e != nil {
		return Created{}, e
	}
	c, e := parseCents(field(res, "amount"))
	if e != nil {
		return Created{}, e
	}
	if field(res, "order_id") != r.OrderID || field(res, "currency") != "CNY" || c != r.AmountCents || field(res, "uuid") == "" || !validHTTPS(field(res, "url")) {
		return Created{}, ErrProtocol
	}
	return Created{OrderID: r.OrderID, TransactionID: field(res, "uuid"), PaymentURL: field(res, "url"), AmountCents: c, Currency: "CNY"}, nil
}
func (p *protocol) queryCryptomus(ctx context.Context, r QueryRequest) (Status, error) {
	m := map[string]any{}
	if r.OrderID != "" {
		m["order_id"] = r.OrderID
	} else {
		m["uuid"] = r.TransactionID
	}
	res, e := p.cryptomusCall(ctx, "/v1/payment/info", m)
	if e != nil {
		return Status{}, e
	}
	s, e := cryptomusStatus(res, true)
	if e != nil {
		return Status{}, e
	}
	return s, matchQuery(s, r)
}
func cryptomusStatus(m map[string]any, query bool) (Status, error) {
	if field(m, "currency") != "CNY" {
		return Status{}, ErrProtocol
	}
	if !query && field(m, "type") != "payment" {
		return Status{}, ErrProtocol
	}
	c, e := parseCents(field(m, "amount"))
	if e != nil {
		return Status{}, e
	}
	state := field(m, "status")
	if query && field(m, "payment_status") != "" {
		state = field(m, "payment_status")
	}
	s := Status{OrderID: field(m, "order_id"), TransactionID: field(m, "uuid"), Currency: "CNY", AmountCents: c}
	switch state {
	case "paid", "paid_over":
		s.State = Paid
	case "check", "confirm_check", "process", "wrong_amount_waiting":
		s.State = Pending
	case "cancel":
		s.State = Expired
	case "fail", "system_fail", "wrong_amount", "refund_process", "refund_fail", "refund_paid":
		s.State = Failed
	default:
		return Status{}, ErrProtocol
	}
	if s.OrderID == "" || s.TransactionID == "" {
		return Status{}, ErrProtocol
	}
	return s, nil
}
func (p *protocol) notifyCryptomus(raw []byte, m map[string]any) (Status, error) {
	canonical, e := phpNotifyJSON(raw)
	if e != nil {
		return Status{}, e
	}
	if !equalSign(cryptomusSign(canonical, p.cfg.Key), field(m, "sign")) {
		return Status{}, ErrSignature
	}
	return cryptomusStatus(m, false)
}

// phpNotifyJSON follows the documented PHP json_decode -> unset(sign) ->
// json_encode(JSON_UNESCAPED_UNICODE). Insertion order and escaped slashes matter.
func phpNotifyJSON(raw []byte) ([]byte, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var out bytes.Buffer
	if e := phpValue(d, &out, true); e != nil {
		return nil, e
	}
	if _, e := d.Token(); e != io.EOF {
		return nil, ErrProtocol
	}
	return out.Bytes(), nil
}
func phpString(v string) []byte { return []byte(strings.ReplaceAll(string(encode(v)), "/", `\/`)) }
func phpValue(d *json.Decoder, out *bytes.Buffer, root bool) error {
	t, e := d.Token()
	if e != nil {
		return ErrProtocol
	}
	if delim, ok := t.(json.Delim); ok {
		switch delim {
		case '{':
			out.WriteByte('{')
			seen := map[string]bool{}
			first := true
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return ErrProtocol
				}
				key, ok := k.(string)
				if !ok || seen[key] {
					return ErrProtocol
				}
				seen[key] = true
				var nested bytes.Buffer
				if e = phpValue(d, &nested, false); e != nil {
					return e
				}
				if root && key == "sign" {
					continue
				}
				if !first {
					out.WriteByte(',')
				}
				first = false
				out.Write(phpString(key))
				out.WriteByte(':')
				out.Write(nested.Bytes())
			}
			if _, e = d.Token(); e != nil {
				return ErrProtocol
			}
			out.WriteByte('}')
			return nil
		case '[':
			if root {
				return ErrProtocol
			}
			out.WriteByte('[')
			first := true
			for d.More() {
				if !first {
					out.WriteByte(',')
				}
				first = false
				if e = phpValue(d, out, false); e != nil {
					return e
				}
			}
			if _, e = d.Token(); e != nil {
				return ErrProtocol
			}
			out.WriteByte(']')
			return nil
		default:
			return ErrProtocol
		}
	}
	if root {
		return ErrProtocol
	}
	switch x := t.(type) {
	case string:
		out.Write(phpString(x))
	case json.Number:
		if i, e := strconv.ParseInt(string(x), 10, 64); e == nil {
			out.WriteString(strconv.FormatInt(i, 10))
		} else {
			f, e := strconv.ParseFloat(string(x), 64)
			if e != nil {
				return ErrProtocol
			}
			out.WriteString(strconv.FormatFloat(f, 'f', -1, 64))
		}
	case bool:
		out.WriteString(strconv.FormatBool(x))
	case nil:
		out.WriteString("null")
	default:
		return ErrProtocol
	}
	return nil
}
