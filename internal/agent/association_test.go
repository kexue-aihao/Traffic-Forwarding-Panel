package agent

import (
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/policy"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/policy/detect"
)

func associationListen(t *testing.T) string {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	address := c.LocalAddr().String()
	c.Close()
	return address
}

// This controlled origin exchanges a real TCP greeting, selected method,
// optional RFC1929 authentication, UDP ASSOCIATE, and a successful numeric BND.
// It does not cooperate with the registry or manufacture association proofs.
func associationOrigin(t *testing.T, endpoint string) (string, string, *atomic.Int64) {
	t.Helper()
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { udp.Close() })
	seen := &atomic.Int64{}
	go func() {
		b := make([]byte, 65535)
		for {
			n, peer, err := udp.ReadFromUDP(b)
			if err != nil {
				return
			}
			seen.Add(1)
			udp.WriteToUDP(b[:n], peer)
		}
	}()
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tcp.Close() })
	go func() {
		for {
			c, err := tcp.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				c.SetDeadline(time.Now().Add(5 * time.Second))
				var greeting [3]byte
				if _, err := io.ReadFull(c, greeting[:]); err != nil || greeting[0] != 5 || greeting[1] != 1 {
					return
				}
				c.Write([]byte{5, greeting[2]})
				if greeting[2] == 2 {
					var auth [5]byte
					if _, err := io.ReadFull(c, auth[:]); err != nil || !bytes.Equal(auth[:], []byte{1, 1, 'u', 1, 'p'}) {
						return
					}
					c.Write([]byte{1, 0})
				}
				var request [10]byte
				if _, err := io.ReadFull(c, request[:]); err != nil || !bytes.Equal(request[:4], []byte{5, 3, 0, 1}) {
					return
				}
				ap, _ := netip.ParseAddrPort(endpoint)
				ip := ap.Addr().As4()
				reply := append([]byte{5, 0, 0, 1}, ip[:]...)
				reply = binary.BigEndian.AppendUint16(reply, ap.Port())
				for _, b := range reply {
					if _, err := c.Write([]byte{b}); err != nil {
						return
					}
				}
				io.Copy(io.Discard, c)
			}()
		}
	}()
	return tcp.Addr().String(), udp.LocalAddr().String(), seen
}

func establishAssociation(t *testing.T, address string, method byte, sourcePort uint16) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(3 * time.Second))
	for _, b := range []byte{5, 1, method} {
		c.Write([]byte{b})
	}
	var methodReply [2]byte
	if _, err = io.ReadFull(c, methodReply[:]); err != nil || methodReply != [2]byte{5, method} {
		t.Fatal("method exchange failed", methodReply, err)
	}
	if method == 2 {
		c.Write([]byte{1, 1, 'u', 1, 'p'})
		if _, err = io.ReadFull(c, methodReply[:]); err != nil || methodReply != [2]byte{1, 0} {
			t.Fatal("authentication exchange failed", methodReply, err)
		}
	}
	request := binary.BigEndian.AppendUint16([]byte{5, 3, 0, 1, 0, 0, 0, 0}, sourcePort)
	c.Write(request)
	var reply [10]byte
	if _, err = io.ReadFull(c, reply[:]); err != nil {
		t.Fatal("UDP association exchange failed", err)
	}
	return c
}

func TestSOCKS5AssociatedUDPUsesActualControlSocketAndRevokes(t *testing.T) {
	for _, scenario := range []struct {
		method   byte
		tunneled bool
	}{{0, false}, {2, false}, {0, true}, {2, true}} {
		t.Run(fmt.Sprintf("method%d/tunneled%t", scenario.method, scenario.tunneled), func(t *testing.T) {
			method := scenario.method
			_, runtime, cfg := setup(t)
			endpoint := associationListen(t)
			controlTarget, udpTarget, seen := associationOrigin(t, endpoint)
			control := testRule(controlTarget)
			control.ID, control.UserID = "control", "user"
			udp := testRule(udpTarget)
			udp.ID, udp.UserID, udp.Network, udp.Listen = "udp-rule", "user", "udp", endpoint
			udp.Lease.ID = "udp-lease"
			udp.BlockedProtocols = []string{"socks5"} // Legacy group mirror must not weaken association evidence.
			layer := contract.InboundPolicy{GroupID: "udp-group", BlockedApps: []string{"socks5"}, Inspection: &contract.InspectionPolicy{Version: 1, Mode: "strict", Unknown: "allow", Profiles: []string{"controlled"}}}
			udp.EffectivePolicy = &contract.EffectivePolicy{Version: 2, InboundLayers: []contract.InboundPolicy{layer}, Failover: &contract.FailoverPolicy{MaxFail: 1, CooldownSec: 10}}
			control.EffectivePolicy = &contract.EffectivePolicy{Version: 2, Failover: &contract.FailoverPolicy{MaxFail: 1, CooldownSec: 10}}
			policy.Seal(control.EffectivePolicy)
			policy.Seal(udp.EffectivePolicy)
			runtime.InspectionProfiles = detect.Profiles{"controlled": {Protocol: "socks5", UDPMode: "associated", ControlRuleIDs: []string{control.ID}, RelayEndpoint: endpoint, RuleIDs: []string{udp.ID}, Targets: []string{udp.Target}}}
			if scenario.tunneled {
				pair, roots := chainCertificate(t)
				listen := freeTCP(t)
				local := localProfile(t, pair, []string{listen})
				service := contract.ServiceConfig{ID: "association-exit", Kind: "exit", GroupID: "exit", Listen: listen, Transport: "tls", Token: "carrier-token-long-123456", Profile: "carrier", Grants: []contract.ServiceGrant{{Identity: "identity", StreamToken: "stream-token-long-123456", Targets: []contract.ServiceTarget{{RuleID: control.ID, Network: "tcp", Target: control.Target}, {RuleID: udp.ID, Network: "udp", Target: udp.Target}}}}}
				exit := &ServiceManager{Profiles: map[string]ServiceProfile{"carrier": local}}
				commit, _, err := exit.Prepare([]contract.ServiceConfig{service}, time.Now().Add(time.Minute))
				if err != nil {
					t.Fatal(err)
				}
				commit()
				t.Cleanup(exit.Close)
				runtime.Client.TLS = &tls.Config{RootCAs: roots}
				control.Transport, udp.Transport = "tls", "tls"
				control.Tunnel = &contract.Tunnel{Endpoint: listen, ServerName: "localhost", Token: "stream-token-long-123456"}
				udp.Tunnel = &contract.Tunnel{Endpoint: listen, ServerName: "localhost", Token: "stream-token-long-123456"}
			}
			cfg.Rules = []contract.Rule{control, udp}
			if err := runtime.Apply(cfg, true); err != nil {
				t.Fatal(err)
			}
			c, err := net.Dial("udp", endpoint)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			payload := []byte{0, 0, 0, 1, 127, 0, 0, 1, 0, 53, 'x'}
			exchange := func() {
				c.SetDeadline(time.Now().Add(2 * time.Second))
				c.Write(payload)
				buf := make([]byte, len(payload))
				if n, err := c.Read(buf); err != nil || !bytes.Equal(buf[:n], payload) {
					t.Fatal("unassociated datagram was not preserved", err)
				}
			}
			exchange()
			port := c.LocalAddr().(*net.UDPAddr).AddrPort().Port()
			controlConn := establishAssociation(t, runtime.listeners[key(control)].tcp.Addr().String(), method, port)
			before := seen.Load()
			c.SetDeadline(time.Now().Add(200 * time.Millisecond))
			c.Write(payload)
			var b [32]byte
			if _, err = c.Read(b[:]); err == nil || seen.Load() != before {
				t.Fatal("associated SOCKS UDP reached origin", err, seen.Load(), before)
			}
			statuses := runtime.PolicyStatuses()
			found := false
			for _, s := range statuses {
				found = found || s.RuleID == udp.ID && s.Rejected > 0 && s.DetectedVariant == "socks5-udp-associated" && s.Evidence == "structural"
			}
			if !found {
				t.Fatal("missing actual association diagnostic", statuses)
			}
			other, err := net.Dial("udp", endpoint)
			if err != nil {
				t.Fatal(err)
			}
			other.SetDeadline(time.Now().Add(time.Second))
			other.Write(payload)
			if _, err = other.Read(b[:]); err != nil {
				t.Fatal("different socket was falsely associated", err)
			}
			other.Close()
			// Accounting renewal preserves the live control association.
			lease := *control.Lease
			lease.ID = "renewed-control-lease"
			control.Lease = &lease
			cfg.Rules, cfg.Version = []contract.Rule{control, udp}, cfg.Version+1
			if err = runtime.Apply(cfg, true); err != nil || runtime.Associations.Count() != 1 {
				t.Fatal("renewal revoked association", err, runtime.Associations.Count())
			}
			controlConn.Close()
			deadline := time.Now().Add(time.Second)
			for runtime.Associations.Count() != 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			exchange()
			controlConn = establishAssociation(t, runtime.listeners[key(control)].tcp.Addr().String(), method, port)
			// Any profile rotation revokes the physical proof before new packets.
			profile := runtime.InspectionProfiles["controlled"]
			profile.GroupIDs = []string{"udp-group"}
			runtime.InspectionProfiles["controlled"] = profile
			cfg.Version++
			if err = runtime.Apply(cfg, true); err != nil || runtime.Associations.Count() != 0 {
				t.Fatal("profile rotation retained stale association", err)
			}
			controlConn.Close()
			exchange()
		})
	}
}

func TestSOCKSAssociationPreparationRefusesUnsupportedControlScopes(t *testing.T) {
	_, runtime, cfg := setup(t)
	endpoint := associationListen(t)
	controlTarget, udpTarget, _ := associationOrigin(t, endpoint)
	control := testRule(controlTarget)
	control.ID, control.UserID = "control", "one"
	udp := testRule(udpTarget)
	udp.ID, udp.UserID, udp.Network, udp.Listen = "udp", "one", "udp", endpoint
	udp.Lease.ID = "udp-lease"
	udp.EffectivePolicy = &contract.EffectivePolicy{Version: 2, InboundLayers: []contract.InboundPolicy{{BlockedApps: []string{"socks5"}, Inspection: &contract.InspectionPolicy{Version: 1, Profiles: []string{"controlled"}, Mode: "strict"}}}}
	policy.Seal(udp.EffectivePolicy)
	runtime.InspectionProfiles = detect.Profiles{"controlled": {Protocol: "socks5", UDPMode: "associated", ControlRuleIDs: []string{control.ID}, RelayEndpoint: endpoint, RuleIDs: []string{udp.ID}, Targets: []string{udp.Target}}}
	control.UserID = "another-owner"
	cfg.Rules = []contract.Rule{control, udp}
	if err := runtime.Apply(cfg, true); err == nil || runtime.Version() != 0 {
		t.Fatal("cross-owner association accepted", err)
	}
	control.UserID = "one"
	control.BlockedProtocols = []string{"socks5"}
	cfg.Rules = []contract.Rule{control, udp}
	if err := runtime.Apply(cfg, true); err == nil || runtime.Version() != 0 {
		t.Fatal("blocked control association accepted", err)
	}
	control.BlockedProtocols = nil
	udp.Listen = "0.0.0.0:" + endpoint[strings.LastIndex(endpoint, ":")+1:]
	cfg.Rules = []contract.Rule{control, udp}
	if err := runtime.Apply(cfg, true); err == nil || runtime.Version() != 0 {
		t.Fatal("wildcard association accepted", err)
	}
}

func TestSOCKSAssociationObserveMissingControlRemainsUnavailableAndForwards(t *testing.T) {
	_, runtime, cfg := setup(t)
	endpoint := associationListen(t)
	_, target := limitsEcho(t)
	udp := testRule(target)
	udp.ID, udp.Network, udp.Listen = "observed-udp", "udp", endpoint
	udp.BlockedProtocols = []string{"socks5"}
	udp.EffectivePolicy = &contract.EffectivePolicy{Version: 2, InboundLayers: []contract.InboundPolicy{{BlockedApps: []string{"socks5"}, Inspection: &contract.InspectionPolicy{Version: 1, Mode: "observe", Unknown: "allow", Profiles: []string{"remote-control"}}}}}
	policy.Seal(udp.EffectivePolicy)
	runtime.InspectionProfiles = detect.Profiles{"remote-control": {Protocol: "socks5", UDPMode: "associated", ControlRuleIDs: []string{"another-agent-control"}, RelayEndpoint: endpoint, RuleIDs: []string{udp.ID}, Targets: []string{target}}}
	cfg.Rules = []contract.Rule{udp}
	if err := runtime.Apply(cfg, true); err != nil {
		t.Fatal("observe-only missing prerequisite rejected configuration", err)
	}
	c, err := net.Dial("udp", endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(time.Second))
	payload := []byte{0, 0, 0, 1, 127, 0, 0, 1, 0, 53, 'x'}
	c.Write(payload)
	var b [32]byte
	if n, err := c.Read(b[:]); err != nil || !bytes.Equal(b[:n], payload) {
		t.Fatal("observe-only unavailable association altered UDP forwarding", err)
	}
	statuses := runtime.PolicyStatuses()
	if len(statuses) != 1 || statuses[0].Unavailable != 1 || statuses[0].Rejected != 0 || statuses[0].InspectionReason != "socks5_udp_unassociated" || runtime.Associations.Count() != 0 {
		t.Fatal("missing honest unassociated diagnostic", statuses)
	}
}
