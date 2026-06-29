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
	ID               int64      `json:"id"`
	Username         string     `json:"username"`
	PasswordHash     string     `json:"-"`
	Status           Status     `json:"status"`
	BalanceCents     int64      `json:"balance_cents"`
	FlowQuotaMB      int64      `json:"flow_quota_mb"`
	TrafficUsedBytes int64      `json:"traffic_used_bytes"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
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
