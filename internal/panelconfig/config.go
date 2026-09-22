// Package panelconfig reads operator-owned YAML startup configuration.
package panelconfig

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

type RateLimit struct {
	Rate  int `yaml:"rate"`
	Limit int `yaml:"limit"`
}

type Config struct {
	DatabasePath             string     `yaml:"database-path"`
	MaxOpenConnection        *int       `yaml:"max-open-connection"`
	MaxIdleConnection        *int       `yaml:"max-idle-connection"`
	DisableQueue             bool       `yaml:"disable-queue"`
	Listen                   string     `yaml:"listen"`
	Origin                   string     `yaml:"origin"`
	TrustProxy               bool       `yaml:"trust-proxy"`
	HTMLPath                 string     `yaml:"html-path"`
	TLSCert                  string     `yaml:"tls-cert"`
	TLSKey                   string     `yaml:"tls-key"`
	DisableGzip              bool       `yaml:"disable-gzip"`
	OfflineNodeRetentionTime int        `yaml:"offline-node-retention-time"`
	OfflineNodeTime          int        `yaml:"offline-node-time"`
	UserRateLimit            *RateLimit `yaml:"user-rate-limit"`
	DefaultRateLimit         *RateLimit `yaml:"default-rate-limit"`
	PaymentsFile             string     `yaml:"payments-file"`
	AgentDir                 string     `yaml:"agent-dir"`
	Key                      any        `yaml:"key"`
	Driver                   string     `yaml:"-"`
	DSN                      string     `yaml:"-"`
}

func Defaults() Config {
	return Config{Listen: "127.0.0.1:8080", Driver: "sqlite", DSN: "data/panel.db", OfflineNodeRetentionTime: 86400, OfflineNodeTime: 20}
}

func Load(path string) (Config, error) {
	c := Defaults()
	f, err := os.Open(path)
	if err != nil {
		return c, errors.New("无法读取面板配置文件")
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || info.Size() > 1<<20 {
		return c, errors.New("面板配置文件不可读或超过 1 MiB")
	}
	d := yaml.NewDecoder(io.LimitReader(f, 1<<20))
	d.KnownFields(true)
	if err := d.Decode(&c); err != nil {
		return c, errors.New("面板配置必须是有效 YAML，且不能包含未知字段或重复字段")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return c, errors.New("面板配置只能包含一个 YAML 文档")
	}
	if c.Key != nil {
		return c, errors.New("key 是 Nyanpass 商业授权码，本面板不使用授权码；请移除 key 配置")
	}
	if c.DatabasePath != "" {
		c.Driver, c.DSN, err = ParseDatabasePath(c.DatabasePath)
		if err != nil {
			return c, err
		}
	}
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return c, errors.New("配置文件目录无效")
	}
	resolve := func(value string) string {
		if value == "" || filepath.IsAbs(value) {
			return value
		}
		return filepath.Join(base, value)
	}
	if c.Driver == "sqlite" && c.DSN != ":memory:" && !strings.HasPrefix(c.DSN, "file:") {
		c.DSN = resolve(c.DSN)
	}
	c.HTMLPath, c.TLSCert, c.TLSKey = resolve(c.HTMLPath), resolve(c.TLSCert), resolve(c.TLSKey)
	c.PaymentsFile, c.AgentDir = resolve(c.PaymentsFile), resolve(c.AgentDir)
	return c, nil
}

func ParseDatabasePath(value string) (string, string, error) {
	for _, prefix := range []string{"sqlite3://", "sqlite://", "mysql://", "postgres://", "postgresql://"} {
		if !strings.HasPrefix(value, prefix) {
			continue
		}
		dsn := strings.TrimPrefix(value, prefix)
		if strings.TrimSpace(dsn) == "" {
			break
		}
		switch prefix {
		case "sqlite3://", "sqlite://":
			return "sqlite", dsn, nil
		case "mysql://":
			return "mysql", dsn, nil
		default:
			// Nyanpass's example prefixes a libpq key=value DSN with postgres://.
			if strings.HasPrefix(dsn, "host=") || strings.HasPrefix(dsn, "user=") {
				return "postgres", dsn, nil
			}
			return "postgres", value, nil
		}
	}
	return "", "", errors.New("database-path 必须使用 sqlite3://、mysql:// 或 postgres://，并填写数据库地址")
}

func (c *Config) Validate() error {
	_, port, err := net.SplitHostPort(c.Listen)
	n, convErr := strconv.Atoi(port)
	if err != nil || convErr != nil || n < 1 || n > 65535 {
		return errors.New("listen 必须是有效的 主机:端口")
	}
	if c.Driver != "sqlite" && c.Driver != "mysql" && c.Driver != "postgres" {
		return errors.New("database 必须为 sqlite、mysql 或 postgres")
	}
	if strings.TrimSpace(c.DSN) == "" {
		return errors.New("数据库地址不能为空")
	}
	if c.Origin != "" {
		u, err := url.Parse(c.Origin)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return errors.New("origin 必须是公开的 http(s)://域名，不包含路径或凭据")
		}
		c.Origin = strings.TrimRight(c.Origin, "/")
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return errors.New("tls-cert 和 tls-key 必须同时配置")
	}
	if c.MaxOpenConnection != nil && (*c.MaxOpenConnection < 1 || *c.MaxOpenConnection > 10000) {
		return errors.New("max-open-connection 必须在 1–10000 之间")
	}
	if c.MaxIdleConnection != nil && (*c.MaxIdleConnection < 0 || *c.MaxIdleConnection > 10000) {
		return errors.New("max-idle-connection 必须在 0–10000 之间")
	}
	if c.MaxOpenConnection != nil && c.MaxIdleConnection != nil && *c.MaxIdleConnection > *c.MaxOpenConnection {
		return errors.New("max-idle-connection 不能大于 max-open-connection")
	}
	if c.OfflineNodeTime < 20 || c.OfflineNodeTime > 86400 {
		return errors.New("offline-node-time 必须在 20–86400 秒之间")
	}
	if c.OfflineNodeRetentionTime < 600 || c.OfflineNodeRetentionTime > 31536000 || c.OfflineNodeRetentionTime < c.OfflineNodeTime {
		return errors.New("offline-node-retention-time 必须在 600–31536000 秒之间，且不小于离线判定时间")
	}
	for name, limit := range map[string]*RateLimit{"user-rate-limit": c.UserRateLimit, "default-rate-limit": c.DefaultRateLimit} {
		if limit != nil && (limit.Rate < 1 || limit.Rate > 86400 || limit.Limit < 1 || limit.Limit > 1000000) {
			return fmt.Errorf("%s 的 rate 必须在 1–86400 秒之间，limit 必须在 1–1000000 之间", name)
		}
	}
	return nil
}
