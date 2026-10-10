package contract

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

const GroupPolicyVersion = 2

// UnmarshalJSON retains explicit zeroes while giving omitted parameters useful
// defaults. Decode strictly even when nested inside an API request.
func (a *GroupAdvanced) UnmarshalJSON(data []byte) error {
	type plain GroupAdvanced
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if v, ok := raw["fail_timeout_sec"]; ok {
		if old, exists := raw["fail_timout_sec"]; exists && !bytes.Equal(bytes.TrimSpace(v), bytes.TrimSpace(old)) {
			return errors.New("conflicting fail_timeout_sec and fail_timout_sec")
		}
		raw["fail_timout_sec"] = v
		delete(raw, "fail_timeout_sec")
		var err error
		data, err = json.Marshal(raw)
		if err != nil {
			return err
		}
	}
	v := plain{MaxFail: 3, FailTimeoutSec: 30}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&v); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("one advanced JSON object required")
	}
	*a = GroupAdvanced(v)
	return nil
}

type EffectivePolicy struct {
	Version       int             `json:"version"`
	InboundLayers []InboundPolicy `json:"inbound_layers,omitempty"`
	PreferIPv6    bool            `json:"prefer_ipv6,omitempty"`
	Failover      *FailoverPolicy `json:"failover,omitempty"`
	Hash          string          `json:"hash"`
}

type InboundPolicy struct {
	GroupID        string   `json:"group_id"`
	AllowedHosts   []string `json:"allowed_hosts,omitempty"`
	BlockedHosts   []string `json:"blocked_hosts,omitempty"`
	BlockedPaths   []string `json:"blocked_paths,omitempty"`
	BlockedApps    []string `json:"blocked_apps,omitempty"`
	TLSRequired    bool     `json:"tls_required,omitempty"`
	RejectEmptySNI bool     `json:"reject_empty_sni,omitempty"`
}

type FailoverPolicy struct {
	MaxFail     int `json:"max_fail"`
	CooldownSec int `json:"cooldown_sec"`
}

type RouteCandidate struct {
	ID                string           `json:"id"`
	ExitGroupID       string           `json:"exit_group_id,omitempty"`
	Target            string           `json:"target"`
	Transport         string           `json:"transport"`
	Tunnel            *Tunnel          `json:"tunnel,omitempty"`
	Weight            int              `json:"weight"`
	EffectivePolicy   *EffectivePolicy `json:"effective_policy,omitempty"`
	BillingMultiplier string           `json:"billing_multiplier"`
}

type BlockedRule struct {
	RuleID string `json:"rule_id"`
	Reason string `json:"reason"`
}

// ReverseTLS contains operator-selected profile names, never file paths or keys.
type ReverseTLS struct {
	Enabled                  *bool    `json:"enabled,omitempty"`
	ServerName               string   `json:"server_name,omitempty"`
	MinVersion               string   `json:"min_version,omitempty"`
	ALPN                     []string `json:"alpn,omitempty"`
	CAProfile                string   `json:"ca_profile,omitempty"`
	CertificateProfile       string   `json:"certificate_profile,omitempty"`
	ClientCertificateProfile string   `json:"client_certificate_profile,omitempty"`
}

type ServiceGrant struct {
	Identity    string          `json:"identity"`
	StreamToken string          `json:"stream_token"`
	Token       string          `json:"token"`
	Targets     []ServiceTarget `json:"targets,omitempty"`
}

type ServiceTarget struct {
	RuleID  string `json:"rule_id"`
	Network string `json:"network"`
	Target  string `json:"target"`
}

type ServiceConfig struct {
	ID         string           `json:"id"`
	Kind       string           `json:"kind"` // exit, hub, reverse
	GroupID    string           `json:"group_id"`
	Listen     string           `json:"listen,omitempty"`
	Endpoint   string           `json:"endpoint,omitempty"`
	Transport  string           `json:"transport"`
	PreferIPv6 bool             `json:"prefer_ipv6,omitempty"`
	Identity   string           `json:"identity,omitempty"`
	Token      string           `json:"token,omitempty"`
	Profile    string           `json:"profile,omitempty"`
	TLS        ReverseTLS       `json:"tls"`
	Grants     []ServiceGrant   `json:"grants,omitempty"`
	Policy     *EffectivePolicy `json:"policy,omitempty"`
	UDP        *UDPExit         `json:"udp,omitempty"`
	NextHops   []TunnelHop      `json:"next_hops,omitempty"`
}

type ServiceStatus struct {
	ID    string `json:"id"`
	Ready bool   `json:"ready"`
	Error string `json:"error,omitempty"`
}

type RuleRuntimeStatus struct {
	RuleID        string `json:"rule_id"`
	PolicyHash    string `json:"policy_hash,omitempty"`
	CandidateID   string `json:"candidate_id,omitempty"`
	AddressFamily string `json:"address_family,omitempty"`
	Carrier       string `json:"carrier,omitempty"`
	Rejected      uint64 `json:"rejected"`
}
