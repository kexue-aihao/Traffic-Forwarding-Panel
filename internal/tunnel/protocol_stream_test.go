package tunnel

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/policy/detect"
	ss "github.com/sagernet/sing-shadowsocks"
	ss2017 "github.com/sagernet/sing-shadowsocks/shadowaead"
	ss2022 "github.com/sagernet/sing-shadowsocks/shadowaead_2022"
	M "github.com/sagernet/sing/common/metadata"
	vnet "github.com/v2fly/v2ray-core/v5/common/net"
	"github.com/v2fly/v2ray-core/v5/common/protocol"
	vuuid "github.com/v2fly/v2ray-core/v5/common/uuid"
	"github.com/v2fly/v2ray-core/v5/proxy/vmess"
	vmencoding "github.com/v2fly/v2ray-core/v5/proxy/vmess/encoding"
)

func protocolStreamReference(t *testing.T, variant string) (detect.Profile, []byte) {
	t.Helper()
	if variant == "vmess-aead" {
		const uuid = "0581b063-cc43-4719-bb46-736235a2c3e9"
		id, err := vuuid.ParseString(uuid)
		if err != nil {
			t.Fatal(err)
		}
		request := &protocol.RequestHeader{Version: 1, Command: protocol.RequestCommandTCP, Security: protocol.SecurityType_NONE, Port: 443, Address: vnet.ParseAddress("example.test"), Option: protocol.RequestOptionChunkStream, User: &protocol.MemoryUser{Account: &vmess.MemoryAccount{ID: protocol.NewID(id)}}}
		var wire bytes.Buffer
		client := vmencoding.NewClientSession(context.Background(), true, protocol.DefaultIDHash, 0)
		if err = client.EncodeRequestHeader(request, &wire); err != nil {
			t.Fatal(err)
		}
		return detect.Profile{Protocol: "vmess", UUID: uuid}, wire.Bytes()
	}
	profile := detect.Profile{Protocol: "shadowsocks", Method: variant}
	var client ss.Method
	var err error
	if variant == "aes-128-gcm" {
		profile.Password = "independent-exit-stream-fixture"
		client, err = ss2017.New(variant, nil, profile.Password)
	} else {
		profile.Key = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x73}, 16))
		client, err = ss2022.NewWithPassword(variant, profile.Key, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	capture := new(protocolPacketCapture)
	stream, err := client.DialConn(capture, M.ParseSocksaddr("example.test:443"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = stream.Write([]byte("independent-business")); err != nil {
		t.Fatal(err)
	}
	return profile, append([]byte(nil), capture.Bytes()...)
}

// An entry's one-byte announcement cannot substitute for the exit's actual
// stream. Reference encrypted headers must be rejected without writing origin
// bytes; staged v4 must also avoid connecting to origin before its decision.
func TestProtocolStreamIndependentExitRejectsShortAnnouncement(t *testing.T) {
	for _, variant := range []string{"aes-128-gcm", "2022-blake3-aes-128-gcm", "vmess-aead"} {
		for _, staged := range []bool{false, true} {
			name := "v3"
			if staged {
				name = "v4"
			}
			t.Run(variant+"/"+name, func(t *testing.T) {
				profile, wire := protocolStreamReference(t, variant)
				origin, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { origin.Close() })
				var connections, received atomic.Uint64
				go func() {
					for {
						c, err := origin.Accept()
						if err != nil {
							return
						}
						connections.Add(1)
						go func() {
							defer c.Close()
							var b [2048]byte
							for {
								n, err := c.Read(b[:])
								received.Add(uint64(n))
								if err != nil {
									return
								}
							}
						}()
					}
				}()
				layers := []contract.InboundPolicy{{BlockedApps: []string{profile.Protocol}, Inspection: &contract.InspectionPolicy{Version: 1, Profiles: []string{"exit-local"}}}}
				target := origin.Addr().String()
				plan, err := detect.Prepare(layers, detect.Profiles{"exit-local": profile}, detect.Scope{RuleID: "stream-rule", Target: target, Network: "tcp"})
				if err != nil {
					t.Fatal(err)
				}
				pair, roots := testCertificate(t)
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				server := &Server{TLS: &tls.Config{Certificates: []tls.Certificate{pair}}, NodeID: "independent-stream-exit", Token: "independent-carrier-fixture-token", Managed: true, Policy: &contract.EffectivePolicy{Version: 2, InboundLayers: layers}, Grants: []contract.ServiceGrant{{StreamToken: "independent-stream-fixture-token", Targets: []contract.ServiceTarget{{RuleID: "stream-rule", Network: "tcp", Target: target}}}}, InspectionPlans: map[string]*detect.Plan{InspectionKey("stream-rule", "tcp", target): plan}}
				done := make(chan struct{})
				go func() { defer close(done); server.Serve(listener, "tls") }()
				t.Cleanup(func() { server.Close(); <-done })
				client := Client{TLS: &tls.Config{RootCAs: roots}, RuleID: "stream-rule", InspectionPrefix: wire[:1], Timeout: 2 * time.Second}
				session, err := client.DialRoute(context.Background(), "tls", "tcp", target, contract.Tunnel{Endpoint: listener.Addr().String(), ServerName: "localhost", Token: "independent-stream-fixture-token", Inspect: true, StagedInspection: staged})
				if err != nil {
					t.Fatal("short announcement should reach actual-stream gate", err)
				}
				defer session.Close()
				session.SetDeadline(time.Now().Add(3 * time.Second))
				if _, err = session.Write(wire); err != nil {
					t.Fatal(err)
				}
				var reply [1]byte
				if n, err := session.Read(reply[:]); n != 0 || err == nil {
					t.Fatal("prohibited business stream was forwarded", n, err)
				}
				statuses := server.PolicyStatuses()
				if len(statuses) != 1 || statuses[0].Rejected != 1 || statuses[0].DetectedProtocol != profile.Protocol || statuses[0].Evidence != string(detect.Authenticated) {
					t.Fatal("missing independently authenticated exit rejection", statuses)
				}
				if received.Load() != 0 || staged && connections.Load() != 0 {
					t.Fatalf("exit gate leaked origin traffic: bytes=%d connections=%d", received.Load(), connections.Load())
				}
			})
		}
	}
}
