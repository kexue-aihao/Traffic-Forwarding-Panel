package integration

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/agent"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/probe"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/tunnel"
)

func managedProfile(t *testing.T, pair tls.Certificate, listens []string) agent.ServiceProfile {
	t.Helper()
	dir := t.TempDir()
	cp, kp := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	key, e := x509.MarshalPKCS8PrivateKey(pair.PrivateKey)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(cp, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pair.Certificate[0]}), 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(kp, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0600); e != nil {
		t.Fatal(e)
	}
	return agent.ServiceProfile{Certificate: cp, PrivateKey: kp, CA: cp, AllowedListen: listens}
}

func TestManagedUDPOverTCPDisableAndRestoreRealAgents(t *testing.T) {
	pair, roots := certificate(t)
	f := newFixture(t, tunnel.Client{TLS: &tls.Config{RootCAs: roots}})
	g := decode[contract.Group](t, f.request("POST", "/groups", contract.Group{Name: "managed-UDP", Type: contract.GroupExit, IdentityGroupIDs: []string{f.owner.IdentityGroupID}, Advanced: &contract.GroupAdvanced{PolicyVersion: 2}, Multiplier: "1"}, f.admin, 201))
	listen, udpListen := freeAddress(t, "tcp"), freeAddress(t, "udp")
	profile := managedProfile(t, pair, []string{listen, udpListen})
	exit := additionalAgent(t, f, g, tunnel.Client{TLS: &tls.Config{RootCAs: roots}}, profile)
	x := decode[contract.Exit](t, f.request("POST", "/exits", contract.Exit{Name: "managed-native-UDP", GroupID: g.ID, NodeID: exit.Store.Identity().NodeID, Managed: true, LocalProfile: "local", Listen: listen, Transport: "tls", Tunnel: contract.Tunnel{Endpoint: listen, ServerName: "localhost", Token: "managed-UDP-base-token-long-123456"}, UDP: &contract.UDPExit{Endpoint: udpListen, ServerName: "localhost", Token: "managed-QUIC-base-token-long-123456"}, Weight: 1, Enabled: true}, f.admin, 201))
	convergeManaged(t, f, exit, func() bool { v := exit.Runtime.Services.Statuses(); return len(v) > 0 && v[0].Ready })
	_, target := targets(t)
	r := decode[contract.Rule](t, f.request("POST", "/rules", contract.Rule{Name: "managed-UDP-rule", GroupID: f.group.ID, NodeID: f.store.Identity().NodeID, ExitGroupID: g.ID, ExitID: x.ID, Network: "udp", Transport: "direct", Listen: freeAddress(t, "udp"), Target: target, Enabled: true}, f.user, 201))
	convergeManaged(t, f, exit, func() bool { return len(f.store.Config().Rules) > 0 })
	rule := f.store.Config().Rules[0]
	granted := false
	for _, svc := range exit.Store.Config().Services {
		for _, g := range svc.Grants {
			if g.StreamToken == rule.Tunnel.Token && len(g.Targets) > 0 {
				granted = true
			}
		}
	}
	if !granted {
		t.Fatal("QUIC rule credential not granted by exit")
	}
	transfer(t, r, []byte("native-UDP"))
	if f.store.Config().Rules[0].Transport != "quic" {
		t.Fatal("native UDP used a stream carrier")
	}
	setAdvanced(t, f, &contract.GroupAdvanced{UDPOverTCP: true})
	convergeManaged(t, f, exit, func() bool { return len(f.store.Config().Rules) > 0 && f.store.Config().Rules[0].Transport == "tls" })
	transfer(t, r, []byte("UDP over TCP"))
	g.Advanced.DisableUDP = true
	g = decode[contract.Group](t, f.request("PUT", "/groups/"+g.ID, g, f.admin, 200))
	convergeManaged(t, f, exit, func() bool { return len(f.store.Config().Rules) == 0 })
	denied(t, r, []byte("disabled UDP"))
	g.Advanced.DisableUDP = false
	g = decode[contract.Group](t, f.request("PUT", "/groups/"+g.ID, g, f.admin, 200))
	setAdvanced(t, f, nil)
	convergeManaged(t, f, exit, func() bool { return len(f.store.Config().Rules) > 0 && f.store.Config().Rules[0].Transport == "quic" })
	transfer(t, r, []byte("native UDP restored"))
}

func TestGroupIPv6PreferenceRealManagedPeer(t *testing.T) {
	l, e := net.Listen("tcp6", "[::1]:0")
	if e != nil {
		t.Skipf("IPv6 loopback unavailable: %v", e)
	}
	listen := l.Addr().String()
	l.Close()
	_, port, _ := net.SplitHostPort(listen)
	pair, roots := certificate(t)
	f := newFixture(t, tunnel.Client{TLS: &tls.Config{RootCAs: roots}})
	g := decode[contract.Group](t, f.request("POST", "/groups", contract.Group{Name: "IPv6 peer", Type: contract.GroupExit, IdentityGroupIDs: []string{f.owner.IdentityGroupID}, Multiplier: "1"}, f.admin, 201))
	profile := managedProfile(t, pair, []string{listen})
	exit := additionalAgent(t, f, g, tunnel.Client{TLS: &tls.Config{RootCAs: roots}}, profile)
	x := decode[contract.Exit](t, f.request("POST", "/exits", contract.Exit{Name: "IPv6 exit", GroupID: g.ID, NodeID: exit.Store.Identity().NodeID, Managed: true, LocalProfile: "local", Listen: listen, Transport: "tls", Tunnel: contract.Tunnel{Endpoint: net.JoinHostPort("localhost", port), ServerName: "localhost", Token: "ipv6-exit-base-token-long-123456"}, Weight: 1, Enabled: true}, f.admin, 201))
	setAdvanced(t, f, &contract.GroupAdvanced{IPv6Group: []string{g.ID}, MaxFail: 3, FailTimeoutSec: 30})
	convergeManaged(t, f, exit, func() bool { v := exit.Runtime.Services.Statuses(); return len(v) > 0 && v[0].Ready })
	target, _ := targets(t)
	r := decode[contract.Rule](t, f.request("POST", "/rules", contract.Rule{Name: "IPv6-rule", GroupID: f.group.ID, NodeID: f.store.Identity().NodeID, ExitGroupID: g.ID, ExitID: x.ID, Network: "tcp", Transport: "direct", Listen: freeAddress(t, "tcp"), Target: target, Enabled: true}, f.user, 201))
	convergeManaged(t, f, exit, func() bool { return len(f.store.Config().Rules) > 0 })
	transfer(t, r, []byte("IPv6 verified localhost certificate"))
	v := f.agent.Runtime.PolicyStatuses()
	if len(v) != 1 || v[0].AddressFamily != "ipv6" {
		t.Fatalf("IPv6 preference did not reach an IPv6 socket: %+v", v)
	}
}
func additionalAgent(t *testing.T, f *fixture, g contract.Group, tc tunnel.Client, profile agent.ServiceProfile) *agent.Agent {
	t.Helper()
	en := decode[map[string]string](t, f.request("POST", "/nodes/enrollment", map[string]any{"name": "managed-exit", "group_ids": []string{g.ID}}, f.admin, 201))
	nodes := decode[struct{ Items []contract.Node }](t, f.request("GET", "/nodes", nil, f.admin, 200))
	var caps []string
	for _, n := range nodes.Items {
		if n.ID == f.store.Identity().NodeID {
			caps = n.Capabilities
		}
	}
	reg := decode[contract.Registered](t, f.request("POST", "/agent/register", contract.Registration{Token: en["token"], Name: "managed-exit", Version: agent.Version, OS: "windows", Arch: "amd64", Capabilities: caps}, nil, http.StatusCreated))
	store, e := agent.OpenStore(filepath.Join(t.TempDir(), "exit-agent.json"))
	if e != nil {
		t.Fatal(e)
	}
	if e = store.SetIdentity(reg); e != nil {
		t.Fatal(e)
	}
	a := &agent.Agent{URL: f.http.URL, Store: store, Runtime: agent.NewRuntime(store, tc), HTTP: f.http.Client(), Probe: &probe.Collector{}}
	a.Runtime.Services.Profiles = map[string]agent.ServiceProfile{"local": profile}
	t.Cleanup(func() { a.Runtime.Close(); store.Close() })
	if e = a.Step(context.Background()); e != nil {
		t.Fatal(e)
	}
	return a
}
func convergeManaged(t *testing.T, f *fixture, exit *agent.Agent, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for {
		f.sync()
		if e := exit.Step(t.Context()); e != nil {
			t.Fatal(e)
		}
		f.sync()
		if condition() {
			if e := exit.Step(t.Context()); e != nil {
				t.Fatal(e)
			}
			f.sync()
			if condition() {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("managed convergence timed out: entry=%+v exit=%+v", f.store.Config().BlockedRules, exit.Runtime.Services.Statuses())
		}
		time.Sleep(200 * time.Millisecond)
	}
}
func TestControlPlaneManagedReverseRealAgents(t *testing.T) {
	pair, roots := certificate(t)
	f := newFixture(t, tunnel.Client{TLS: &tls.Config{RootCAs: roots}})
	listen := freeAddress(t, "tcp")
	profile := managedProfile(t, pair, []string{listen})
	f.agent.Runtime.Services.Profiles = map[string]agent.ServiceProfile{"local": profile}
	g := decode[contract.Group](t, f.request("POST", "/groups", contract.Group{Name: "reverse-exit", Type: contract.GroupExit, IdentityGroupIDs: []string{f.owner.IdentityGroupID}, Advanced: &contract.GroupAdvanced{PolicyVersion: 2, ReverseGroup: []string{f.group.ID}, Protocol: "tls", TLS: map[string]any{"server_name": "localhost", "alpn": []string{"tfp-reverse-v1"}, "ca_profile": "local"}}, Multiplier: "1"}, f.admin, 201))
	exit := additionalAgent(t, f, g, tunnel.Client{TLS: &tls.Config{RootCAs: roots}}, profile)
	hub := decode[contract.Exit](t, f.request("POST", "/exits", contract.Exit{Name: "managed-hub", GroupID: f.group.ID, NodeID: f.store.Identity().NodeID, Managed: true, ReverseHub: true, LocalProfile: "local", Listen: listen, Transport: "tls", Tunnel: contract.Tunnel{Endpoint: listen, ServerName: "localhost", Token: "managed-hub-base-secret-123456"}, Weight: 1, Enabled: true}, f.admin, 201))
	_ = hub
	x := decode[contract.Exit](t, f.request("POST", "/exits", contract.Exit{Name: "managed-connector", GroupID: g.ID, NodeID: exit.Store.Identity().NodeID, Managed: true, LocalProfile: "local", Listen: freeAddress(t, "tcp"), Transport: "tls", Tunnel: contract.Tunnel{Endpoint: freeAddress(t, "tcp"), ServerName: "localhost", Token: "managed-exit-base-secret-123456"}, Weight: 1, Enabled: true}, f.admin, 201))
	convergeManaged(t, f, exit, func() bool {
		statuses := exit.Runtime.Services.Statuses()
		return len(statuses) > 0 && statuses[0].Ready && len(f.agent.Runtime.Services.Statuses()) > 1 && f.agent.Runtime.Services.Statuses()[1].Ready
	})
	target, _ := targets(t)
	r := decode[contract.Rule](t, f.request("POST", "/rules", contract.Rule{Name: "managed-reverse-rule", NodeID: f.store.Identity().NodeID, GroupID: f.group.ID, ExitGroupID: g.ID, ExitID: x.ID, Network: "tcp", Transport: "direct", Listen: freeAddress(t, "tcp"), Target: target, Enabled: true}, f.user, 201))
	convergeManaged(t, f, exit, func() bool { return len(f.store.Config().Rules) > 0 })
	transfer(t, r, []byte("control-plane reverse carrier"))
	g.Advanced.BlockedHost = []string{"blocked.example"}
	g = decode[contract.Group](t, f.request("PUT", "/groups/"+g.ID, g, f.admin, 200))
	convergeManaged(t, f, exit, func() bool {
		rules := f.store.Config().Rules
		return len(rules) > 0 && rules[0].Tunnel != nil && rules[0].Tunnel.Inspect
	})
	denied(t, r, []byte("GET / HTTP/1.1\r\nHost: blocked.example\r\n\r\n"))
	transfer(t, r, []byte("GET / HTTP/1.1\r\nHost: allowed.example\r\n\r\n"))
	// Authorization revocation must advance both nodes, and remove the exact
	// target grant from the remote connector as well as the entry listener.
	before := decode[struct{ Items []contract.Node }](t, f.request("GET", "/nodes", nil, f.admin, 200))
	f.request("PUT", "/users/"+f.owner.ID+"/status", map[string]bool{"disabled": true}, f.admin, 204)
	after := decode[struct{ Items []contract.Node }](t, f.request("GET", "/nodes", nil, f.admin, 200))
	for _, old := range before.Items {
		for _, next := range after.Items {
			if old.ID == next.ID && next.DesiredVersion <= old.DesiredVersion {
				t.Fatalf("authorization did not publish to %s", old.ID)
			}
		}
	}
	convergeManaged(t, f, exit, func() bool {
		if len(f.store.Config().Rules) != 0 {
			return false
		}
		for _, service := range exit.Store.Config().Services {
			for _, grant := range service.Grants {
				if len(grant.Targets) != 0 {
					return false
				}
			}
		}
		return true
	})
	denied(t, r, []byte("revoked account"))
	f.request("PUT", "/users/"+f.owner.ID+"/status", map[string]bool{"disabled": false}, f.admin, 204)
	convergeManaged(t, f, exit, func() bool { return len(f.store.Config().Rules) > 0 })
	transfer(t, r, []byte("GET / HTTP/1.1\r\nHost: allowed.example\r\n\r\n"))
	g.Advanced.TLS["enabled"] = false
	g = decode[contract.Group](t, f.request("PUT", "/groups/"+g.ID, g, f.admin, 200))
	convergeManaged(t, f, exit, func() bool { return len(f.store.Config().Rules) == 0 && len(exit.Runtime.Services.Statuses()) == 0 })
	denied(t, r, []byte("revoked relation"))
}
