// Package contract defines version 1 wire contracts shared by panel and Agent.
package contract

import "time"

const Version = 1

type User struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Role     string `json:"role"`
	Disabled bool   `json:"disabled"`
}

type Group struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	UserIDs          []string `json:"user_ids"`
	BlockedProtocols []string `json:"blocked_protocols"`
	Multiplier       string   `json:"multiplier"`
	PortMin          int      `json:"port_min"`
	PortMax          int      `json:"port_max"`
	MaxRules         int      `json:"max_rules"`
	Version          int64    `json:"version"`
}

type Node struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	GroupIDs       []string   `json:"group_ids"`
	Version        string     `json:"agent_version"`
	OS             string     `json:"os"`
	Arch           string     `json:"arch"`
	Capabilities   []string   `json:"capabilities"`
	LastSeen       *time.Time `json:"last_seen"`
	DesiredVersion int64      `json:"desired_version"`
	AppliedVersion int64      `json:"applied_version"`
	ApplyError     string     `json:"apply_error"`
}

// Tunnel credentials are distributed only to the assigned Agent, never user lists.
type Tunnel struct {
	Endpoint   string `json:"endpoint"`
	ServerName string `json:"server_name"`
	Token      string `json:"token,omitempty"`
	Mux        bool   `json:"mux,omitempty"`
	Reverse    string `json:"reverse,omitempty"`
	// Chain lists the remaining exits after Endpoint; at most two are allowed.
	Chain []TunnelHop `json:"chain,omitempty"`
}

// TunnelHop is one authenticated, encrypted exit in an ordered tunnel chain.
type TunnelHop struct {
	Transport  string `json:"transport"`
	Endpoint   string `json:"endpoint"`
	ServerName string `json:"server_name"`
	Token      string `json:"token,omitempty"`
}

type Rule struct {
	ExitGroupID       string         `json:"exit_group_id,omitempty"`
	ExitID            string         `json:"exit_id,omitempty"`
	SelectedExitID    string         `json:"selected_exit_id,omitempty"`
	BillingMultiplier string         `json:"billing_multiplier,omitempty"`
	ExitUnavailable   bool           `json:"exit_unavailable,omitempty"`
	ProxyProtocol     *ProxyProtocol `json:"proxy_protocol,omitempty"`
	ID                string         `json:"id"`
	UserID            string         `json:"user_id"`
	Name              string         `json:"name"`
	NodeID            string         `json:"node_id"`
	GroupID           string         `json:"group_id"`
	Network           string         `json:"network"`
	Transport         string         `json:"transport"`
	Listen            string         `json:"listen"`
	Target            string         `json:"target"`
	Enabled           bool           `json:"enabled"`
	Version           int64          `json:"version"`
	BlockedProtocols  []string       `json:"blocked_protocols"`
	Tunnel            *Tunnel        `json:"tunnel,omitempty"`
	Lease             *Lease         `json:"lease,omitempty"`
	Backends          []Backend      `json:"backends,omitempty"`
	SharedTLS         *SharedTLS     `json:"shared_tls,omitempty"`
}

// Lease is a finite node allocation; expired/unallocated bytes cannot be spent.
type Lease struct {
	ID            string         `json:"id"`
	EntitlementID string         `json:"entitlement_id"`
	ExpiresAt     time.Time      `json:"expires_at"`
	Bytes         int64          `json:"bytes,string"`
	Limits        ResourceLimits `json:"limits"`
}

type Config struct {
	ContractVersion int       `json:"contract_version"`
	NodeID          string    `json:"node_id"`
	Version         int64     `json:"version"`
	ValidUntil      time.Time `json:"valid_until"`
	Rules           []Rule    `json:"rules"`
}

type Ack struct {
	AgentVersion   string   `json:"agent_version,omitempty"`
	Capabilities   []string `json:"capabilities,omitempty"`
	Version        int64    `json:"version"`
	AppliedVersion int64    `json:"applied_version"`
	Error          string   `json:"error"`
}

type Registration struct {
	Token        string   `json:"token"`
	Name         string   `json:"name"`
	Version      string   `json:"agent_version"`
	OS           string   `json:"os"`
	Arch         string   `json:"arch"`
	Capabilities []string `json:"capabilities"`
}

type Registered struct {
	NodeID string `json:"node_id"`
	Token  string `json:"token"`
}

type IPObservation struct {
	Address    string    `json:"address"`
	Family     string    `json:"family"`
	Source     string    `json:"source"`
	ObservedAt time.Time `json:"observed_at"`
}

type Probe struct {
	NodeID        string          `json:"node_id"`
	SampledAt     time.Time       `json:"sampled_at"`
	CPUPercent    *float64        `json:"cpu_percent"`
	MemoryUsed    *uint64         `json:"memory_used,string"`
	MemoryTotal   *uint64         `json:"memory_total,string"`
	DiskUsed      *uint64         `json:"disk_used,string"`
	DiskTotal     *uint64         `json:"disk_total,string"`
	UploadBPS     *float64        `json:"upload_bps"`
	DownloadBPS   *float64        `json:"download_bps"`
	UptimeSeconds *uint64         `json:"uptime_seconds,string"`
	Load1         *float64        `json:"load1"`
	PublicIPs     []IPObservation `json:"public_ips,omitempty"`
}

type UsageRecord struct {
	ID            string    `json:"id"`
	NodeID        string    `json:"node_id"`
	RuleID        string    `json:"rule_id"`
	LeaseID       string    `json:"lease_id"`
	EntitlementID string    `json:"entitlement_id"`
	StartedAt     time.Time `json:"started_at"`
	EndedAt       time.Time `json:"ended_at"`
	UploadBytes   int64     `json:"upload_bytes,string"`
	DownloadBytes int64     `json:"download_bytes,string"`
}

type UsageBatch struct {
	Records []UsageRecord `json:"records"`
}

type APIError struct {
	Code      string `json:"code"`
	Error     string `json:"error"`
	RequestID string `json:"request_id,omitempty"`
}
