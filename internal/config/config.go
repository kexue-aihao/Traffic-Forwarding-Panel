package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Mode                  string
	HTTPAddr              string
	DatabaseDSN           string
	AppName               string
	BaseURL               string
	MasterSecret          string
	SessionTTL            time.Duration
	BootstrapAdminUser    string
	BootstrapAdminPass    string
	BootstrapAdminEmail   string
	NodePollInterval      time.Duration
	NodeReportInterval    time.Duration
	AgentUDPIdleTimeout   time.Duration
	AgentNodeID           int64
	AgentNodeSecret       string
	AgentServerURL        string
	AgentListenAddr       string
	AgentNodeName         string
	AgentNodeHost         string
	AgentNodePort         int
	EpayAPIURL            string
	EpayPID               string
	EpayKey               string
	EpayType              string
	BEpusdtAPIURL         string
	BEpusdtPID            string
	BEpusdtKey            string
	BEpusdtType           string
	AllowInsecureDefaults bool
	PublicRegisterEnabled bool
}

func Load() Config {
	cfg := Config{
		Mode:                  envOr("TP_MODE", "server"),
		HTTPAddr:              envOr("TP_HTTP_ADDR", ":8080"),
		DatabaseDSN:           envOr("TP_DATABASE_DSN", "sqlite:/data/trafficpanel.db"),
		AppName:               envOr("TP_APP_NAME", "Traffic Panel"),
		BaseURL:               envOr("TP_BASE_URL", "http://127.0.0.1:8080"),
		MasterSecret:          envOr("TP_MASTER_SECRET", "change-me-in-production"),
		SessionTTL:            envDurationOr("TP_SESSION_TTL", 24*time.Hour),
		BootstrapAdminUser:    envOr("TP_BOOTSTRAP_ADMIN_USER", "admin"),
		BootstrapAdminPass:    envOr("TP_BOOTSTRAP_ADMIN_PASS", "admin123456"),
		BootstrapAdminEmail:   envOr("TP_BOOTSTRAP_ADMIN_EMAIL", "admin@example.com"),
		NodePollInterval:      envDurationOr("TP_NODE_POLL_INTERVAL", 3*time.Second),
		NodeReportInterval:    envDurationOr("TP_NODE_REPORT_INTERVAL", 10*time.Second),
		AgentUDPIdleTimeout:   envDurationOr("TP_AGENT_UDP_IDLE_TIMEOUT", 2*time.Minute),
		AgentServerURL:        envOr("TP_AGENT_SERVER_URL", "http://127.0.0.1:8080"),
		AgentListenAddr:       envOr("TP_AGENT_LISTEN_ADDR", "0.0.0.0:0"),
		AgentNodeName:         envOr("TP_AGENT_NODE_NAME", "default-node"),
		AgentNodeHost:         envOr("TP_AGENT_NODE_HOST", "127.0.0.1"),
		EpayAPIURL:            envOr("TP_EPAY_API_URL", ""),
		EpayPID:               envOr("TP_EPAY_PID", ""),
		EpayKey:               envOr("TP_EPAY_KEY", ""),
		EpayType:              envOr("TP_EPAY_TYPE", "alipay"),
		BEpusdtAPIURL:         envOr("TP_BEPUSDT_API_URL", ""),
		BEpusdtPID:            envOr("TP_BEPUSDT_PID", ""),
		BEpusdtKey:            envOr("TP_BEPUSDT_KEY", ""),
		BEpusdtType:           envOr("TP_BEPUSDT_TYPE", "usdt"),
		AllowInsecureDefaults: envBoolOr("TP_ALLOW_INSECURE_DEFAULTS", false),
		PublicRegisterEnabled: envBoolOr("TP_PUBLIC_REGISTER_ENABLED", false),
	}
	cfg.AgentNodeID = envInt64Or("TP_AGENT_NODE_ID", 0)
	cfg.AgentNodePort = int(envInt64Or("TP_AGENT_NODE_PORT", 0))
	cfg.AgentNodeSecret = envOr("TP_AGENT_NODE_SECRET", "")
	return cfg
}

func envOr(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func envDurationOr(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envBoolOr(key string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envInt64Or(key string, fallback int64) int64 {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fallback
	}
	return parsed
}
