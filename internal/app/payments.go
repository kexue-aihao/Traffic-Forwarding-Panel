package app

import (
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/url"
	"strings"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/commerce"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
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
	// FeePercent 是这条通道加收的手续费百分比（"1.5" 就是 1.5%），
	// FeeFixed 是固定部分、以元为单位（"1.00" 就是一元）。手续费加在充值
	// 金额之上：用户充值 100 元、手续费 1.5%，实付 101.50 元，钱包到账 100 元。
	FeePercent string `json:"fee_percent"`
	FeeFixed   string `json:"fee_fixed"`
	// Rate 是「1 单位加密货币折多少人民币」，用于给用户算出需要支付多少
	// USDT。网关侧也要配同一个汇率，两边才会一致。
	Rate string `json:"rate"`
}

// ParsePaymentConfigs 读一份运营方写的通道配置文件。
//
// 拆成「解析」与「构建」两步：配置要存进数据库供面板展示与编辑，能拿到原始
// 字段才谈得上回显 —— 已经建好的 Adapter 是取不回商户密钥的。
func ParsePaymentConfigs(r io.Reader) (map[string]PaymentConfiguration, error) {
	d := json.NewDecoder(io.LimitReader(r, 1<<20))
	d.DisallowUnknownFields()
	var input map[string]PaymentConfiguration
	if err := d.Decode(&input); err != nil {
		return nil, errors.New("invalid payment configuration JSON")
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, errors.New("payment configuration requires one JSON object")
	}
	return input, nil
}

// BuildPaymentChannels 校验配置并建成可用的通道。校验失败时只报是哪条通道，
// 不带任何字段值 —— 这些错误会一路回到界面上。
func BuildPaymentChannels(input map[string]PaymentConfiguration, origin string) (map[string]commerce.Channel, error) {
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
		feeBPS, feeFixed, err := paymentFees(cfg)
		if err != nil {
			return nil, errors.New("invalid fee configuration for " + name)
		}
		if err := validRate(cfg.Rate); err != nil {
			return nil, errors.New("invalid exchange rate for " + name)
		}
		result[name] = commerce.Channel{Adapter: adapter, NotifyURL: cfg.NotifyURL, ReturnURL: cfg.ReturnURL, Method: cfg.Method, FeeBPS: feeBPS, FeeFixed: feeFixed, Rate: cfg.Rate, CryptoCurrency: cfg.CryptoCurrency}
	}
	return result, nil
}

// paymentFees 把运营方写的百分比与固定手续费换算成万分之一与分。
//
// 两个字段都是可选的，留空就是不收手续费 —— 升级既有部署时不会凭空多出一笔
// 费用。百分比最多两位小数：再细就不是给人看的了。
func paymentFees(cfg PaymentConfiguration) (int, int64, error) {
	bps := 0
	if percent := strings.TrimSpace(cfg.FeePercent); percent != "" {
		value, ok := new(big.Rat).SetString(percent)
		if !ok || value.Sign() < 0 || value.Cmp(big.NewRat(100, 1)) > 0 {
			return 0, 0, errors.New("fee percent out of range")
		}
		scaled := new(big.Rat).Mul(value, big.NewRat(100, 1))
		if !scaled.IsInt() || !scaled.Num().IsInt64() {
			return 0, 0, errors.New("fee percent needs at most two decimals")
		}
		bps = int(scaled.Num().Int64())
	}
	fixed := int64(0)
	if raw := strings.TrimSpace(cfg.FeeFixed); raw != "" {
		value, err := contract.ParseAmount(raw)
		if err != nil {
			return 0, 0, err
		}
		fixed = value
	}
	return bps, fixed, nil
}

// validRate 校验通道汇率：留空表示不报价，给了就必须是正数。
func validRate(raw string) error {
	rate := strings.TrimSpace(raw)
	if rate == "" {
		return nil
	}
	value, ok := new(big.Rat).SetString(rate)
	if !ok || value.Sign() <= 0 {
		return errors.New("exchange rate must be positive")
	}
	return nil
}

func validPaymentURL(raw string, fragment bool) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && (fragment || u.Fragment == "")
}

// startupPaymentConfigs 把启动时的两种来源并成一份原始配置：配置文件里的那些，
// 加上旧环境变量等价的那条 epay —— 显式写在文件里的 epay 优先。
//
// 环境变量那条在下面被补全回调地址后才算完整，所以这里拼的是原始字段，后面统一
// 走同一套校验与构建。
func startupPaymentConfigs(opts Options) map[string]PaymentConfiguration {
	configs := map[string]PaymentConfiguration{}
	for name, cfg := range opts.PaymentConfigs {
		configs[name] = cfg
	}
	if _, exists := configs["epay"]; exists || opts.EPay.Gateway == "" || opts.EPay.Key == "" {
		return configs
	}
	notify, returned := opts.EPay.NotifyURL, opts.EPay.ReturnURL
	if notify == "" {
		notify = strings.TrimRight(opts.Origin, "/") + "/api/v1/payments/epay/notify"
	}
	if returned == "" {
		returned = strings.TrimRight(opts.Origin, "/") + "/#/commerce"
	}
	configs["epay"] = PaymentConfiguration{Gateway: opts.EPay.Gateway, MerchantID: opts.EPay.PID, Key: opts.EPay.Key, NotifyURL: notify, ReturnURL: returned}
	return configs
}
