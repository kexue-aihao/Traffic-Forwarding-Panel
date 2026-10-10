package detect_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
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

// Frames below are emitted by independent, pinned client implementations, not
// a second copy of our detector's encoders or handcrafted authenticated bytes.
type wireCapture struct{ bytes.Buffer }

func (c *wireCapture) Read(b []byte) (int, error)       { return 0, io.EOF }
func (c *wireCapture) Close() error                     { return nil }
func (c *wireCapture) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c *wireCapture) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c *wireCapture) SetDeadline(time.Time) error      { return nil }
func (c *wireCapture) SetReadDeadline(time.Time) error  { return nil }
func (c *wireCapture) SetWriteDeadline(time.Time) error { return nil }

func testPlan(t *testing.T, p detect.Profile, network, visibility, mode string) *detect.Plan {
	t.Helper()
	plan, e := detect.Prepare([]contract.InboundPolicy{{BlockedApps: []string{p.Protocol}, Inspection: &contract.InspectionPolicy{Version: 1, Profiles: []string{"fixture"}, Mode: mode}}}, detect.Profiles{"fixture": p}, detect.Scope{Network: network, Visibility: visibility})
	if e != nil {
		t.Fatal(e)
	}
	return plan
}

func checkAuthenticated(t *testing.T, plan *detect.Plan, wire []byte, network string) {
	t.Helper()
	d := plan.Feed(wire, true, network)
	if d.Status != detect.Match || d.Evidence != detect.Authenticated {
		t.Fatalf("reference client frame was not authenticated: %+v", d)
	}
	if !errors.Is(plan.Decision(d), detect.ErrDenied) {
		t.Fatal("authenticated blocked application permitted")
	}
	if network == "tcp" {
		s := plan.NewSession()
		for n := 1; n <= len(wire); n++ {
			d = s.Feed(wire[:n], false, network)
			if d.Status == detect.Match {
				break
			}
			if d.Status != detect.NeedMore {
				t.Fatalf("fragment %d/%d: %+v", n, len(wire), d)
			}
		}
		if d.Status != detect.Match || d.Evidence != detect.Authenticated {
			t.Fatalf("fragmented reference client: %+v", d)
		}
	}
}

func TestPinnedShadowsocksClientsTCPUDP(t *testing.T) {
	methods := []string{"aes-128-gcm", "aes-256-gcm", "chacha20-ietf-poly1305", "2022-blake3-aes-128-gcm", "2022-blake3-aes-256-gcm", "2022-blake3-chacha20-poly1305"}
	for _, method := range methods {
		for _, destination := range []string{"127.0.0.1:443", "[::1]:443", "example.test:443"} {
			t.Run(method+"/"+destination, func(t *testing.T) {
				p := detect.Profile{Protocol: "shadowsocks", Method: method}
				var ref ss.Method
				var e error
				if strings.HasPrefix(method, "2022-") {
					size := 32
					if strings.Contains(method, "128") {
						size = 16
					}
					p.Key = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x41}, size))
					ref, e = ss2022.NewWithPassword(method, p.Key, nil)
				} else {
					p.Password = "interop-password"
					ref, e = ss2017.New(method, nil, p.Password)
				}
				if e != nil {
					t.Fatal(e)
				}
				c := new(wireCapture)
				wrapped, e := ref.DialConn(c, M.ParseSocksaddr(destination))
				if e != nil {
					t.Fatal(e)
				}
				if _, e = wrapped.Write([]byte("business-payload")); e != nil {
					t.Fatal(e)
				}
				checkAuthenticated(t, testPlan(t, p, "tcp", "raw", "strict"), append([]byte(nil), c.Bytes()...), "tcp")
				c = new(wireCapture)
				packet := ref.DialPacketConn(c)
				if _, e = packet.WriteTo([]byte("business-payload"), M.ParseSocksaddr(destination)); e != nil {
					t.Fatal(e)
				}
				wire := append([]byte(nil), c.Bytes()...)
				plan := testPlan(t, p, "udp", "raw", "strict")
				checkAuthenticated(t, plan, wire, "udp")
				wire[len(wire)-1] ^= 0x01
				if d := plan.Feed(wire, true, "udp"); d.Status == detect.Match {
					t.Fatalf("corrupted tag became protocol evidence: %+v", d)
				}
			})
		}
	}
}

func TestPinnedShadowsocksSIP023TCPUDP(t *testing.T) {
	for _, method := range []string{"2022-blake3-aes-128-gcm", "2022-blake3-aes-256-gcm"} {
		for _, count := range []int{2, 3, 4} {
			t.Run(fmt.Sprintf("%s/%d", method, count), func(t *testing.T) {
				size := 32
				if strings.Contains(method, "128") {
					size = 16
				}
				keys := make([]string, count)
				for i := range keys {
					keys[i] = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{byte(0x50 + i)}, size))
				}
				p := detect.Profile{Protocol: "shadowsocks", Method: method, Key: strings.Join(keys, ":")}
				ref, e := ss2022.NewWithPassword(method, p.Key, nil)
				if e != nil {
					t.Fatal(e)
				}
				c := new(wireCapture)
				if _, e = ref.DialConn(c, M.ParseSocksaddr("example.test:443")); e != nil {
					t.Fatal(e)
				}
				checkAuthenticated(t, testPlan(t, p, "tcp", "raw", "strict"), append([]byte(nil), c.Bytes()...), "tcp")
				c = new(wireCapture)
				if _, e = ref.DialPacketConn(c).WriteTo([]byte("payload"), M.ParseSocksaddr("[::1]:443")); e != nil {
					t.Fatal(e)
				}
				plan := testPlan(t, p, "udp", "raw", "strict")
				wire := append([]byte(nil), c.Bytes()...)
				checkAuthenticated(t, plan, wire, "udp")
				wire[16] ^= 0x01
				if d := plan.Feed(wire, true, "udp"); d.Status == detect.Match {
					t.Fatalf("identity modification passed: %+v", d)
				}
			})
		}
	}
}

const fixtureUUID = "0581b063-cc43-4719-bb46-736235a2c3e9"

func referenceVMess(t *testing.T, aead bool, security protocol.SecurityType, command protocol.RequestCommand, address string) []byte {
	t.Helper()
	id, e := vuuid.ParseString(fixtureUUID)
	if e != nil {
		t.Fatal(e)
	}
	request := &protocol.RequestHeader{Version: 1, Command: command, Security: security, Port: 443, Address: vnet.ParseAddress(address), Option: protocol.RequestOptionChunkStream, User: &protocol.MemoryUser{Account: &vmess.MemoryAccount{ID: protocol.NewID(id)}}}
	var wire bytes.Buffer
	c := vmencoding.NewClientSession(context.Background(), aead, protocol.DefaultIDHash, 0)
	if e = c.EncodeRequestHeader(request, &wire); e != nil {
		t.Fatal(e)
	}
	return wire.Bytes()
}

func TestPinnedVMessClientHeaders(t *testing.T) {
	p := detect.Profile{Protocol: "vmess", UUID: fixtureUUID, Method: "aead"}
	plan := testPlan(t, p, "tcp", "raw", "strict")
	for _, security := range []protocol.SecurityType{protocol.SecurityType_AES128_GCM, protocol.SecurityType_CHACHA20_POLY1305, protocol.SecurityType_NONE, protocol.SecurityType_ZERO} {
		for _, cmd := range []protocol.RequestCommand{protocol.RequestCommandTCP, protocol.RequestCommandUDP, protocol.RequestCommandMux} {
			for _, addr := range []string{"127.0.0.1", "::1", "example.test"} {
				t.Run(fmt.Sprintf("%d/%d/%s", security, cmd, addr), func(t *testing.T) {
					wire := referenceVMess(t, true, security, cmd, addr)
					checkAuthenticated(t, plan, wire, "tcp")
					wire[len(wire)-1] ^= 1
					if d := plan.Feed(wire, true, "tcp"); d.Status == detect.Match {
						t.Fatalf("bad VMess authentication tag matched: %+v", d)
					}
					wrong := p
					wrong.UUID = "a3708488-9a55-48cd-aa7f-aa3f8463a149"
					if d := testPlan(t, wrong, "tcp", "raw", "strict").Feed(wire, true, "tcp"); d.Status == detect.Match {
						t.Fatalf("unknown UUID matched: %+v", d)
					}
				})
			}
		}
	}
}

func TestPinnedVMessLegacyIsDiagnosticOnly(t *testing.T) {
	p := detect.Profile{Protocol: "vmess", UUID: fixtureUUID, Method: "legacy"}
	plan := testPlan(t, p, "tcp", "raw", "observe")
	wire := referenceVMess(t, false, protocol.SecurityType_AES128_GCM, protocol.RequestCommandTCP, "example.test")
	d := plan.Feed(wire, true, "tcp")
	if d.Status != detect.Match || d.Evidence != detect.LegacyAuth || plan.Decision(d) != nil {
		t.Fatalf("legacy diagnostic: %+v", d)
	}
	if _, e := detect.Prepare([]contract.InboundPolicy{{BlockedApps: []string{"vmess"}, Inspection: &contract.InspectionPolicy{Version: 1, Profiles: []string{"legacy"}}}}, detect.Profiles{"legacy": p}, detect.Scope{Network: "tcp"}); e == nil {
		t.Fatal("legacy incorrectly prepared as reliable block")
	}
}
