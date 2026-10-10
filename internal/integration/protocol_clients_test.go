package integration

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/agent"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/policy/detect"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/tunnel"
	"golang.org/x/net/proxy"
)

// Run with TFP_TEST_XRAY pointing to the independently distributed Xray binary.
// This uses real protocol servers and clients: a timeout alone is never proof
// of blocking; the echo origin and the Agent's rejection count are observed.
func TestIndependentXrayClients(t *testing.T) {
	xray := os.Getenv("TFP_TEST_XRAY")
	if xray == "" {
		t.Skip("set TFP_TEST_XRAY to run the independent-client matrix")
	}
	if _, err := os.Stat(xray); err != nil {
		t.Fatal(err)
	}
	const uuid = "acba6f58-e34f-40e0-80bc-d0e760c68ac3"
	key128 := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x31}, 16))
	key256 := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x32}, 32))
	cases := []struct{ name, protocol, method, password string }{
		{"socks5", "socks5", "", ""},
		{"ss2017-aes128", "shadowsocks", "aes-128-gcm", "independent-test-password"},
		{"ss2017-aes256", "shadowsocks", "aes-256-gcm", "independent-test-password"},
		{"ss2017-chacha", "shadowsocks", "chacha20-ietf-poly1305", "independent-test-password"},
		{"ss2022-aes128", "shadowsocks", "2022-blake3-aes-128-gcm", key128},
		{"ss2022-aes256", "shadowsocks", "2022-blake3-aes-256-gcm", key256},
		{"vmess-aead", "vmess", "", ""},
		{"vmess-ws", "vmess", "", ""},
		{"trojan-tls", "trojan", "", "independent-trojan-password"},
	}
	for _, carrier := range []string{"direct", "tls"} {
		for _, tc := range cases {
			if carrier == "tls" && (tc.name == "vmess-ws" || tc.protocol == "trojan") {
				continue
			}
			t.Run(carrier+"/"+tc.name, func(t *testing.T) {
				var received atomic.Int64
				origin, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { origin.Close() })
				go func() {
					for {
						c, e := origin.Accept()
						if e != nil {
							return
						}
						go func() {
							defer c.Close()
							buf := make([]byte, 1024)
							for {
								n, e := c.Read(buf)
								if n > 0 {
									received.Add(int64(n))
									c.Write(buf[:n])
								}
								if e != nil {
									return
								}
							}
						}()
					}
				}()
				upstream := freeAddress(t, "tcp")
				pair, roots := certificate(t)
				certProfile := managedProfile(t, pair, []string{upstream})
				var serverSettings, clientSettings map[string]any
				xProtocol := tc.protocol
				switch tc.protocol {
				case "socks5":
					xProtocol = "socks"
					serverSettings = map[string]any{"auth": "noauth"}
					clientSettings = map[string]any{"servers": []any{map[string]any{"address": "127.0.0.1"}}}
				case "shadowsocks":
					serverSettings = map[string]any{"method": tc.method, "password": tc.password, "network": "tcp"}
					clientSettings = map[string]any{"servers": []any{map[string]any{"address": "127.0.0.1", "method": tc.method, "password": tc.password}}}
				case "vmess":
					serverSettings = map[string]any{"clients": []any{map[string]any{"id": uuid, "alterId": 0}}}
					clientSettings = map[string]any{"vnext": []any{map[string]any{"address": "127.0.0.1", "users": []any{map[string]any{"id": uuid, "alterId": 0, "security": "auto"}}}}}
				case "trojan":
					serverSettings = map[string]any{"clients": []any{map[string]any{"password": tc.password}}}
					clientSettings = map[string]any{"servers": []any{map[string]any{"address": "127.0.0.1", "password": tc.password}}}
				}
				inbound := xrayInbound(upstream, xProtocol, serverSettings)
				outbound := map[string]any{"protocol": xProtocol, "settings": clientSettings}
				if tc.name == "vmess-ws" {
					stream := map[string]any{"network": "ws", "wsSettings": map[string]any{"path": "/proxy"}}
					inbound["streamSettings"] = stream
					outbound["streamSettings"] = stream
				}
				if tc.protocol == "trojan" {
					inbound["streamSettings"] = map[string]any{"network": "tcp", "security": "tls", "tlsSettings": map[string]any{"certificates": []any{map[string]any{"certificateFile": certProfile.Certificate, "keyFile": certProfile.PrivateKey}}}}
					outbound["streamSettings"] = map[string]any{"network": "tcp", "security": "tls", "tlsSettings": map[string]any{"serverName": "localhost", "allowInsecure": false, "disableSystemRoot": true, "certificates": []any{map[string]any{"certificateFile": certProfile.Certificate, "usage": "verify"}}}}
				}
				startXray(t, xray, map[string]any{"inbounds": []any{inbound}, "outbounds": []any{map[string]any{"protocol": "freedom"}}}, upstream)
				f := newFixture(t, tunnel.Client{TLS: &tls.Config{RootCAs: roots}})
				var spec *contract.Tunnel
				if carrier == "tls" {
					listener, e := net.Listen("tcp", "127.0.0.1:0")
					if e != nil {
						t.Fatal(e)
					}
					s := &tunnel.Server{TLS: &tls.Config{Certificates: []tls.Certificate{pair}}, Token: "independent-client-carrier-token"}
					done := make(chan struct{})
					go func() { defer close(done); s.Serve(listener, "tls") }()
					t.Cleanup(func() { s.Close(); <-done })
					spec = &contract.Tunnel{Endpoint: listener.Addr().String(), ServerName: "localhost", Token: s.Token}
				}
				profile := detect.Profile{Protocol: tc.protocol, Method: tc.method, Password: tc.password}
				if tc.protocol == "vmess" {
					profile.UUID = uuid
					profile.Password = ""
				}
				if len(tc.method) > 5 && tc.method[:5] == "2022-" {
					profile.Key = tc.password
					profile.Password = ""
				}
				labels := []string{}
				if tc.protocol != "socks5" {
					f.agent.Runtime.InspectionProfiles = detect.Profiles{"client-test": profile}
					labels = []string{"client-test"}
				}
				f.sync()
				r := f.rule("tcp", carrier, upstream, spec)
				var business *contract.BusinessInbound
				if tc.name == "vmess-ws" {
					business = &contract.BusinessInbound{WebSocket: true}
				}
				if tc.protocol == "trojan" {
					f.agent.Runtime.BusinessProfiles = map[string]agent.BusinessProfile{"owned-service": {Certificate: certProfile.Certificate, PrivateKey: certProfile.PrivateKey, CA: certProfile.CA, ServerName: "localhost", AllowedListen: []string{r.Listen}, AllowedTargets: []string{upstream}}}
					business = &contract.BusinessInbound{TLSProfile: "owned-service", UpstreamTLSProfile: "owned-service"}
				}
				f.sync()
				_, port, _ := net.SplitHostPort(r.Listen)
				portNum, _ := strconv.Atoi(port)
				if tc.protocol == "vmess" {
					clientSettings["vnext"].([]any)[0].(map[string]any)["port"] = portNum
				} else {
					clientSettings["servers"].([]any)[0].(map[string]any)["port"] = portNum
				}
				local := freeAddress(t, "tcp")
				startXray(t, xray, map[string]any{"inbounds": []any{xrayInbound(local, "socks", map[string]any{"auth": "noauth"})}, "outbounds": []any{outbound}}, local)
				exchange := func(want bool) {
					t.Helper()
					d, e := proxy.SOCKS5("tcp", local, nil, &net.Dialer{Timeout: 2 * time.Second})
					if e != nil {
						t.Fatal(e)
					}
					c, e := d.Dial("tcp", origin.Addr().String())
					if e != nil {
						if want {
							t.Fatal(e)
						}
						return
					}
					defer c.Close()
					c.SetDeadline(time.Now().Add(8 * time.Second))
					marker := []byte("independent-client-business")
					_, e = c.Write(marker)
					got := make([]byte, len(marker))
					if e == nil {
						_, e = io.ReadFull(c, got)
					}
					if want && (e != nil || !bytes.Equal(marker, got)) {
						t.Fatalf("client exchange failed %q: %v", got, e)
					}
					if !want && e == nil {
						t.Fatal("prohibited protocol reached origin")
					}
				}
				exchange(true)
				baseline := received.Load()
				setAdvanced(t, f, &contract.GroupAdvanced{BlockedProtocol: []string{tc.protocol}, Inspection: &contract.InspectionPolicy{Version: 1, Profiles: labels, Business: business}})
				if cfg := f.store.Config(); len(cfg.Rules) != 1 {
					t.Fatalf("detector route not prepared: %+v", cfg.BlockedRules)
				}
				exchange(false)
				if received.Load() != baseline {
					t.Fatalf("blocked bytes reached origin: %d", received.Load()-baseline)
				}
				statuses := f.agent.Runtime.PolicyStatuses()
				if len(statuses) != 1 || statuses[0].Rejected == 0 {
					t.Fatalf("no actual detector rejection: %+v", statuses)
				}
				setAdvanced(t, f, nil)
				exchange(true)
			})
		}
	}
}

func xrayInbound(address, protocol string, settings map[string]any) map[string]any {
	host, port, _ := net.SplitHostPort(address)
	n, _ := strconv.Atoi(port)
	return map[string]any{"listen": host, "port": n, "protocol": protocol, "settings": settings}
}

func startXray(t *testing.T, binary string, config map[string]any, address string) {
	t.Helper()
	config["log"] = map[string]any{"loglevel": "info"}
	body, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err = os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(filepath.Join(dir, "client.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "run", "-config", path)
	cmd.Stdout = log
	cmd.Stderr = log
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
		log.Close()
		if t.Failed() {
			raw, _ := os.ReadFile(log.Name())
			t.Logf("independent Xray log: %s", raw)
		}
	})
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case e := <-done:
			raw, _ := os.ReadFile(log.Name())
			t.Fatalf("Xray exited: %v %s", e, raw)
		default:
		}
		c, e := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if e == nil {
			c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	raw, _ := os.ReadFile(log.Name())
	t.Fatal(fmt.Sprintf("Xray did not listen: %s", raw))
}
