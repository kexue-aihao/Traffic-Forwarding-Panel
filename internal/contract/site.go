package contract

type SiteSettings struct {
	Version              int64  `json:"version"`
	Name                 string `json:"name"`
	Announcement         string `json:"announcement"`
	Registration         string `json:"registration"`
	Captcha              bool   `json:"captcha"`
	Accent               string `json:"accent"`
	PaymentsEnabled      bool   `json:"payments_enabled"`
	MinimumRecharge      int64  `json:"minimum_recharge_cents,string"`
	MaximumRecharge      int64  `json:"maximum_recharge_cents,string"`
	DiagnosticsEnabled   bool   `json:"diagnostics_enabled"`
	DiagnosticsPerMinute int    `json:"diagnostics_per_minute"`
}
