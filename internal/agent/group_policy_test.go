package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/policy"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/tunnel"
)

func TestFailoverThresholdCooldownAndSingleProbe(t *testing.T) {
	for _, f := range []contract.FailoverPolicy{{MaxFail: 3, CooldownSec: 30}, {MaxFail: 0, CooldownSec: 0}} {
		b := &binding{}
		v := contract.Rule{ID: "r", Target: "localhost:1", EffectivePolicy: &contract.EffectivePolicy{Hash: "generation", Failover: &f}}
		now := time.Now()
		for i := 0; i < max(1, f.MaxFail); i++ {
			c, ok := b.selectCandidate(v, nil, now)
			if !ok {
				t.Fatal("excluded before threshold")
			}
			b.markCandidate(v, c.ID, true, now)
		}
		if _, ok := b.selectCandidate(v, nil, now); ok {
			t.Fatal("cooldown ignored")
		}
		now = now.Add(time.Duration(max(1, f.CooldownSec)) * time.Second)
		c, ok := b.selectCandidate(v, nil, now)
		if !ok {
			t.Fatal("no half-open probe")
		}
		if _, ok = b.selectCandidate(v, nil, now); ok {
			t.Fatal("parallel half-open probes")
		}
		b.markCandidate(v, c.ID, false, now)
		if _, ok = b.selectCandidate(v, nil, now); !ok {
			t.Fatal("success did not recover")
		}
	}
}

func TestCandidateWeightAndAdditionPreserveHealthyConnection(t *testing.T) {
	_, runtime, config := setup(t)
	rule := testRule(managedEcho(t))
	rule.RouteCandidates = []contract.RouteCandidate{{ID: "primary", Target: rule.Target, Transport: "direct", Weight: 1}}
	config.Rules = []contract.Rule{rule}
	if err := runtime.Apply(config, true); err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("tcp", runtime.listeners[key(rule)].tcp.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := exchange(c, "before"); err != nil {
		t.Fatal(err)
	}
	rule.RouteCandidates[0].Weight = 5
	rule.RouteCandidates = append(rule.RouteCandidates, contract.RouteCandidate{ID: "backup", Target: rule.Target, Transport: "direct", Weight: 1})
	config.Rules = []contract.Rule{rule}
	config.Version++
	if err := runtime.Apply(config, true); err != nil {
		t.Fatal(err)
	}
	if err := exchange(c, "after"); err != nil {
		t.Fatal("candidate metadata disconnected healthy flow", err)
	}
	rule.RouteCandidates = rule.RouteCandidates[1:]
	config.Rules = []contract.Rule{rule}
	config.Version++
	if err := runtime.Apply(config, true); err != nil {
		t.Fatal(err)
	}
	if err := exchange(c, "revoked"); err == nil {
		t.Fatal("removed authorized candidate remained active")
	}
}

func TestFailoverRealTargetAndBillingIsolation(t *testing.T) {
	_, r, c := setup(t)
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	go func() {
		for {
			c, e := l.Accept()
			if e != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	v := testRule(l.Addr().String())
	v.BillingMultiplier = "1"
	v.Backends = []contract.Backend{{Target: "127.0.0.1:1", Weight: 100}, {Target: v.Target, Weight: 1}}
	v.EffectivePolicy = &contract.EffectivePolicy{Version: 2, Failover: &contract.FailoverPolicy{MaxFail: 3, CooldownSec: 30}}
	policy.Seal(v.EffectivePolicy)
	c.Rules = []contract.Rule{v}
	if e = r.Apply(c, true); e != nil {
		t.Fatal(e)
	}
	for range 3 {
		conn, e := net.Dial("tcp", r.listeners[key(v)].tcp.Addr().String())
		if e != nil {
			t.Fatal(e)
		}
		conn.SetDeadline(time.Now().Add(2 * time.Second))
		conn.Write([]byte("ok"))
		var b [2]byte
		if _, e = io.ReadFull(conn, b[:]); e != nil || string(b[:]) != "ok" {
			t.Fatalf("backup target failed %q %v", b, e)
		}
		conn.Close()
	}
	b := r.listeners[key(v)]
	b.mu.Lock()
	state := b.backends[b.backendKey(v, "127.0.0.1:1")]
	if state.failures != 3 || state.until.IsZero() {
		t.Fatal("fail threshold not enforced")
	}
	b.mu.Unlock()
	v.RouteCandidates = []contract.RouteCandidate{{ID: "unauthorized", Target: v.Target, Transport: "direct", Weight: 1, BillingMultiplier: "2"}}
	if conn, _, e := b.dial(context.Background(), v); e == nil {
		conn.Close()
		t.Fatal("lease reused across multiplier")
	}
}

func localProfile(t *testing.T, pair tls.Certificate, listens []string) ServiceProfile {
	t.Helper()
	dir := t.TempDir()
	cert, key := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	var cp []byte
	for _, der := range pair.Certificate {
		cp = append(cp, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	kb, e := x509.MarshalPKCS8PrivateKey(pair.PrivateKey)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(cert, cp, 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}), 0600); e != nil {
		t.Fatal(e)
	}
	return ServiceProfile{Certificate: cert, PrivateKey: key, CA: cert, AllowedListen: listens}
}
func freeTCP(t *testing.T) string {
	t.Helper()
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	a := l.Addr().String()
	l.Close()
	return a
}
func TestManagedReverseAllCarriers(t *testing.T) {
	pair, roots := chainCertificate(t)
	for _, carrier := range []string{"tls", "tls_simple", "ws", "http"} {
		t.Run(carrier, func(t *testing.T) {
			listen := freeTCP(t)
			endpoint := listen
			if carrier == "ws" {
				endpoint = "ws://" + listen + "/tunnel"
			}
			target, e := net.Listen("tcp", "127.0.0.1:0")
			if e != nil {
				t.Fatal(e)
			}
			defer target.Close()
			go func() {
				for {
					c, e := target.Accept()
					if e != nil {
						return
					}
					go func() { defer c.Close(); io.Copy(c, c) }()
				}
			}()
			profile := localProfile(t, pair, []string{listen})
			hub := &ServiceManager{Profiles: map[string]ServiceProfile{"local": profile}, BaseTLS: &tls.Config{RootCAs: roots}}
			exit := &ServiceManager{Profiles: map[string]ServiceProfile{"local": profile}, BaseTLS: &tls.Config{RootCAs: roots}}
			defer hub.Close()
			defer exit.Close()
			grant := contract.ServiceGrant{Identity: "relation-a", Token: "carrier-token-long-123456", StreamToken: "business-token-long-123456", Targets: []contract.ServiceTarget{{RuleID: "rule-a", Network: "tcp", Target: target.Addr().String()}}}
			second := grant
			second.StreamToken = "second-rule-token-long-123456"
			second.Targets = []contract.ServiceTarget{{RuleID: "rule-b", Network: "tcp", Target: target.Addr().String()}}
			grants := []contract.ServiceGrant{grant, second}
			apply := func(m *ServiceManager, configs []contract.ServiceConfig) {
				t.Helper()
				commit, _, e := m.Prepare(configs, time.Now().Add(time.Minute))
				if e != nil {
					t.Fatal(e)
				}
				commit()
			}
			apply(hub, []contract.ServiceConfig{{ID: "hub", Kind: "hub", Listen: listen, Endpoint: endpoint, Transport: carrier, Token: "hub-base-token-long-123456", Profile: "local", Grants: grants}})
			apply(exit, []contract.ServiceConfig{{ID: "connector", Kind: "reverse", Endpoint: endpoint, Transport: carrier, Token: grant.Token, Identity: grant.Identity, Profile: "local", TLS: contract.ReverseTLS{ServerName: "localhost"}, Grants: grants}})
			deadline := time.Now().Add(4 * time.Second)
			for !hub.Statuses()[0].Ready || !exit.Statuses()[0].Ready {
				if time.Now().After(deadline) {
					t.Fatalf("not ready: hub=%+v exit=%+v", hub.Statuses(), exit.Statuses())
				}
				time.Sleep(10 * time.Millisecond)
			}
			client := tunnel.Client{TLS: &tls.Config{RootCAs: roots}, RuleID: "rule-a"}
			spec := contract.Tunnel{Endpoint: endpoint, ServerName: "localhost", Token: grant.StreamToken, Reverse: grant.Identity}
			conn, e := client.DialRoute(t.Context(), carrier, "tcp", target.Addr().String(), spec)
			if e != nil {
				t.Fatal(e)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(2 * time.Second))
			p := []byte("reverse business payload")
			conn.Write(p)
			got := make([]byte, len(p))
			if _, e = io.ReadFull(conn, got); e != nil || !bytes.Equal(got, p) {
				t.Fatalf("echo %q %v", got, e)
			}
			client.RuleID = "rule-b"
			secondSpec := spec
			secondSpec.Token = second.StreamToken
			other, e := client.DialRoute(t.Context(), carrier, "tcp", target.Addr().String(), secondSpec)
			if e != nil {
				t.Fatalf("second rule on same reverse identity: %v", e)
			}
			if e := exchange(other, "second rule business"); e != nil {
				t.Fatal(e)
			}
			other.Close()
			client.RuleID = "other-tenant"
			if c, e := client.DialRoute(t.Context(), carrier, "tcp", target.Addr().String(), spec); e == nil {
				c.Close()
				t.Fatal("cross-rule credential accepted")
			}
			apply(exit, nil)
			conn.SetDeadline(time.Now().Add(time.Second))
			if n, _ := conn.Read(got); n > 0 {
				t.Fatal("revoked relationship remained active")
			}
		})
	}
}
