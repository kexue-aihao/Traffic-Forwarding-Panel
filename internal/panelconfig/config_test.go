package panelconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDatabasePaths(t *testing.T) {
	for _, tc := range []struct{ raw, driver, dsn string }{
		{"sqlite3://data.db", "sqlite", "data.db"},
		{"mysql://u:p@tcp(127.0.0.1:3306)/tfp?parseTime=true", "mysql", "u:p@tcp(127.0.0.1:3306)/tfp?parseTime=true"},
		{"postgres://host=localhost user=tfp dbname=tfp TimeZone=Asia/Shanghai", "postgres", "host=localhost user=tfp dbname=tfp TimeZone=Asia/Shanghai"},
		{"postgres://u:p@localhost/tfp?sslmode=disable", "postgres", "postgres://u:p@localhost/tfp?sslmode=disable"},
	} {
		driver, dsn, err := ParseDatabasePath(tc.raw)
		if err != nil || driver != tc.driver || dsn != tc.dsn {
			t.Fatalf("unexpected database interpretation: %s %s %v", driver, dsn, err)
		}
	}
}

func TestConfigurationFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("database-path: sqlite3://data.db\nlisten: 127.0.0.1:18888\nmax-open-connection: 10\nmax-idle-connection: 0\nhtml-path: ./public\nuser-rate-limit:\n  rate: 5\n  limit: 5\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.DSN != filepath.Join(dir, "data.db") || c.HTMLPath != filepath.Join(dir, "public") || *c.MaxIdleConnection != 0 || c.UserRateLimit.Limit != 5 {
		t.Fatal("configuration values lost")
	}
	for _, body := range []string{
		"listen: secret-value\nlisten: 127.0.0.1:18888", "unknown-setting: secret-value", "key: secret-value", "listen: [secret-value]", "listen: 127.0.0.1:18888\n---\nlisten: 127.0.0.1:19999",
	} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := Load(path)
		if err == nil || strings.Contains(err.Error(), "secret-value") {
			t.Fatalf("invalid config accepted or secret exposed: %v", err)
		}
	}
}

func TestValidation(t *testing.T) {
	for _, change := range []func(*Config){
		func(c *Config) { c.Listen = "localhost" },
		func(c *Config) { c.TLSCert = "cert.pem" },
		func(c *Config) { c.Origin = "https://example.com/path" },
		func(c *Config) { c.OfflineNodeTime = 19 },
		func(c *Config) { c.OfflineNodeRetentionTime = 599 },
		func(c *Config) { c.UserRateLimit = &RateLimit{Rate: 0, Limit: 1} },
		func(c *Config) { open, idle := 2, 3; c.MaxOpenConnection, c.MaxIdleConnection = &open, &idle },
	} {
		c := Defaults()
		change(&c)
		if err := c.Validate(); err == nil {
			t.Fatal("invalid setting accepted")
		}
	}
}
