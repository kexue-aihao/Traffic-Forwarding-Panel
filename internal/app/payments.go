package app

import (
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/commerce"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/payment"
)

type PaymentConfiguration struct {
	Gateway            string `json:"gateway"`
	MerchantID         string `json:"merchant_id"`
	Key                string `json:"key"`
	CryptoCurrency     string `json:"crypto_currency"`
	SignatureAlgorithm string `json:"signature_algorithm"`
	EPayMode           string `json:"epay_mode"`
	Method             string `json:"method"`
	NotifyURL          string `json:"notify_url"`
	ReturnURL          string `json:"return_url"`
}

// PaymentChannels loads operator-owned settings. No config secrets are returned
// by public channel or order APIs, nor included in validation errors.
func PaymentChannels(r io.Reader, origin string) (map[string]commerce.Channel, error) {
	d := json.NewDecoder(io.LimitReader(r, 1<<20))
	d.DisallowUnknownFields()
	var input map[string]PaymentConfiguration
	if err := d.Decode(&input); err != nil {
		return nil, errors.New("invalid payment configuration JSON")
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, errors.New("payment configuration requires one JSON object")
	}
	result := map[string]commerce.Channel{}
	for name, cfg := range input {
		adapter, err := payment.NewAdapter(payment.Configuration{Kind: name, Gateway: cfg.Gateway, MerchantID: cfg.MerchantID, Key: cfg.Key, CryptoCurrency: cfg.CryptoCurrency, SignatureAlgorithm: cfg.SignatureAlgorithm, EPayMode: cfg.EPayMode})
		if err != nil {
			return nil, errors.New("invalid payment configuration for " + name)
		}
		if cfg.NotifyURL == "" {
			cfg.NotifyURL = strings.TrimRight(origin, "/") + "/api/v1/payments/" + name + "/notify"
		}
		if cfg.ReturnURL == "" {
			cfg.ReturnURL = strings.TrimRight(origin, "/") + "/#/commerce"
		}
		if !validPaymentURL(cfg.NotifyURL, false) || !validPaymentURL(cfg.ReturnURL, true) {
			return nil, errors.New("public HTTPS payment URLs required for " + name)
		}
		result[name] = commerce.Channel{Adapter: adapter, NotifyURL: cfg.NotifyURL, ReturnURL: cfg.ReturnURL, Method: cfg.Method}
	}
	return result, nil
}

func validPaymentURL(raw string, fragment bool) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && (fragment || u.Fragment == "")
}
