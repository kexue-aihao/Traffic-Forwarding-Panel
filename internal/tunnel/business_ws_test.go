package tunnel

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/policy/detect"
)

func wsTestFrame(opcode byte, fin bool, payload []byte) []byte {
	first := opcode
	if fin {
		first |= 128
	}
	mask := [4]byte{3, 5, 7, 11}
	p := []byte{first, 128 | byte(len(payload))}
	p = append(p, mask[:]...)
	for i, b := range payload {
		p = append(p, b^mask[i%4])
	}
	return p
}

func testWSGate(t *testing.T, plan *detect.Plan, business *contract.BusinessInbound, rawCapture ...bool) (net.Conn, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	origin, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { origin.Close() })
	handshakes := &atomic.Int64{}
	bytesSeen := &atomic.Int64{}
	go func() {
		for {
			c, err := origin.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				c.SetDeadline(time.Now().Add(8 * time.Second))
				r := bufio.NewReader(c)
				header, err := wsHeader(r)
				if err != nil {
					return
				}
				req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(header)))
				if err != nil {
					return
				}
				handshakes.Add(1)
				sum := sha1.Sum([]byte(req.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
				fmt.Fprintf(c, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(sum[:]))
				if len(rawCapture) > 0 && rawCapture[0] {
					var b [1024]byte
					for {
						n, err := r.Read(b[:])
						bytesSeen.Add(int64(n))
						if err != nil {
							return
						}
						c.Write(b[:n])
					}
				}
				for {
					raw, _, _, _, err := readInspectionWSFrame(r)
					if err != nil {
						return
					}
					bytesSeen.Add(int64(len(raw)))
					c.Write(raw)
				}
			}()
		}
	}()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer client.Close()
				target, err := net.Dial("tcp", origin.Addr().String())
				if err != nil {
					return
				}
				defer target.Close()
				c, s := WebSocketBusinessInspection(client, target, nil, plan, business, nil)
				idle := time.Second
				if len(rawCapture) > 0 && rawCapture[0] {
					idle = 8 * time.Second
				}
				Relay(c, s, idle, nil)
			}()
		}
	}()
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	client.SetDeadline(time.Now().Add(2 * time.Second))
	return client, handshakes, bytesSeen
}
func wsTestRequest(subprotocol string) []byte {
	extra := ""
	if subprotocol != "" {
		extra = "Sec-WebSocket-Protocol: " + subprotocol + "\r\n"
	}
	return []byte("GET /socket HTTP/1.1\r\nHost: localhost\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n" + extra + "\r\n")
}
func wsSOCKSPlan(t *testing.T, mode string) *detect.Plan {
	t.Helper()
	p, err := detect.Prepare([]contract.InboundPolicy{{BlockedApps: []string{"socks5"}, Inspection: &contract.InspectionPolicy{Version: 1, Mode: mode}}}, nil, detect.Scope{Network: "tcp", Visibility: "ws-payload"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestWebSocketPartialFrameRetainsOnlyActualWireBytes(t *testing.T) {
	frame := wsTestFrame(2, true, []byte{5, 1, 0})
	for cut := 1; cut < len(frame); cut++ {
		wire, _, _, _, err := readInspectionWSFrame(bufio.NewReader(bytes.NewReader(frame[:cut])))
		if err == nil || !bytes.Equal(wire, frame[:cut]) {
			t.Fatalf("cut %d lost or invented consumed bytes: %x err=%v", cut, wire, err)
		}
	}
}

func TestWebSocketPartialFrameCannotReleaseAuthenticatedTrojanHeader(t *testing.T) {
	for _, eof := range []bool{true, false} {
		t.Run(fmt.Sprintf("eof%t", eof), func(t *testing.T) {
			password := "partial-frame-authenticated-password"
			layer := contract.InboundPolicy{BlockedApps: []string{"trojan"}, Inspection: &contract.InspectionPolicy{Version: 1, Mode: "strict", Unknown: "allow", Profiles: []string{"trojan"}}}
			plan, err := detect.Prepare([]contract.InboundPolicy{layer}, detect.Profiles{"trojan": {Protocol: "trojan", Password: password}}, detect.Scope{Network: "tcp", Visibility: "ws-payload"})
			if err != nil {
				t.Fatal(err)
			}
			client, handshakes, seen := testWSGate(t, plan, &contract.BusinessInbound{WebSocket: true}, true)
			client.SetDeadline(time.Now().Add(7 * time.Second))
			client.Write(wsTestRequest("chat"))
			r := bufio.NewReader(client)
			if _, err = wsHeader(r); err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum224([]byte(password))
			payload := []byte(hex.EncodeToString(sum[:]) + "\r\n")
			payload = append(payload, 1, 1, 127, 0, 0, 1, 0, 80, '\r', '\n')
			// The declared frame includes ten unsent bytes after an entire valid
			// authenticated protocol header. EOF/deadline cannot turn it unknown.
			frame := wsTestFrame(2, true, append(payload, make([]byte, 10)...))
			client.Write(frame[:len(frame)-10])
			if eof {
				client.(*net.TCPConn).CloseWrite()
			}
			var b [1]byte
			if n, err := r.Read(b[:]); n != 0 || err == nil {
				t.Fatal("partial authenticated frame was released", n, err)
			} else if e, ok := err.(net.Error); ok && e.Timeout() {
				t.Fatal("client deadline expired instead of inspection rejecting")
			}
			if handshakes.Load() != 1 || seen.Load() != 0 {
				t.Fatal("original partial frame reached origin", handshakes.Load(), seen.Load())
			}
		})
	}
}

func TestWebSocketGateChecksFragmentedInnerHeaderAndReplaysExactlyOnce(t *testing.T) {
	for _, mode := range []string{"strict", "observe"} {
		t.Run(mode, func(t *testing.T) {
			client, handshakes, seen := testWSGate(t, wsSOCKSPlan(t, mode), &contract.BusinessInbound{WebSocket: true})
			client.Write(wsTestRequest("chat"))
			r := bufio.NewReader(client)
			if _, err := wsHeader(r); err != nil {
				t.Fatal(err)
			}
			first := wsTestFrame(2, false, []byte{5})
			second := wsTestFrame(0, true, []byte{1, 0})
			client.Write(first)
			client.Write(second)
			if mode == "strict" {
				var b [1]byte
				if _, err := r.Read(b[:]); err == nil {
					t.Fatal("fragmented inner greeting escaped")
				}
				if seen.Load() != 0 {
					t.Fatal("origin received blocked raw frame")
				}
			} else {
				reply := make([]byte, len(first)+len(second))
				if _, err := io.ReadFull(r, reply); err != nil || !bytes.Equal(reply, append(first, second...)) {
					t.Fatal("original mask/fragments were changed or repeated", err)
				}
				if seen.Load() != int64(len(reply)) {
					t.Fatal("frame duplication")
				}
			}
			if handshakes.Load() != 1 {
				t.Fatal("handshake did not reach origin")
			}
		})
	}
}

func TestWebSocketGateTextAndPingRemainUsable(t *testing.T) {
	client, _, seen := testWSGate(t, wsSOCKSPlan(t, "strict"), &contract.BusinessInbound{WebSocket: true})
	client.Write(wsTestRequest("chat"))
	r := bufio.NewReader(client)
	if _, err := wsHeader(r); err != nil {
		t.Fatal(err)
	}
	ping := wsTestFrame(9, true, []byte("ping"))
	client.Write(ping)
	reply := make([]byte, len(ping))
	if _, err := io.ReadFull(r, reply); err != nil || !bytes.Equal(reply, ping) {
		t.Fatal("ping stalled behind detector", err)
	}
	text := wsTestFrame(1, true, []byte("ordinary text"))
	client.Write(text)
	reply = make([]byte, len(text))
	if _, err := io.ReadFull(r, reply); err != nil || !bytes.Equal(reply, text) {
		t.Fatal("normal text rejected", err)
	}
	if seen.Load() != int64(len(ping)+len(text)) {
		t.Fatal("unexpected origin wire count")
	}
}

func TestWebSocketEarlyDataCannotEscapeInUpgradeHeader(t *testing.T) {
	for _, data := range [][]byte{{5}, {5, 1, 0}} {
		t.Run(fmt.Sprintf("length%d", len(data)), func(t *testing.T) {
			client, handshakes, seen := testWSGate(t, wsSOCKSPlan(t, "strict"), &contract.BusinessInbound{WebSocket: true, WebSocketEarlyData: true})
			client.Write(wsTestRequest(base64.RawURLEncoding.EncodeToString(data)))
			var b [1]byte
			if _, err := client.Read(b[:]); err == nil {
				t.Fatal("early data upgrade unexpectedly forwarded")
			}
			if handshakes.Load() != 0 || seen.Load() != 0 {
				t.Fatal("early data leaked before upgrade")
			}
		})
	}
}

func TestManagedWebSocketExitVerifiesActualFrames(t *testing.T) {
	pair, roots := testCertificate(t)
	origin, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer origin.Close()
	seen := &atomic.Int64{}
	go func() {
		for {
			c, err := origin.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				c.SetDeadline(time.Now().Add(3 * time.Second))
				r := bufio.NewReader(c)
				header, err := wsHeader(r)
				if err != nil {
					return
				}
				req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(header)))
				if err != nil {
					return
				}
				sum := sha1.Sum([]byte(req.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
				fmt.Fprintf(c, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(sum[:]))
				for {
					raw, _, _, _, err := readInspectionWSFrame(r)
					if err != nil {
						return
					}
					seen.Add(int64(len(raw)))
					c.Write(raw)
				}
			}()
		}
	}()
	for _, mode := range []string{"strict", "observe"} {
		t.Run(mode, func(t *testing.T) {
			layer := contract.InboundPolicy{GroupID: "exit", BlockedApps: []string{"socks5"}, Inspection: &contract.InspectionPolicy{Version: 1, Mode: mode}}
			plan, err := detect.Prepare([]contract.InboundPolicy{layer}, nil, detect.Scope{Network: "tcp", Visibility: "ws-payload"})
			if err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			business := &contract.BusinessInbound{WebSocket: true}
			token := "managed-ws-stream-token-long-123"
			server := &Server{Managed: true, TLS: &tls.Config{Certificates: []tls.Certificate{pair}}, NodeID: "ws-exit", Token: "carrier-token-long-123456", Policy: &contract.EffectivePolicy{Version: 2, InboundLayers: []contract.InboundPolicy{layer}}, InspectionPlans: map[string]*detect.Plan{InspectionKey("r", "tcp", origin.Addr().String()): plan}, Grants: []contract.ServiceGrant{{StreamToken: token, Targets: []contract.ServiceTarget{{RuleID: "r", Network: "tcp", Target: origin.Addr().String(), Business: business}}}}}
			go server.Serve(listener, "tls")
			defer server.Close()
			client := Client{TLS: &tls.Config{RootCAs: roots}, RuleID: "r", Business: business, InspectionEnabled: true, InspectionPrefix: wsTestRequest("chat")}
			session, err := client.Dial(context.Background(), "tls", listener.Addr().String(), "localhost", token, "tcp", origin.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			session.SetDeadline(time.Now().Add(2 * time.Second))
			session.Write(wsTestRequest("chat"))
			r := bufio.NewReader(session)
			if _, err := wsHeader(r); err != nil {
				t.Fatal(err)
			}
			before := seen.Load()
			frame := wsTestFrame(2, true, []byte{5, 1, 0})
			session.Write(frame)
			if mode == "strict" {
				var b [1]byte
				if _, err := r.Read(b[:]); err == nil {
					t.Fatal("managed exit leaked inner header")
				}
				if seen.Load() != before {
					t.Fatal("origin saw denied masked frame")
				}
				statuses := server.PolicyStatuses()
				if len(statuses) != 1 || statuses[0].Rejected != 1 || statuses[0].DetectedProtocol != "socks5" {
					t.Fatal("missing exit frame evidence", statuses)
				}
			} else {
				reply := make([]byte, len(frame))
				if _, err := io.ReadFull(r, reply); err != nil || !bytes.Equal(reply, frame) {
					t.Fatal("managed WS observe altered original frame", err)
				}
			}
			// A client cannot choose a different business mode than its local grant.
			client.Business = nil
			if invalid, e := client.Dial(context.Background(), "tls", listener.Addr().String(), "localhost", token, "tcp", origin.Addr().String()); e == nil {
				invalid.Close()
				t.Fatal("business grant binding omitted")
			}
		})
	}
}
