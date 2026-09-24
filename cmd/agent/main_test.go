package main

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTrustConfigSharesSessionCacheAcrossClones(t *testing.T) {
	config, err := trustConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if config.ClientSessionCache == nil || config.Clone().ClientSessionCache != config.ClientSessionCache || config.InsecureSkipVerify || config.MinVersion != tls.VersionTLS13 {
		t.Fatal("TLS trust configuration lost verification or shared session cache")
	}
}

func TestLoadPrivateNextHops(t *testing.T) {
	valid := `[{"transport":"tls","endpoint":"exit.example:9443","server_name":"exit.example","token":"test-private-hop-token"}]`
	for _, tc := range []struct {
		name, json string
		ok         bool
	}{
		{"valid", valid, true},
		{"trailing JSON", valid + ` []`, false},
		{"unknown field", strings.Replace(valid, `"transport"`, `"unexpected"`, 1), false},
		{"unencrypted hop", strings.Replace(valid, `"tls"`, `"direct"`, 1), false},
		{"short credential", strings.Replace(valid, "test-private-hop-token", "short", 1), false},
		{"oversized file", strings.Repeat(" ", 65537), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "next-hops.json")
			if err := os.WriteFile(path, []byte(tc.json), 0600); err != nil {
				t.Fatal(err)
			}
			hops, err := loadNextHops(path)
			if (err == nil) != tc.ok {
				t.Fatalf("loadNextHops error: %v", err)
			}
			if tc.ok && (len(hops) != 1 || hops[0].Token != "test-private-hop-token") {
				t.Fatal("authorized hop did not retain its private credential")
			}
		})
	}
}
