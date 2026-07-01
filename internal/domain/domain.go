package domain

import "time"

type Role string

const (
	RoleAdmin Role = "admin"
	RoleUser  Role = "user"
)

type Status string

const (
	StatusEnabled  Status = "enabled"
	StatusDisabled Status = "disabled"
)

type NodeStatus string

const (
	NodeOnline  NodeStatus = "online"
	NodeOffline NodeStatus = "offline"
	NodePaused  NodeStatus = "paused"
)

type Protocol string

const (
	ProtocolTCP Protocol = "tcp"
	ProtocolUDP Protocol = "udp"
)

type ServiceStatus string

const (
	ServiceActive ServiceStatus = "active"
	ServicePaused ServiceStatus = "paused"
	ServiceClosed ServiceStatus = "closed"
)

type PaymentStatus string

const (
	PaymentPending PaymentStatus = "pending"
	PaymentPaid    PaymentStatus = "paid"
	PaymentFailed  PaymentStatus = "failed"
	PaymentClosed  PaymentStatus = "closed"
)

type CommandType string

const (
	CommandUpsertService CommandType = "upsert_service"
	CommandDeleteService CommandType = "delete_service"
	CommandPauseService  CommandType = "pause_service"
	CommandResumeService CommandType = "resume_service"
	CommandSyncState     CommandType = "sync_state"
)

type ActorKind string

const (
	ActorAdmin ActorKind = "admin"
	ActorUser  ActorKind = "user"
	ActorNode  ActorKind = "node"
)

type Admin struct {
	ID           int64     `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"-"`
	Status       Status    `json:"status"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type User struct {
	ID                       int64      `json:"id"`
	Username                 string     `json:"username"`
	PasswordHash             string     `json:"-"`
	Status                   Status     `json:"status"`
	BalanceCents             int64      `json:"balance_cents"`
	FlowQuotaMB              int64      `json:"flow_quota_mb"`
	TrafficUsedBytes         int64      `json:"traffic_used_bytes"`
	AffBalanceCents          int64      `json:"aff_balance_cents"`
	PlanID                   *int64     `json:"plan_id,omitempty"`
	UserGroupID              *int64     `json:"user_group_id,omitempty"`
	MaxRules                 int        `json:"max_rules"`
	TrafficEnable            bool       `json:"traffic_enable"`
	AutoRenew                bool       `json:"auto_renew"`
	TelegramID               string     `json:"telegram_id"`
	InviteCode               string     `json:"invite_code"`
	InvitedByUserID          *int64     `json:"invited_by_user_id,omitempty"`
	AllowDevice              bool       `json:"allow_device"`
	NotificationSettingsJSON string     `json:"notification_settings_json,omitempty"`
	ExpiresAt                *time.Time `json:"expires_at,omitempty"`
	CreatedAt                time.Time  `json:"created_at"`
	UpdatedAt                time.Time  `json:"updated_at"`
}

type Node struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	Host       string     `json:"host"`
	Port       int        `json:"port"`
	Secret     string     `json:"-"`
	Status     NodeStatus `json:"status"`
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

type Tunnel struct {
	ID               int64         `json:"id"`
	UserID           int64         `json:"user_id"`
	NodeID           int64         `json:"node_id"`
	Name             string        `json:"name"`
	Protocol         Protocol      `json:"protocol"`
	ListenAddr       string        `json:"listen_addr"`
	TargetAddr       string        `json:"target_addr"`
	ListenHost       string        `json:"listen_host"`
	ListenPort       *int          `json:"listen_port,omitempty"`
	TargetHost       string        `json:"target_host"`
	TargetPort       *int          `json:"target_port,omitempty"`
	DeviceGroupInID  *int64        `json:"device_group_in_id,omitempty"`
	DeviceGroupOutID *int64        `json:"device_group_out_id,omitempty"`
	ConfigJSON       string        `json:"config_json,omitempty"`
	Folder           string        `json:"folder"`
	ShowOrder        int           `json:"show_order"`
	MaxConn          int           `json:"max_conn"`
	SpeedLimitKB     int           `json:"speed_limit_kb"`
	QuotaBytes       int64         `json:"quota_bytes"`
	UsedBytes        int64         `json:"used_bytes"`
	ExpiresAt        *time.Time    `json:"expires_at,omitempty"`
	AutoPauseOnLimit bool          `json:"auto_pause_on_limit"`
	Status           ServiceStatus `json:"status"`
	CreatedAt        time.Time     `json:"created_at"`
	UpdatedAt        time.Time     `json:"updated_at"`
}

type ForwardService struct {
	ID           int64         `json:"id"`
	TunnelID     int64         `json:"tunnel_id"`
	UserID       int64         `json:"user_id"`
	NodeID       int64         `json:"node_id"`
	ServiceKey   string        `json:"service_key"`
	Protocol     Protocol      `json:"protocol"`
	ListenAddr   string        `json:"listen_addr"`
	TargetAddr   string        `json:"target_addr"`
	Status       ServiceStatus `json:"status"`
	MaxConn      int           `json:"max_conn"`
	SpeedLimitKB int           `json:"speed_limit_kb"`
	QuotaBytes   int64         `json:"quota_bytes"`
	UsedBytes    int64         `json:"used_bytes"`
	BytesIn      int64         `json:"bytes_in"`
	BytesOut     int64         `json:"bytes_out"`
	ActiveConn   int           `json:"active_conn"`
	PausedReason string        `json:"paused_reason"`
	CreatedAt    time.Time     `json:"created_at"`
	UpdatedAt    time.Time     `json:"updated_at"`
}

type DeviceGroup struct {
	ID             int64      `json:"id"`
	Name           string     `json:"name"`
	Type           string     `json:"type"`
	NodeID         *int64     `json:"node_id,omitempty"`
	Ratio          float64    `json:"ratio"`
	ConnectHost    string     `json:"connect_host"`
	PortRangeStart *int       `json:"port_range_start,omitempty"`
	PortRangeEnd   *int       `json:"port_range_end,omitempty"`
	ConfigJSON     string     `json:"config_json,omitempty"`
	ShowOrder      int        `json:"show_order"`
	DisplayNum     int        `json:"display_num"`
	HideStatus     bool       `json:"hide_status"`
	Status         Status     `json:"status"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	DeletedAt      *time.Time `json:"deleted_at,omitempty"`
}

type Plan struct {
	ID           int64      `json:"id"`
	Name         string     `json:"name"`
	Description  string     `json:"description"`
	PriceCents   int64      `json:"price_cents"`
	TrafficMB    int64      `json:"traffic_mb"`
	DurationDays int        `json:"duration_days"`
	MaxRules     int        `json:"max_rules"`
	AllowDevice  bool       `json:"allow_device"`
	UserGroupID  *int64     `json:"user_group_id,omitempty"`
	Status       Status     `json:"status"`
	ShowOrder    int        `json:"show_order"`
	ConfigJSON   string     `json:"config_json,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	DeletedAt    *time.Time `json:"deleted_at,omitempty"`
}

type UserGroup struct {
	ID                int64     `json:"id"`
	Name              string    `json:"name"`
	Description       string    `json:"description"`
	MaxRules          int       `json:"max_rules"`
	TrafficMultiplier float64   `json:"traffic_multiplier"`
	PermissionsJSON   string    `json:"permissions_json,omitempty"`
	ShowOrder         int       `json:"show_order"`
	Status            Status    `json:"status"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type PaymentChannel struct {
	ID         int64     `json:"id"`
	Code       string    `json:"code"`
	Name       string    `json:"name"`
	Enabled    bool      `json:"enabled"`
	Provider   string    `json:"provider"`
	ConfigJSON string    `json:"config_json"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type PaymentOrder struct {
	ID          int64         `json:"id"`
	OrderNo     string        `json:"order_no"`
	UserID      int64         `json:"user_id"`
	Channel     string        `json:"channel"`
	AmountCents int64         `json:"amount_cents"`
	Status      PaymentStatus `json:"status"`
	PayURL      string        `json:"pay_url"`
	TradeNo     string        `json:"trade_no"`
	RawRequest  string        `json:"raw_request"`
	RawNotify   string        `json:"raw_notify"`
	PaidAt      *time.Time    `json:"paid_at,omitempty"`
	CreatedAt   time.Time     `json:"created_at"`
	UpdatedAt   time.Time     `json:"updated_at"`
}

type Session struct {
	ID        int64     `json:"id"`
	ActorKind ActorKind `json:"actor_kind"`
	ActorID   int64     `json:"actor_id"`
	TokenHash string    `json:"-"`
	ExpiresAt time.Time `json:"expires_at"`
	CreatedAt time.Time `json:"created_at"`
}

type NodeCommand struct {
	ID          int64       `json:"id"`
	NodeID      int64       `json:"node_id"`
	Type        CommandType `json:"type"`
	PayloadJSON string      `json:"payload_json"`
	Status      string      `json:"status"`
	AvailableAt time.Time   `json:"available_at"`
	ConsumedAt  *time.Time  `json:"consumed_at,omitempty"`
	CreatedAt   time.Time   `json:"created_at"`
}

type UsageReport struct {
	NodeID     int64     `json:"node_id"`
	ServiceKey string    `json:"service_key"`
	BytesIn    int64     `json:"bytes_in"`
	BytesOut   int64     `json:"bytes_out"`
	ActiveConn int       `json:"active_conn"`
	ReportedAt time.Time `json:"reported_at"`
}

type CommandEnvelope struct {
	CommandID int64       `json:"command_id"`
	NodeID    int64       `json:"node_id"`
	Type      CommandType `json:"type"`
	Payload   any         `json:"payload"`
}

type NodeCommandPayload struct {
	Service ForwardService `json:"service"`
	Reason  string         `json:"reason,omitempty"`
}
