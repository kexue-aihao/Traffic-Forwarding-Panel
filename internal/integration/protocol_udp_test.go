package integration

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/policy/detect"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/tunnel"
	ss "github.com/sagernet/sing-shadowsocks"
	ss2017 "github.com/sagernet/sing-shadowsocks/shadowaead"
	ss2022 "github.com/sagernet/sing-shadowsocks/shadowaead_2022"
	M "github.com/sagernet/sing/common/metadata"
)

type udpInspectionCapture struct{ bytes.Buffer }

func (c *udpInspectionCapture) Read([]byte) (int, error)         { return 0, io.EOF }
func (c *udpInspectionCapture) Close() error                     { return nil }
func (c *udpInspectionCapture) LocalAddr() net.Addr              { return &net.UDPAddr{} }
func (c *udpInspectionCapture) RemoteAddr() net.Addr             { return &net.UDPAddr{} }
func (c *udpInspectionCapture) SetDeadline(time.Time) error      { return nil }
func (c *udpInspectionCapture) SetReadDeadline(time.Time) error  { return nil }
func (c *udpInspectionCapture) SetWriteDeadline(time.Time) error { return nil }

func udpInspectionClients(t *testing.T) (detect.Profiles, []string, [][]byte) {
	t.Helper()
	profiles := detect.Profiles{}
	var labels []string
	var packets [][]byte
	for _, method := range []string{"aes-128-gcm", "aes-256-gcm", "chacha20-ietf-poly1305", "2022-blake3-aes-128-gcm", "2022-blake3-aes-256-gcm", "2022-blake3-chacha20-poly1305"} {
		profile := detect.Profile{Protocol: "shadowsocks", Method: method}
		var client ss.Method
		var err error
		if strings.HasPrefix(method, "2022-") {
			n := 32
			if strings.Contains(method, "128") {
				n = 16
			}
			profile.Key = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x64}, n))
			client, err = ss2022.NewWithPassword(method, profile.Key, nil)
		} else {
			profile.Password = "udp-independent-client-fixture"
			client, err = ss2017.New(method, nil, profile.Password)
		}
		if err != nil {
			t.Fatal(err)
		}
		capture := new(udpInspectionCapture)
		packet := client.DialPacketConn(capture)
		if _, err = packet.WriteTo([]byte("real-udp-business"), M.ParseSocksaddr("example.test:443")); err != nil {
			t.Fatal(err)
		}
		profiles[method] = profile
		labels = append(labels, method)
		packets = append(packets, append([]byte(nil), capture.Bytes()...))
		packet.Close()
	}
	return profiles, labels, packets
}

func udpInspectionOrigin(t *testing.T) (string, *atomic.Uint64) {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	received := new(atomic.Uint64)
	go func() {
		buf := make([]byte, 65535)
		for {
			n, peer, err := c.ReadFrom(buf)
			if err != nil {
				return
			}
			received.Add(1)
			c.WriteTo(buf[:n], peer)
		}
	}()
	return c.LocalAddr().String(), received
}

func udpInspectionExchange(t *testing.T, c net.Conn, payload []byte) {
	t.Helper()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write(payload); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 65535)
	n, err := c.Read(buf)
	if err != nil || !bytes.Equal(buf[:n], payload) {
		t.Fatalf("UDP echo n=%d: %v", n, err)
	}
}

// A pinned independent client emits the business UDP packets; the real HTTP
// configuration, Agent, each carrier, origin counter and rejection diagnostics
// must agree. A client timeout alone cannot satisfy the blocking assertion.
func TestProtocolUDPAgentCarrierEveryPacketAndRestore(t *testing.T) {
	for _, carrier := range []string{"direct", "tls", "quic"} {
		t.Run(carrier, func(t *testing.T) {
			pair, roots := certificate(t)
			f := newFixture(t, tunnel.Client{TLS: &tls.Config{RootCAs: roots}})
			profiles, labels, packets := udpInspectionClients(t)
			f.agent.Runtime.InspectionProfiles = profiles
			f.sync() // Advertise local profile readiness before selecting it in policy.
			target, received := udpInspectionOrigin(t)
			var spec *contract.Tunnel
			switch carrier {
			case "tls":
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				server := &tunnel.Server{TLS: &tls.Config{Certificates: []tls.Certificate{pair}}, Token: "udp-agent-carrier-fixture-token"}
				done := make(chan struct{})
				go func() { defer close(done); server.Serve(listener, "tls") }()
				t.Cleanup(func() { server.Close(); <-done })
				spec = &contract.Tunnel{Endpoint: listener.Addr().String(), ServerName: "localhost", Token: server.Token}
			case "quic":
				server := &tunnel.DatagramServer{TLS: &tls.Config{Certificates: []tls.Certificate{pair}}, Token: "udp-agent-carrier-fixture-token"}
				if err := server.Listen("127.0.0.1:0"); err != nil {
					t.Fatal(err)
				}
				done := make(chan struct{})
				go func() { defer close(done); server.ServeBound() }()
				t.Cleanup(func() { server.Close(); <-done })
				spec = &contract.Tunnel{Endpoint: server.Addr().String(), ServerName: "localhost", Token: server.Token}
			}
			rule := f.rule("udp", carrier, target, spec)
			f.sync()
			for _, wire := range packets {
				transfer(t, rule, wire)
			}
			if received.Load() != uint64(len(packets)) {
				t.Fatal("off-policy packets did not reach origin")
			}
			setAdvanced(t, f, &contract.GroupAdvanced{BlockedProtocol: []string{"shadowsocks"}, Inspection: &contract.InspectionPolicy{Version: 1, Profiles: labels}})
			if len(f.store.Config().Rules) != 1 {
				t.Fatalf("UDP policy preparation failed: %+v", f.store.Config().BlockedRules)
			}
			for index, wire := range packets {
				t.Run(labels[index], func(t *testing.T) {
					c, err := net.DialTimeout("udp", rule.Listen, time.Second)
					if err != nil {
						t.Fatal(err)
					}
					defer c.Close()
					udpInspectionExchange(t, c, []byte("unknown-first-datagram"))
					badTag := append([]byte(nil), wire...)
					badTag[len(badTag)-1] ^= 1
					udpInspectionExchange(t, c, badTag)
					before := received.Load()
					drops := f.agent.Runtime.UDPStats()[rule.ID].PolicyDrops
					if _, err = c.Write(wire); err != nil {
						t.Fatal(err)
					}
					deadline := time.Now().Add(3 * time.Second)
					for f.agent.Runtime.UDPStats()[rule.ID].PolicyDrops == drops && time.Now().Before(deadline) {
						time.Sleep(time.Millisecond)
					}
					if f.agent.Runtime.UDPStats()[rule.ID].PolicyDrops != drops+1 {
						t.Fatal("authenticated packet was not actually rejected")
					}
					statuses := f.agent.Runtime.PolicyStatuses()
					if len(statuses) != 1 || statuses[0].Rejected == 0 || statuses[0].DetectedProtocol != "shadowsocks" || statuses[0].Evidence != string(detect.Authenticated) {
						t.Fatalf("missing actual protocol rejection: %+v", statuses)
					}
					c.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
					if n, _ := c.Read(make([]byte, 65535)); n != 0 {
						t.Fatal("prohibited UDP packet echoed")
					}
					if received.Load() != before {
						t.Fatal("prohibited UDP packet reached origin")
					}
					udpInspectionExchange(t, c, []byte("allowed-after-rejection"))
				})
			}
			setAdvanced(t, f, nil)
			before := received.Load()
			for _, wire := range packets {
				transfer(t, rule, wire)
			}
			if received.Load() != before+uint64(len(packets)) {
				t.Fatal("restored UDP policy did not reach origin")
			}
		})
	}
}
