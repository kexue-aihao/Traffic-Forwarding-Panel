package contract

import "time"

type Diagnostic struct {
	ID          string            `json:"id"`
	RuleID      string            `json:"rule_id"`
	RuleVersion int64             `json:"rule_version"`
	Status      string            `json:"status"`
	Claim       string            `json:"claim,omitempty"`
	Checks      []DiagnosticCheck `json:"checks"`
	CreatedAt   time.Time         `json:"created_at"`
}
type DiagnosticCheck struct {
	Stage        string `json:"stage"`
	OK           bool   `json:"ok"`
	Milliseconds int64  `json:"milliseconds"`
	Detail       string `json:"detail"`
}
