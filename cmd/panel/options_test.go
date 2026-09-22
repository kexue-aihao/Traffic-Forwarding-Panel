package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestOptionsPrecedenceAndCompatibility(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("listen: 127.0.0.1:18888\ntrust-proxy: true\ndatabase-path: sqlite3://data.db\n"), 0600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"TFP_CONFIG_FILE": path, "TFP_ADDR": "127.0.0.1:19999", "TFP_DSN": "env.db"}
	getenv := func(key string) string { return env[key] }
	o, err := readOptions([]string{"-addr", "127.0.0.1:17777", "-trust-proxy=false"}, getenv, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if o.Listen != "127.0.0.1:17777" || o.TrustProxy || o.DSN != "env.db" {
		t.Fatal("explicit flags did not override environment/YAML")
	}
	o, err = readOptions(nil, getenv, io.Discard)
	if err != nil || o.Listen != env["TFP_ADDR"] || !o.TrustProxy {
		t.Fatal("environment precedence failed", err)
	}
	delete(env, "TFP_ADDR")
	delete(env, "TFP_DSN")
	o, err = readOptions(nil, getenv, io.Discard)
	if err != nil || o.Listen != "127.0.0.1:18888" || o.DSN != filepath.Join(filepath.Dir(path), "data.db") {
		t.Fatal("YAML paths failed", err)
	}
	o, err = readOptions([]string{"-database", "sqlite", "-dsn", "old.db", "-init-admin", "admin"}, func(string) string { return "" }, io.Discard)
	if err != nil || o.DSN != "old.db" || o.InitAdmin != "admin" {
		t.Fatal("legacy flags failed", err)
	}
}

func TestLocalCommandsCannotBeCombined(t *testing.T) {
	for _, args := range [][]string{{"-check-config", "-init-admin", "admin"}, {"-healthcheck", "-backup", "snapshot"}, {"config.yaml"}} {
		if _, err := readOptions(args, func(string) string { return "" }, io.Discard); err == nil {
			t.Fatal("ambiguous command accepted", args)
		}
	}
}
