package main

import (
	"errors"
	"flag"
	"io"
	"strconv"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/panelconfig"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/platform"
)

type options struct {
	panelconfig.Config
	ConfigFile                                                    string
	ShowVersion, CheckHealth, CheckConfig                         bool
	InitAdmin, ResetPassword, BackupPath, RestorePath, ExportHTML string
}

func rateLimit(value *panelconfig.RateLimit) *platform.RateLimit {
	if value == nil {
		return nil
	}
	return &platform.RateLimit{Period: time.Duration(value.Rate) * time.Second, Limit: value.Limit}
}

// Precedence: explicit flags > environment > YAML > built-in defaults.
func readOptions(args []string, getenv func(string) string, output io.Writer) (options, error) {
	o := options{Config: panelconfig.Defaults()}
	f := flag.NewFlagSet("panel", flag.ContinueOnError)
	f.SetOutput(output)
	f.StringVar(&o.ConfigFile, "config", getenv("TFP_CONFIG_FILE"), "YAML startup configuration file")
	f.BoolVar(&o.ShowVersion, "version", false, "print panel release version")
	f.BoolVar(&o.CheckHealth, "healthcheck", false, "check the running panel HTTP/database health and exit")
	f.BoolVar(&o.CheckConfig, "check-config", false, "validate startup configuration without opening the database")
	f.BoolVar(&o.TrustProxy, "trust-proxy", false, "trust protocol and client IP headers overwritten by an isolated reverse proxy")
	f.StringVar(&o.Listen, "addr", o.Listen, "HTTP(S) listen address")
	f.StringVar(&o.Driver, "database", o.Driver, "sqlite, postgres or mysql")
	f.StringVar(&o.DSN, "dsn", o.DSN, "database DSN (prefer configuration or TFP_DSN for credentials)")
	f.StringVar(&o.Origin, "origin", "", "public scheme://host for reverse proxy and CSRF checks")
	f.StringVar(&o.InitAdmin, "init-admin", "", "create first administrator then exit; reads password from stdin")
	f.StringVar(&o.ResetPassword, "reset-password", "", "reset account password and revoke sessions; reads password from stdin")
	f.StringVar(&o.PaymentsFile, "payments", "", "operator-owned payment configuration JSON file")
	f.StringVar(&o.AgentDir, "agent-dir", "", "directory holding the published Agent binaries")
	f.StringVar(&o.HTMLPath, "html-path", "", "external frontend directory; defaults to embedded assets")
	f.StringVar(&o.TLSCert, "tls-cert", "", "certificate PEM for direct HTTPS")
	f.StringVar(&o.TLSKey, "tls-key", "", "private key PEM for direct HTTPS")
	f.StringVar(&o.BackupPath, "backup", "", "export sensitive database snapshot to a new JSONL file")
	f.StringVar(&o.RestorePath, "restore", "", "restore JSONL into an empty database")
	f.StringVar(&o.ExportHTML, "export-html", "", "export embedded frontend to a new directory for Caddy")
	if err := f.Parse(args); err != nil {
		return o, err
	}
	if f.NArg() != 0 {
		return o, errors.New("不接受位置参数；配置文件请使用 -config 指定")
	}
	if o.ShowVersion {
		return o, nil
	}
	explicit := map[string]bool{}
	f.Visit(func(v *flag.Flag) { explicit[v.Name] = true })
	c := panelconfig.Defaults()
	if o.ConfigFile != "" {
		var err error
		c, err = panelconfig.Load(o.ConfigFile)
		if err != nil {
			return o, err
		}
	}
	for _, item := range []struct {
		env, flag string
		value     *string
		cli       string
	}{
		{"TFP_ADDR", "addr", &c.Listen, o.Listen},
		{"TFP_DATABASE", "database", &c.Driver, o.Driver},
		{"TFP_DSN", "dsn", &c.DSN, o.DSN},
		{"TFP_ORIGIN", "origin", &c.Origin, o.Origin},
		{"TFP_PAYMENTS_FILE", "payments", &c.PaymentsFile, o.PaymentsFile},
		{"TFP_AGENT_DIR", "agent-dir", &c.AgentDir, o.AgentDir},
		{"TFP_HTML_PATH", "html-path", &c.HTMLPath, o.HTMLPath},
		{"TFP_TLS_CERT", "tls-cert", &c.TLSCert, o.TLSCert},
		{"TFP_TLS_KEY", "tls-key", &c.TLSKey, o.TLSKey},
	} {
		if value := getenv(item.env); value != "" {
			*item.value = value
		}
		if explicit[item.flag] {
			*item.value = item.cli
		}
	}
	if value := getenv("TFP_TRUST_PROXY"); value != "" {
		var err error
		c.TrustProxy, err = strconv.ParseBool(value)
		if err != nil {
			return o, errors.New("TFP_TRUST_PROXY 必须为 true 或 false")
		}
	}
	if explicit["trust-proxy"] {
		c.TrustProxy = o.TrustProxy
	}
	o.Config = c
	if err := o.Validate(); err != nil {
		return o, err
	}
	commands := 0
	for _, command := range []string{o.InitAdmin, o.ResetPassword, o.BackupPath, o.RestorePath, o.ExportHTML} {
		if command != "" {
			commands++
		}
	}
	if o.CheckConfig {
		commands++
	}
	if o.CheckHealth {
		commands++
	}
	if commands > 1 {
		return o, errors.New("一次只能选择一个本地管理命令")
	}
	return o, nil
}
