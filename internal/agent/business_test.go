package agent

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/policy"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/policy/detect"
)

func trojanBusinessHeader(password string) []byte {
	sum := sha256.Sum224([]byte(password))
	p := []byte(hex.EncodeToString(sum[:]) + "\r\n")
	p = append(p, 1, 1, 127, 0, 0, 1, 0, 80)
	return append(p, []byte("\r\nhello")...)
}

func businessEcho(t *testing.T, pair tls.Certificate) (string, *atomic.Int64) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	count := &atomic.Int64{}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			count.Add(1)
			go func() {
				defer c.Close()
				secure := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12})
				secure.SetDeadline(time.Now().Add(3 * time.Second))
				if secure.Handshake() == nil {
					io.Copy(secure, secure)
				}
			}()
		}
	}()
	return l.Addr().String(), count
}

func TestBusinessTLSDirectPolicyAndUpstreamIdentity(t *testing.T) {
	pair, roots := chainCertificate(t)
	target, count := businessEcho(t, pair)
	_, runtime, cfg := setup(t)
	rule := testRule(target)
	local := localProfile(t, pair, nil)
	runtime.BusinessProfiles = map[string]BusinessProfile{"business": {Certificate: local.Certificate, PrivateKey: local.PrivateKey, CA: local.CA, ServerName: "localhost", AllowedListen: []string{rule.Listen}, AllowedTargets: []string{target}}}
	runtime.InspectionProfiles = detect.Profiles{"trojan": {Protocol: "trojan", Password: "known-business-password"}}
	rule.Business = &contract.BusinessInbound{TLSProfile: "business", UpstreamTLSProfile: "business"}
	layer := contract.InboundPolicy{GroupID: "entry", BlockedApps: []string{"trojan"}, Inspection: &contract.InspectionPolicy{Version: 1, Profiles: []string{"trojan"}, Mode: "strict", Unknown: "allow"}}
	rule.EffectivePolicy = &contract.EffectivePolicy{Version: 2, InboundLayers: []contract.InboundPolicy{layer}}
	policy.Seal(rule.EffectivePolicy)
	cfg.Rules = []contract.Rule{rule}
	if err := runtime.Apply(cfg, true); err != nil {
		t.Fatal(err)
	}
	address := runtime.listeners[key(rule)].tcp.Addr().String()
	c, err := tls.Dial("tcp", address, &tls.Config{RootCAs: roots, ServerName: "localhost"})
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(2 * time.Second))
	payload := trojanBusinessHeader("known-business-password")
	c.Write(payload)
	var b [1]byte
	if _, err = c.Read(b[:]); err == nil {
		t.Fatal("Trojan business unexpectedly allowed")
	}
	c.Close()
	if count.Load() != 0 {
		t.Fatal("blocked business dialed origin")
	}
	statuses := runtime.PolicyStatuses()
	if len(statuses) != 1 || statuses[0].Rejected != 1 || statuses[0].DetectedProtocol != "trojan" || statuses[0].Evidence != "authenticated" {
		t.Fatal("missing authenticated rejection", statuses)
	}
	layer.Inspection.Mode = "observe"
	rule.EffectivePolicy.InboundLayers = []contract.InboundPolicy{layer}
	policy.Seal(rule.EffectivePolicy)
	cfg.Rules = []contract.Rule{rule}
	cfg.Version++
	if err = runtime.Apply(cfg, true); err != nil {
		t.Fatal(err)
	}
	c, err = tls.Dial("tcp", address, &tls.Config{RootCAs: roots, ServerName: "localhost"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err = c.Write(payload); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, len(payload))
	if _, err = io.ReadFull(c, reply); err != nil || string(reply) != string(payload) {
		t.Fatal("observe did not preserve business bytes", err)
	}
	// A valid identity policy prepares, then the real upstream handshake rejects
	// an origin certificate that does not authenticate that configured name.
	profile := runtime.BusinessProfiles["business"]
	profile.ServerName = "wrong.invalid"
	runtime.BusinessProfiles["business"] = profile
	cfg.Version++
	if err = runtime.Apply(cfg, true); err != nil {
		t.Fatal("valid local identity policy should prepare", err)
	}
	c.Close()
	bad, err := tls.Dial("tcp", address, &tls.Config{RootCAs: roots, ServerName: "localhost"})
	if err != nil {
		t.Fatal(err)
	}
	defer bad.Close()
	bad.SetDeadline(time.Now().Add(2 * time.Second))
	bad.Write(payload)
	if _, err = bad.Read(b[:]); err == nil {
		t.Fatal("origin certificate name was not verified")
	}
}

func TestManagedBusinessTLSExitInspectsActualStreamBeforeOrigin(t *testing.T) {
	pair, roots := chainCertificate(t)
	target, count := businessEcho(t, pair)
	listen := freeTCP(t)
	carrier := localProfile(t, pair, []string{listen})
	businessLocal := localProfile(t, pair, nil)
	business := &contract.BusinessInbound{TLSProfile: "business", UpstreamTLSProfile: "business"}
	layer := contract.InboundPolicy{GroupID: "exit", BlockedApps: []string{"trojan"}, Inspection: &contract.InspectionPolicy{Version: 1, Profiles: []string{"trojan"}, Mode: "strict"}}
	policyConfig := &contract.EffectivePolicy{Version: 2, InboundLayers: []contract.InboundPolicy{layer}}
	policy.Seal(policyConfig)
	service := contract.ServiceConfig{ID: "exit", Kind: "exit", GroupID: "exit", Listen: listen, Transport: "tls", Token: "carrier-token-long-123456", Profile: "carrier", Policy: policyConfig, Grants: []contract.ServiceGrant{{Identity: "identity", StreamToken: "stream-token-long-123456", Targets: []contract.ServiceTarget{{RuleID: "r", Network: "tcp", Target: target, Business: business}}}}}
	exit := &ServiceManager{Profiles: map[string]ServiceProfile{"carrier": carrier}, InspectionProfiles: detect.Profiles{"trojan": {Protocol: "trojan", Password: "known-business-password"}}, BusinessProfiles: map[string]BusinessProfile{"business": {CA: businessLocal.CA, ServerName: "localhost", AllowedTargets: []string{target}}}}
	commit, _, err := exit.Prepare([]contract.ServiceConfig{service}, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	commit()
	defer exit.Close()
	_, entry, cfg := setup(t)
	rule := testRule(target)
	rule.ID = "r"
	rule.Transport = "tls"
	rule.Tunnel = &contract.Tunnel{Endpoint: listen, ServerName: "localhost", Token: "stream-token-long-123456", Inspect: true, StagedInspection: true}
	rule.Business = business
	entry.Client.TLS = &tls.Config{RootCAs: roots}
	entryLocal := localProfile(t, pair, nil)
	entry.BusinessProfiles = map[string]BusinessProfile{"business": {Certificate: entryLocal.Certificate, PrivateKey: entryLocal.PrivateKey, CA: entryLocal.CA, ServerName: "localhost", AllowedListen: []string{rule.Listen}, AllowedTargets: []string{target}}}
	// The entry has no Trojan detector or password. Its legacy prefix is
	// insufficient; only the exit's actual-stream authentication can reject.
	cfg.Rules = []contract.Rule{rule}
	if err = entry.Apply(cfg, true); err != nil {
		t.Fatal(err)
	}
	client, err := tls.Dial("tcp", entry.listeners[key(rule)].tcp.Addr().String(), &tls.Config{RootCAs: roots, ServerName: "localhost"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	client.SetDeadline(time.Now().Add(2 * time.Second))
	client.Write(trojanBusinessHeader("known-business-password"))
	var b [1]byte
	if _, err = client.Read(b[:]); err == nil {
		t.Fatal("exit failed to reject independent Trojan stream")
	}
	if count.Load() != 0 {
		t.Fatal("staged exit connected to origin before actual confirmation")
	}
	statuses := exit.RuleStatuses()
	if len(statuses) != 1 || statuses[0].Rejected != 1 || statuses[0].DetectedProtocol != "trojan" {
		t.Fatal("missing independent exit evidence", statuses)
	}
}

func TestObserveGroupMirrorDoesNotBecomeStrictRule(t *testing.T) {
	_, runtime, cfg := setup(t)
	rule := testRule(managedEcho(t))
	rule.BlockedProtocols = []string{"socks5"}
	rule.EffectivePolicy = &contract.EffectivePolicy{Version: 2, InboundLayers: []contract.InboundPolicy{{GroupID: "entry", BlockedApps: []string{"socks5"}, Inspection: &contract.InspectionPolicy{Version: 1, Mode: "observe"}}}}
	policy.Seal(rule.EffectivePolicy)
	cfg.Rules = []contract.Rule{rule}
	if err := runtime.Apply(cfg, true); err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("tcp", runtime.listeners[key(rule)].tcp.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	c.Write([]byte{5, 1, 0})
	var b [3]byte
	if _, err = io.ReadFull(c, b[:]); err != nil {
		t.Fatal("observe mirror enforced as strict", err)
	}
}
