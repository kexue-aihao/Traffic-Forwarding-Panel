// Package contract defines version 1 wire contracts shared by panel and Agent.
package contract

import "time"

const Version = 1

type User struct {
	ID                string `json:"id"`
	Username          string `json:"username"`
	Role              string `json:"role"`
	IdentityGroupID   string `json:"identity_group_id"`
	IdentityGroupName string `json:"identity_group_name,omitempty"`
	Disabled          bool   `json:"disabled"`
}

// UserCreated includes the initial password exactly once in the create response.
// The stored user record contains only its bcrypt hash.
type UserCreated struct {
	ID                string `json:"id"`
	Username          string `json:"username"`
	Role              string `json:"role"`
	IdentityGroupID   string `json:"identity_group_id"`
	IdentityGroupName string `json:"identity_group_name,omitempty"`
	Disabled          bool   `json:"disabled"`
	InitialPassword   string `json:"initial_password"`
}

// UserPasswordReset is returned once after an administrator resets an account.
type UserPasswordReset struct {
	UserID   string `json:"user_id"`
	Password string `json:"password"`
}

// IdentityGroup is the authorization identity shared by one or more users.
// Device groups grant access to these identities instead of individual users.
type IdentityGroup struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	UserCount        int    `json:"user_count"`
	DeviceGroupCount int    `json:"device_group_count"`
}

type Group struct {
	ID               string         `json:"id"`
	Name             string         `json:"name"`
	Type             string         `json:"type"`
	DirectPolicy     string         `json:"direct_policy,omitempty"`
	ChainGroupIDs    []string       `json:"chain_group_ids,omitempty"`
	Advanced         *GroupAdvanced `json:"advanced,omitempty"`
	IdentityGroupIDs []string       `json:"identity_group_ids"`
	// UserIDs is accepted only to migrate older API clients to identity groups.
	UserIDs            []string `json:"user_ids,omitempty"`
	BlockedProtocols   []string `json:"blocked_protocols"`
	DisabledNetworks   []string `json:"disabled_networks"`
	DisabledTransports []string `json:"disabled_transports"`
	Multiplier         string   `json:"multiplier"`
	PortMin            int      `json:"port_min"`
	PortMax            int      `json:"port_max"`
	MaxRules           int      `json:"max_rules"`
	Version            int64    `json:"version"`
}

// GroupAdvanced mirrors the optional device-group settings exposed by the
// reference panel. The UI edits JSONC; the API validates structured JSON.
type GroupAdvanced struct {
	AllowedHost       []string       `json:"allowed_host,omitempty"`
	BlockedHost       []string       `json:"blocked_host,omitempty"`
	BlockedPath       []string       `json:"blocked_path,omitempty"`
	BlockedProtocol   []string       `json:"blocked_protocol,omitempty"`
	TLSInboundPolicy  int            `json:"tls_inbound_policy,omitempty"`
	TLSRejectEmptySNI bool           `json:"tls_reject_empty_sni,omitempty"`
	DisableUDP        bool           `json:"disable_udp,omitempty"`
	UDPOverTCP        bool           `json:"udp_over_tcp,omitempty"`
	IPv6Group         []string       `json:"ipv6_group,omitempty"`
	MaxFail           int            `json:"max_fail"`
	FailTimeoutSec    int            `json:"fail_timout_sec"`
	ReverseGroup      []string       `json:"reverse_group,omitempty"`
	Protocol          string         `json:"protocol,omitempty"`
	TLS               map[string]any `json:"tls,omitempty"`
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
	NodeID        string    `json:"node_id"`
	Online        *bool     `json:"online,omitempty"`
	SampledAt     time.Time `json:"sampled_at"`
	CPUPercent    *float64  `json:"cpu_percent"`
	MemoryUsed    *uint64   `json:"memory_used,string"`
	MemoryTotal   *uint64   `json:"memory_total,string"`
	DiskUsed      *uint64   `json:"disk_used,string"`
	DiskTotal     *uint64   `json:"disk_total,string"`
	UploadBPS     *float64  `json:"upload_bps"`
	DownloadBPS   *float64  `json:"download_bps"`
	UploadTotal   *uint64   `json:"upload_total,string"`
	DownloadTotal *uint64   `json:"download_total,string"`
	UptimeSeconds *uint64   `json:"uptime_seconds,string"`
	Load1         *float64  `json:"load1"`
	// CPU 型号是静态的，但「这台机器性能怎么样」第一个要看的就是它 ——
	// 光有占用率回答不了「为什么这台一直满载」。
	CPUModel  *string         `json:"cpu_model"`
	SwapUsed  *uint64         `json:"swap_used,string"`
	SwapTotal *uint64         `json:"swap_total,string"`
	PublicIPs []IPObservation `json:"public_ips,omitempty"`

	// 以下三个字段由控制面在返回探针时补齐，Agent 从不上报：节点名、所属
	// 设备组和位置图标。放在 Probe 上而不是另做一层响应结构，是因为实时
	// 推送、历史查询和前端都已经按这个形状对齐了。
	NodeName string   `json:"node_name,omitempty"`
	GroupIDs []string `json:"group_ids,omitempty"`
	// Location 保留作为旧客户端的单一位置字段；新客户端应按地址族读取
	// IPv4Location 与 IPv6Location，避免把一台双栈机器的两个地址混在一起。
	Location     *GeoLocation `json:"location,omitempty"`
	IPv4Location *GeoLocation `json:"ipv4_location,omitempty"`
	IPv6Location *GeoLocation `json:"ipv6_location,omitempty"`
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

// GeoLocation 是探针卡片上的位置图标所依据的地区信息。
//
// 只有国家/地区码是必需的：图标就靠它画。国家和城市是给人看的补充。
// 探针页面按地区区分设备，但不因此暴露机器地址 —— 普通用户看得到位置、
// 看不到 IP，管理员两者都能看到。
type GeoLocation struct {
	CountryCode string `json:"country_code"`
	CountryName string `json:"country_name,omitempty"`
	Region      string `json:"region,omitempty"`
	City        string `json:"city,omitempty"`
	// Source 说明这条位置是怎么来的：geo（查到的）或 node（节点自带）。
	Source string `json:"source,omitempty"`
}

// DeviceIP 是探针页面归属组在某一刻对外暴露的地址。
//
// 机器被替换或换 IP 之后，客户脚本要拿到的是「现在这一个」。所以响应里
// 同时给出节点身份与观测时间：换了机器看 node_id，只换 IP 看 address 与
// observed_at，两者都是同一个列表的字段，不需要另外的接口。
type DeviceIP struct {
	NodeID     string       `json:"node_id"`
	NodeName   string       `json:"node_name"`
	GroupID    string       `json:"group_id"`
	GroupName  string       `json:"group_name"`
	Address    string       `json:"address"`
	Family     string       `json:"family"`
	Source     string       `json:"source"`
	ObservedAt time.Time    `json:"observed_at"`
	Online     bool         `json:"online"`
	LastSeen   *time.Time   `json:"last_seen,omitempty"`
	Location   *GeoLocation `json:"location,omitempty"`
}
