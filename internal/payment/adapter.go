package payment

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var ErrUnsupported = errors.New("payment operation unsupported by selected protocol")
var ErrProtocol = errors.New("invalid payment protocol response")
var ErrSignature = errors.New("invalid payment signature")

type PaymentState string

const (
	Pending PaymentState = "pending"
	Paid    PaymentState = "paid"
	Expired PaymentState = "expired"
	Failed  PaymentState = "failed"
)

type CreateRequest struct {
	OrderID, UserKey, Name, Currency, NotifyURL, ReturnURL, ClientIP, Method string
	AmountCents                                                              int64
}
type QueryRequest struct{ OrderID, TransactionID string }
type Created struct {
	OrderID, TransactionID, PaymentURL, Currency string
	AmountCents                                  int64
}
type Status struct {
	OrderID, TransactionID, Currency string
	AmountCents                      int64
	State                            PaymentState
}
type Capabilities struct {
	Protocol              string
	Create, Query, Notify bool
	NotifyAck             string
}
type Adapter interface {
	Create(context.Context, CreateRequest) (Created, error)
	Query(context.Context, QueryRequest) (Status, error)
	VerifyNotify([]byte, string) (Status, error)
	Capabilities() Capabilities
}

// Configuration is operator-owned. URLs and secrets must never be accepted from
// an untrusted order request. Client supports custom trusted CAs, not TLS bypass.
type Configuration struct {
	Kind, Gateway, MerchantID, Key, CryptoCurrency, SignatureAlgorithm, EPayMode string
	Client                                                                       *http.Client
}
type protocol struct {
	cfg     Configuration
	client  *http.Client
	gateway string
}

func NewAdapter(c Configuration) (Adapter, error) {
	if c.Key == "" {
		return nil, errors.New("payment key required")
	}
	switch c.Kind {
	case "epay", "epusdt", "bepusdt", "tokenpay", "cryptomus":
	default:
		return nil, ErrUnsupported
	}
	if c.Kind == "cryptomus" {
		if c.Gateway != "" && strings.TrimRight(c.Gateway, "/") != "https://api.cryptomus.com" {
			return nil, errors.New("Cryptomus requires its official API endpoint")
		}
		c.Gateway = "https://api.cryptomus.com"
	}
	if !validHTTPS(c.Gateway) {
		return nil, errors.New("HTTPS payment gateway required")
	}
	u, _ := url.Parse(c.Gateway)
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("gateway cannot contain query or fragment")
	}
	if (c.Kind == "epay" || c.Kind == "cryptomus") && c.MerchantID == "" {
		return nil, errors.New("merchant identity required")
	}
	if c.Kind == "tokenpay" && c.CryptoCurrency == "" {
		return nil, errors.New("TokenPay payment currency required")
	}
	if c.SignatureAlgorithm == "" {
		c.SignatureAlgorithm = "md5"
	}
	if c.SignatureAlgorithm != "md5" && c.SignatureAlgorithm != "hmac-sha256" {
		return nil, errors.New("unsupported signature algorithm")
	}
	if c.Kind != "tokenpay" && c.SignatureAlgorithm != "md5" {
		return nil, ErrUnsupported
	}
	if c.EPayMode == "" {
		c.EPayMode = "submit"
	}
	if c.Kind == "epay" && c.EPayMode != "submit" && c.EPayMode != "mapi" {
		return nil, ErrUnsupported
	}
	client := http.Client{Timeout: 15 * time.Second}
	if c.Client != nil {
		client = *c.Client
		if client.Timeout <= 0 || client.Timeout > 15*time.Second {
			client.Timeout = 15 * time.Second
		}
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("payment redirects forbidden") }
	return &protocol{cfg: c, client: &client, gateway: strings.TrimRight(c.Gateway, "/")}, nil
}
func (p *protocol) Capabilities() Capabilities {
	return Capabilities{Protocol: p.cfg.Kind, Create: true, Query: p.cfg.Kind != "epusdt", Notify: true, NotifyAck: map[string]string{"epay": "success", "epusdt": "ok", "bepusdt": "success", "tokenpay": "ok", "cryptomus": "ok"}[p.cfg.Kind]}
}
func validHTTPS(raw string) bool {
	u, e := url.Parse(raw)
	return e == nil && u.Scheme == "https" && u.Host != "" && u.User == nil
}
func validateCreate(r CreateRequest) error {
	if r.OrderID == "" || len(r.OrderID) > 128 || r.AmountCents <= 0 || r.AmountCents > 999999999999 || r.Currency != "CNY" || !validHTTPS(r.NotifyURL) || !validHTTPS(r.ReturnURL) {
		return errors.New("order requires positive CNY amount and HTTPS callbacks")
	}
	return nil
}
func ValidateExpected(s Status, order string, cents int64, currency string) error {
	if s.OrderID != order || s.AmountCents != cents || s.Currency != currency || s.TransactionID == "" {
		return errors.New("payment order, amount or currency mismatch")
	}
	return nil
}
func money(c int64) string { return fmt.Sprintf("%d.%02d", c/100, c%100) }

var decimalPattern = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)?$`)

func parseCents(v string) (int64, error) {
	if len(v) > 64 || !decimalPattern.MatchString(v) {
		return 0, ErrProtocol
	}
	n, ok := new(big.Rat).SetString(v)
	if !ok {
		return 0, ErrProtocol
	}
	n.Mul(n, big.NewRat(100, 1))
	if !n.IsInt() || !n.Num().IsInt64() || n.Sign() <= 0 {
		return 0, ErrProtocol
	}
	return n.Num().Int64(), nil
}
func (p *protocol) call(ctx context.Context, method, path string, query url.Values, body []byte, contentType string, headers map[string]string) (map[string]any, error) {
	endpoint := p.gateway + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	req, e := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if e != nil {
		return nil, ErrProtocol
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, e := p.client.Do(req)
	if e != nil {
		return nil, errors.New("payment transport failed")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("payment gateway HTTP %d", res.StatusCode)
	}
	raw, e := io.ReadAll(io.LimitReader(res.Body, 1<<20+1))
	if e != nil || len(raw) > 1<<20 {
		return nil, ErrProtocol
	}
	return object(raw)
}
func object(raw []byte) (map[string]any, error) {
	if len(raw) > 1<<20 {
		return nil, ErrProtocol
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	v, e := readValue(d)
	if e != nil {
		return nil, e
	}
	if _, e = d.Token(); e != io.EOF {
		return nil, ErrProtocol
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, ErrProtocol
	}
	return m, nil
}
func readValue(d *json.Decoder) (any, error) {
	t, e := d.Token()
	if e != nil {
		return nil, ErrProtocol
	}
	if delim, ok := t.(json.Delim); ok {
		switch delim {
		case '{':
			m := map[string]any{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return nil, ErrProtocol
				}
				key, ok := k.(string)
				if !ok {
					return nil, ErrProtocol
				}
				if _, exists := m[key]; exists {
					return nil, ErrProtocol
				}
				v, e := readValue(d)
				if e != nil {
					return nil, e
				}
				m[key] = v
			}
			if _, e = d.Token(); e != nil {
				return nil, ErrProtocol
			}
			return m, nil
		case '[':
			a := []any{}
			for d.More() {
				v, e := readValue(d)
				if e != nil {
					return nil, e
				}
				a = append(a, v)
			}
			if _, e = d.Token(); e != nil {
				return nil, ErrProtocol
			}
			return a, nil
		default:
			return nil, ErrProtocol
		}
	}
	return t, nil
}
func scalar(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return string(x)
	case bool:
		return strconv.FormatBool(x)
	case nil:
		return ""
	}
	return ""
}
func field(m map[string]any, k string) string { return scalar(m[k]) }
func child(m map[string]any, k string) (map[string]any, error) {
	v, ok := m[k].(map[string]any)
	if !ok {
		return nil, ErrProtocol
	}
	return v, nil
}
func encode(v any) []byte {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	_ = e.Encode(v)
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}
func md5hex(s string) string { h := md5.Sum([]byte(s)); return hex.EncodeToString(h[:]) }
func equalSign(a, b string) bool {
	return a != "" && subtle.ConstantTimeCompare([]byte(strings.ToLower(a)), []byte(strings.ToLower(b))) == 1
}
func sortedSign(m map[string]any, omit, key, algorithm, numbers string, skipEmpty bool) (string, error) {
	keys := []string{}
	values := map[string]string{}
	for k, v := range m {
		if k == omit || v == nil {
			continue
		}
		s := scalar(v)
		switch v.(type) {
		case string, json.Number, bool:
		default:
			return "", ErrProtocol
		}
		if skipEmpty && s == "" {
			continue
		}
		if n, ok := v.(json.Number); ok && numbers != "exact" {
			f, e := strconv.ParseFloat(string(n), 64)
			if e != nil {
				return "", ErrProtocol
			}
			format := byte('f')
			if numbers == "general" {
				format = 'g'
			}
			s = strconv.FormatFloat(f, format, -1, 64)
		}
		keys = append(keys, k)
		values[k] = s
	}
	sort.Strings(keys)
	pairs := []string{}
	for _, k := range keys {
		pairs = append(pairs, k+"="+values[k])
	}
	canonical := strings.Join(pairs, "&")
	if algorithm == "hmac-sha256" {
		h := hmac.New(sha256.New, []byte(key))
		h.Write([]byte(canonical))
		return hex.EncodeToString(h.Sum(nil)), nil
	}
	return md5hex(canonical + key), nil
}
func contentJSON(ct string) bool {
	mt, _, e := mime.ParseMediaType(ct)
	return e == nil && mt == "application/json"
}
func (p *protocol) Create(ctx context.Context, r CreateRequest) (Created, error) {
	if e := validateCreate(r); e != nil {
		return Created{}, e
	}
	switch p.cfg.Kind {
	case "epay":
		return p.createEPay(ctx, r)
	case "epusdt", "bepusdt":
		return p.createUSDT(ctx, r)
	case "tokenpay":
		return p.createTokenPay(ctx, r)
	case "cryptomus":
		return p.createCryptomus(ctx, r)
	}
	return Created{}, ErrUnsupported
}
func (p *protocol) Query(ctx context.Context, r QueryRequest) (Status, error) {
	if r.OrderID == "" && r.TransactionID == "" {
		return Status{}, ErrProtocol
	}
	switch p.cfg.Kind {
	case "epay":
		return p.queryEPay(ctx, r)
	case "bepusdt":
		return p.queryUSDT(ctx, r)
	case "tokenpay":
		return p.queryTokenPay(ctx, r)
	case "cryptomus":
		return p.queryCryptomus(ctx, r)
	}
	return Status{}, ErrUnsupported
}
func (p *protocol) VerifyNotify(raw []byte, ct string) (Status, error) {
	if p.cfg.Kind == "epay" {
		return p.notifyEPay(raw, ct)
	}
	if !contentJSON(ct) {
		return Status{}, ErrProtocol
	}
	m, e := object(raw)
	if e != nil {
		return Status{}, e
	}
	switch p.cfg.Kind {
	case "epusdt", "bepusdt":
		mode := "fixed"
		skip := false
		if p.cfg.Kind == "bepusdt" {
			mode = "general"
			skip = true
		}
		sig, e := sortedSign(m, "signature", p.cfg.Key, "md5", mode, skip)
		if e != nil || !equalSign(sig, field(m, "signature")) {
			return Status{}, ErrSignature
		}
		return p.usdtStatus(m, false)
	case "tokenpay":
		sig, e := sortedSign(m, "Signature", p.cfg.Key, p.cfg.SignatureAlgorithm, "exact", true)
		if e != nil || !equalSign(sig, field(m, "Signature")) {
			return Status{}, ErrSignature
		}
		return p.tokenStatus(m)
	case "cryptomus":
		return p.notifyCryptomus(raw, m)
	}
	return Status{}, ErrUnsupported
}
func matchQuery(s Status, r QueryRequest) error {
	if r.OrderID != "" && s.OrderID != r.OrderID {
		return ErrProtocol
	}
	if r.TransactionID != "" && s.TransactionID != r.TransactionID {
		return ErrProtocol
	}
	return nil
}
