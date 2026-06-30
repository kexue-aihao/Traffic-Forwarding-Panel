package payment

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"trafficpanel/internal/domain"
)

type Provider interface {
	Code() string
	Name() string
	CreateOrder(ctx context.Context, order PaymentOrderInput) (PaymentOrderResult, error)
	ParseNotify(ctx context.Context, body []byte, form url.Values) (ProviderNotify, error)
}

type PaymentOrderInput struct {
	OrderNo     string
	AmountCents int64
	Subject     string
	NotifyURL   string
	ReturnURL   string
	Extra       map[string]string
}

type PaymentOrderResult struct {
	PayURL  string
	TradeNo string
	Raw     string
}

type ProviderNotify struct {
	OrderNo     string
	TradeNo     string
	AmountCents int64
	Status      domain.PaymentStatus
	Raw         string
}

type Registry struct {
	providers map[string]Provider
}

func NewRegistry(providers ...Provider) *Registry {
	items := make(map[string]Provider, len(providers))
	for _, provider := range providers {
		if provider == nil {
			continue
		}
		items[strings.ToLower(provider.Code())] = provider
	}
	return &Registry{providers: items}
}

func (r *Registry) Get(code string) (Provider, bool) {
	p, ok := r.providers[strings.ToLower(strings.TrimSpace(code))]
	return p, ok
}

type GenericRedirectProvider struct {
	code string
	name string
}

func NewGenericRedirectProvider(code, name string) *GenericRedirectProvider {
	return &GenericRedirectProvider{code: code, name: name}
}

func (p *GenericRedirectProvider) Code() string { return p.code }
func (p *GenericRedirectProvider) Name() string { return p.name }

func (p *GenericRedirectProvider) CreateOrder(_ context.Context, order PaymentOrderInput) (PaymentOrderResult, error) {
	if order.OrderNo == "" || order.AmountCents <= 0 {
		return PaymentOrderResult{}, errors.New("invalid order")
	}
	query := url.Values{}
	query.Set("order_no", order.OrderNo)
	query.Set("amount", fmt.Sprintf("%d", order.AmountCents))
	query.Set("subject", order.Subject)
	query.Set("notify_url", order.NotifyURL)
	query.Set("return_url", order.ReturnURL)
	for k, v := range order.Extra {
		if strings.TrimSpace(k) == "" || v == "" {
			continue
		}
		query.Set(k, v)
	}
	return PaymentOrderResult{
		PayURL:  "about:blank?" + query.Encode(),
		TradeNo: order.OrderNo,
		Raw:     query.Encode(),
	}, nil
}

func (p *GenericRedirectProvider) ParseNotify(_ context.Context, body []byte, form url.Values) (ProviderNotify, error) {
	orderNo := form.Get("order_no")
	tradeNo := form.Get("trade_no")
	if orderNo == "" {
		orderNo = form.Get("out_trade_no")
	}
	if tradeNo == "" {
		tradeNo = form.Get("trade_no")
	}
	status := domain.PaymentPending
	if strings.EqualFold(form.Get("status"), "success") || strings.EqualFold(form.Get("trade_status"), "success") || strings.EqualFold(form.Get("result"), "success") {
		status = domain.PaymentPaid
	}
	var raw strings.Builder
	raw.Write(body)
	if len(body) == 0 {
		encoded, _ := json.Marshal(form)
		raw.Write(encoded)
	}
	return ProviderNotify{
		OrderNo:     orderNo,
		TradeNo:     tradeNo,
		AmountCents: amountCentsFromForm(form),
		Status:      status,
		Raw:         raw.String(),
	}, nil
}

type SignedFormProvider struct {
	code    string
	name    string
	apiURL  string
	pid     string
	key     string
	payType string
}

func NewSignedFormProvider(code, name, apiURL, pid, key, payType string) *SignedFormProvider {
	return &SignedFormProvider{
		code:    strings.TrimSpace(code),
		name:    strings.TrimSpace(name),
		apiURL:  strings.TrimRight(strings.TrimSpace(apiURL), "/"),
		pid:     strings.TrimSpace(pid),
		key:     strings.TrimSpace(key),
		payType: strings.TrimSpace(payType),
	}
}

func (p *SignedFormProvider) Code() string { return p.code }
func (p *SignedFormProvider) Name() string { return p.name }

func (p *SignedFormProvider) CreateOrder(_ context.Context, order PaymentOrderInput) (PaymentOrderResult, error) {
	if p.apiURL == "" || p.pid == "" || p.key == "" {
		return PaymentOrderResult{}, fmt.Errorf("%s payment plugin is not configured", p.code)
	}
	if order.OrderNo == "" || order.AmountCents <= 0 {
		return PaymentOrderResult{}, errors.New("invalid order")
	}
	form := url.Values{}
	form.Set("pid", p.pid)
	form.Set("type", p.payType)
	form.Set("out_trade_no", order.OrderNo)
	form.Set("notify_url", order.NotifyURL)
	form.Set("return_url", order.ReturnURL)
	form.Set("name", order.Subject)
	form.Set("money", fmt.Sprintf("%.2f", float64(order.AmountCents)/100))
	for k, v := range order.Extra {
		if strings.TrimSpace(k) == "" || v == "" {
			continue
		}
		form.Set(k, v)
	}
	form.Set("sign", md5Sign(form, p.key))
	form.Set("sign_type", "MD5")
	return PaymentOrderResult{
		PayURL:  p.apiURL + "/submit.php?" + form.Encode(),
		TradeNo: order.OrderNo,
		Raw:     form.Encode(),
	}, nil
}

func (p *SignedFormProvider) ParseNotify(_ context.Context, body []byte, form url.Values) (ProviderNotify, error) {
	if p.key != "" {
		if form.Get("sign") == "" {
			return ProviderNotify{}, errors.New("missing payment signature")
		}
		if !strings.EqualFold(form.Get("sign"), md5Sign(form, p.key)) {
			return ProviderNotify{}, errors.New("invalid payment signature")
		}
	}
	status := domain.PaymentPending
	if strings.EqualFold(form.Get("trade_status"), "TRADE_SUCCESS") || strings.EqualFold(form.Get("trade_status"), "success") || strings.EqualFold(form.Get("status"), "success") {
		status = domain.PaymentPaid
	}
	raw := string(body)
	if raw == "" {
		encoded, _ := json.Marshal(form)
		raw = string(encoded)
	}
	return ProviderNotify{
		OrderNo:     firstNonEmpty(form.Get("out_trade_no"), form.Get("order_no")),
		TradeNo:     firstNonEmpty(form.Get("trade_no"), form.Get("transaction_id")),
		AmountCents: amountCentsFromForm(form),
		Status:      status,
		Raw:         raw,
	}, nil
}

func md5Sign(values url.Values, key string) string {
	keys := make([]string, 0, len(values))
	for k := range values {
		if k == "sign" || k == "sign_type" || values.Get(k) == "" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+values.Get(k))
	}
	sum := md5.Sum([]byte(strings.Join(parts, "&") + key))
	return fmt.Sprintf("%x", sum)
}

func amountCentsFromForm(form url.Values) int64 {
	for _, key := range []string{"amount_cents", "amount", "money", "total_fee"} {
		value := strings.TrimSpace(form.Get(key))
		if value == "" {
			continue
		}
		if key == "amount_cents" {
			cents, err := strconv.ParseInt(value, 10, 64)
			if err == nil && cents > 0 {
				return cents
			}
			continue
		}
		amount, err := strconv.ParseFloat(value, 64)
		if err == nil && amount > 0 {
			return int64(math.Round(amount * 100))
		}
	}
	return 0
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
