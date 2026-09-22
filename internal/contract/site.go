package contract

import "strings"

// SettlementCurrency 是站点默认的结算币种。人民币元 —— 面板上所有金额都按它
// 计价，加密货币通道只是在收款那一段按通道汇率折算。
const SettlementCurrency = "CNY"

// SiteSettings 是站点级配置。
//
// 金额字段是「元」的十进制字符串（见 money.go），不是分。旧版本存的是
// minimum_recharge_cents / maximum_recharge_cents，仍能读回来（下面的
// Legacy* 字段只用于兼容读取，保存时不再写出）。
type SiteSettings struct {
	Version              int64  `json:"version"`
	Name                 string `json:"name"`
	Announcement         string `json:"announcement"`
	Registration         string `json:"registration"`
	Captcha              bool   `json:"captcha"`
	Accent               string `json:"accent"`
	PaymentsEnabled      bool   `json:"payments_enabled"`
	Currency             string `json:"currency"`
	MinimumRecharge      string `json:"minimum_recharge"`
	MaximumRecharge      string `json:"maximum_recharge"`
	DiagnosticsEnabled   bool   `json:"diagnostics_enabled"`
	DiagnosticsPerMinute int    `json:"diagnostics_per_minute"`
	// GeoLookupURL 是探针页面位置图标所用的地区查询地址模板，必须含 {ip}
	// 占位符。留空表示关闭 —— 地址是客户机器的信息，面板不主动外发。
	GeoLookupURL string `json:"geo_lookup_url"`

	// 只用于读回 v0.1.1 及更早写下的旧配置。
	LegacyMinimumRechargeCents int64 `json:"minimum_recharge_cents,string,omitempty"`
	LegacyMaximumRechargeCents int64 `json:"maximum_recharge_cents,string,omitempty"`
}

// Normalize 把缺省字段补齐，并把旧版本以「分」为单位的配置换算成「元」。
//
// 读配置的每条路径都要经过它：数据库里的旧文档没有 currency，也可能只有
// 旧的分字段，直接拿去判空会得到一个「最小充值 0 元」的站点。
func (v *SiteSettings) Normalize() {
	if v.Currency == "" {
		v.Currency = SettlementCurrency
	}
	if v.MinimumRecharge == "" {
		if v.LegacyMinimumRechargeCents > 0 {
			v.MinimumRecharge = FormatAmount(v.LegacyMinimumRechargeCents)
		} else {
			v.MinimumRecharge = "1.00"
		}
	}
	if v.MaximumRecharge == "" {
		if v.LegacyMaximumRechargeCents > 0 {
			v.MaximumRecharge = FormatAmount(v.LegacyMaximumRechargeCents)
		} else {
			v.MaximumRecharge = "1000000.00"
		}
	}
	// 单位已经换成元，旧字段不再写回；留着会让同一个金额出现两种单位。
	v.LegacyMinimumRechargeCents = 0
	v.LegacyMaximumRechargeCents = 0
	if v.Registration == "" {
		v.Registration = "closed"
	}
	if v.Accent == "" {
		v.Accent = "blue"
	}
}

// RechargeRange 返回充值金额的允许区间（整数分）。
func (v SiteSettings) RechargeRange() (int64, int64, error) {
	min, err := ParseAmount(v.MinimumRecharge)
	if err != nil {
		return 0, 0, err
	}
	max, err := ParseAmount(v.MaximumRecharge)
	if err != nil {
		return 0, 0, err
	}
	if min < 1 || max < min {
		return 0, 0, ErrAmount
	}
	return min, max, nil
}

// ValidGeoLookupURL 校验地区查询地址模板：必须是 HTTPS，必须带 {ip} 占位符，
// 不能带认证信息。留空是合法的 —— 那表示关闭。
func ValidGeoLookupURL(raw string) bool {
	s := strings.TrimSpace(raw)
	if s == "" {
		return true
	}
	if len(s) > 300 || !strings.Contains(s, "{ip}") {
		return false
	}
	rest, ok := strings.CutPrefix(s, "https://")
	if !ok {
		return false
	}
	host, _, found := strings.Cut(rest, "/")
	if !found || host == "" || strings.Contains(host, "@") {
		return false
	}
	return !strings.ContainsAny(s, " \t\r\n")
}
