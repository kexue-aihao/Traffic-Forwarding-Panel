package platform

import (
	"bytes"
	"testing"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

func TestChainRuleRedactionUpdateAndGroupPolicy(t *testing.T) {
	f := setup(t)
	g, n := f.node()
	rule := ruleFor(g, n)
	rule.Transport = "tls"
	rule.Tunnel = &contract.Tunnel{Endpoint: "exit-one.example:443", ServerName: "exit-one.example", Token: "first-test-token-long", Chain: []contract.TunnelHop{{Transport: "wss", Endpoint: "wss://exit-two.example/tunnel", ServerName: "exit-two.example", Token: "second-test-token-long"}, {Transport: "http", Endpoint: "exit-three.example:9443", Token: "third-test-token-long"}}}
	created := f.req("POST", "/rules", rule, "")
	rule = read[contract.Rule](t, created, 201)
	if bytes.Contains(created.Body.Bytes(), []byte("test-token-long")) {
		t.Fatal("created response exposed hop credentials")
	}
	listed := f.req("GET", "/rules", nil, "")
	if listed.Code != 200 || bytes.Contains(listed.Body.Bytes(), []byte("test-token-long")) {
		t.Fatal("list exposed credentials", listed.Body.String())
	}
	rule.Name = "updated without sending stored credentials"
	rule = read[contract.Rule](t, f.req("PUT", "/rules/"+rule.ID, rule, ""), 200)
	cfg := read[contract.Config](t, f.req("GET", "/agent/config", nil, n.Token), 200)
	if len(cfg.Rules) != 1 || cfg.Rules[0].Tunnel.Token != "first-test-token-long" || cfg.Rules[0].Tunnel.Chain[1].Token != "third-test-token-long" {
		t.Fatal("redacted update lost stored hop credentials")
	}
	rule.Tunnel.Chain[0].Endpoint = "wss://other.example/tunnel"
	if res := f.req("PUT", "/rules/"+rule.ID, rule, ""); res.Code != 409 {
		t.Fatal("credential reused for changed endpoint")
	}
	g.DisabledTransports = []string{"http"}
	read[contract.Group](t, f.req("PUT", "/groups/"+g.ID, g, ""), 200)
	cfg = read[contract.Config](t, f.req("GET", "/agent/config", nil, n.Token), 200)
	if len(cfg.Rules) != 0 {
		t.Fatal("later chain hop bypassed group transport policy")
	}
}

func TestChainRuleRejectsLoopsAndOversizedRoute(t *testing.T) {
	rule := contract.Rule{Name: "chain", Network: "tcp", Transport: "tls", Listen: "127.0.0.1:12000", Target: "target.example:443", Tunnel: &contract.Tunnel{Endpoint: "exit.example:9443", Token: "first-test-token-long"}}
	rule.Tunnel.Chain = []contract.TunnelHop{{Transport: "wss", Endpoint: "wss://EXIT.example:9443/path", Token: "second-test-token-long"}}
	if _, err := validateRule(rule); err == nil {
		t.Fatal("equivalent endpoint loop accepted")
	}
	rule.Tunnel.Chain = []contract.TunnelHop{{Transport: "tls", Endpoint: "second.example:9443", Token: "second-test-token-long"}, {Transport: "tls", Endpoint: "third.example:9443", Token: "third-test-token-long"}, {Transport: "tls", Endpoint: "fourth.example:9443", Token: "fourth-test-token-long"}}
	if _, err := validateRule(rule); err == nil {
		t.Fatal("four exits accepted")
	}
}
