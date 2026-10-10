package tunnel

import (
	"bytes"
	"context"
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
	ss "github.com/sagernet/sing-shadowsocks"
	ss2017 "github.com/sagernet/sing-shadowsocks/shadowaead"
	ss2022 "github.com/sagernet/sing-shadowsocks/shadowaead_2022"
	M "github.com/sagernet/sing/common/metadata"
)

type protocolPacketCapture struct{ bytes.Buffer }

func (c *protocolPacketCapture) Read([]byte) (int, error)         { return 0, io.EOF }
func (c *protocolPacketCapture) Close() error                     { return nil }
func (c *protocolPacketCapture) LocalAddr() net.Addr              { return &net.UDPAddr{} }
func (c *protocolPacketCapture) RemoteAddr() net.Addr             { return &net.UDPAddr{} }
func (c *protocolPacketCapture) SetDeadline(time.Time) error      { return nil }
func (c *protocolPacketCapture) SetReadDeadline(time.Time) error  { return nil }
func (c *protocolPacketCapture) SetWriteDeadline(time.Time) error { return nil }

func protocolUDPReference(t testing.TB, method string) (detect.Profile, []byte) {
	t.Helper()
	profile := detect.Profile{Protocol: "shadowsocks", Method: method}
	var client ss.Method
	var err error
	if strings.HasPrefix(method, "2022-") {
		size := 32
		if strings.Contains(method, "128") {
			size = 16
		}
		profile.Key = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x63}, size))
		client, err = ss2022.NewWithPassword(method, profile.Key, nil)
	} else {
		profile.Password = "independent-exit-udp-password"
		client, err = ss2017.New(method, nil, profile.Password)
	}
	if err != nil {
		t.Fatal(err)
	}
	capture := new(protocolPacketCapture)
	packet := client.DialPacketConn(capture)
	defer packet.Close()
	if _, err = packet.WriteTo([]byte("udp-origin-business"), M.ParseSocksaddr("example.test:443")); err != nil {
		t.Fatal(err)
	}
	return profile, append([]byte(nil), capture.Bytes()...)
}

func protocolUDPPlan(t testing.TB, profile detect.Profile, target string) *detect.Plan {
	t.Helper()
	plan, err := detect.Prepare([]contract.InboundPolicy{{BlockedApps: []string{"shadowsocks"}, Inspection: &contract.InspectionPolicy{Version: 1, Profiles: []string{"exit-local"}}}}, detect.Profiles{"exit-local": profile}, detect.Scope{RuleID: "udp-rule", Target: target, Network: "udp", Visibility: "raw"})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func protocolUDPOrigin(t testing.TB) (string, *atomic.Uint64) {
	t.Helper()
	listener, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	count := new(atomic.Uint64)
	t.Cleanup(func() { listener.Close() })
	go func() {
		buf := make([]byte, 65535)
		for {
			n, peer, err := listener.ReadFrom(buf)
			if err != nil {
				return
			}
			count.Add(1)
			listener.WriteTo(buf[:n], peer)
		}
	}()
	return listener.LocalAddr().String(), count
}

// Packet transport adapter keeps framed UDP-over-TCP and QUIC DATAGRAM tests
// identical without mistaking the carrier stream's Read for a UDP packet.
type protocolUDPFlow struct {
	send     func([]byte) error
	receive  func() ([]byte, error)
	deadline func(time.Time) error
	close    func() error
}

func protocolSessionFlow(s *Session) protocolUDPFlow {
	return protocolUDPFlow{send: s.WritePacket, receive: s.ReadPacket, deadline: s.SetDeadline, close: s.Close}
}

func protocolDatagramFlow(s *DatagramSession) protocolUDPFlow {
	return protocolUDPFlow{
		send:     func(p []byte) error { _, e := s.Write(p); return e },
		receive:  func() ([]byte, error) { buf := make([]byte, 65535); n, e := s.Read(buf); return buf[:n], e },
		deadline: s.SetDeadline, close: s.Close,
	}
}

func protocolUDPExchange(t *testing.T, flow protocolUDPFlow, payload []byte) {
	t.Helper()
	flow.deadline(time.Now().Add(3 * time.Second))
	if err := flow.send(payload); err != nil {
		t.Fatal(err)
	}
	got, err := flow.receive()
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("UDP echo %d/%d: %v", len(got), len(payload), err)
	}
}

func protocolUDPBlock(t *testing.T, flow protocolUDPFlow, packet []byte, origin, rejected *atomic.Uint64) {
	t.Helper()
	protocolUDPExchange(t, flow, []byte("unknown-first-packet"))
	bad := append([]byte(nil), packet...)
	bad[len(bad)-1] ^= 1
	protocolUDPExchange(t, flow, bad)
	before, rejectedBefore := origin.Load(), rejected.Load()
	if err := flow.send(packet); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for rejected.Load() == rejectedBefore && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if rejected.Load() != rejectedBefore+1 {
		t.Fatal("exit did not report an actual authenticated UDP rejection")
	}
	flow.deadline(time.Now().Add(30 * time.Millisecond))
	if got, _ := flow.receive(); len(got) > 0 {
		t.Fatal("prohibited UDP packet echoed")
	}
	if origin.Load() != before {
		t.Fatal("prohibited UDP packet reached origin")
	}
}

// The entry has no inspection plan. Only the independently configured exit
// knows the local credential; actual UDP bytes at either chain hop or the
// reverse exit must be checked even when prior packets were unknown.
func TestProtocolUDPIndependentExitTLSChainReverse(t *testing.T) {
	for _, route := range []string{"tls", "chain", "chain-middle", "reverse"} {
		for _, method := range []string{"aes-128-gcm", "2022-blake3-aes-128-gcm"} {
			t.Run(route+"/"+method, func(t *testing.T) {
				profile, wire := protocolUDPReference(t, method)
				origin, received := protocolUDPOrigin(t)
				for _, phase := range []string{"off", "on", "restore"} {
					t.Run(phase, func(t *testing.T) {
						pair, roots := testCertificate(t)
						rejected := new(atomic.Uint64)
						client := Client{TLS: &tls.Config{RootCAs: roots}, RuleID: "udp-rule", Timeout: time.Second}
						var plan *detect.Plan
						if phase == "on" {
							plan = protocolUDPPlan(t, profile, origin)
						}
						var inspectingExit *Server
						configure := func(s *Server) {
							inspectingExit = s
							s.InspectionPlans = map[string]*detect.Plan{InspectionKey("udp-rule", "udp", origin): plan}
							s.OnPolicyReject = func(id string) {
								if id == "udp-rule" {
									rejected.Add(1)
								}
							}
						}
						var dial func() (*Session, error)
						ctx, cancel := context.WithCancel(context.Background())
						defer cancel()
						switch route {
						case "chain", "chain-middle":
							f := setupChain(t, []string{"tls", "tls"}, pair, roots, func(f *chainFixture) {
								index := 1
								if route == "chain-middle" {
									index = 0
								}
								configure(f.servers[index])
								f.client.RuleID = "udp-rule"
							})
							dial = func() (*Session, error) { return f.dial(ctx, "udp", origin) }
						case "tls", "reverse":
							listener, err := net.Listen("tcp", "127.0.0.1:0")
							if err != nil {
								t.Fatal(err)
							}
							server := &Server{TLS: &tls.Config{Certificates: []tls.Certificate{pair}}, Token: "udp-exit-fixture-token", NodeID: "reverse-udp-exit"}
							if route == "tls" {
								configure(server)
							} else {
								server.ReverseAllowed = map[string]bool{"reverse-udp-exit": true}
							}
							done := make(chan struct{})
							go func() { defer close(done); server.Serve(listener, "tls") }()
							t.Cleanup(func() { server.Close(); <-done })
							spec := contract.Tunnel{Endpoint: listener.Addr().String(), ServerName: "localhost", Token: server.Token}
							if route == "reverse" {
								exit := &Server{Token: server.Token, NodeID: "reverse-udp-exit"}
								configure(exit)
								spec.Reverse = "reverse-udp-exit"
								reverseDone := make(chan struct{})
								go func() {
									defer close(reverseDone)
									exit.RunReverse(ctx, client, "tls", listener.Addr().String(), "localhost", "reverse-udp-exit")
								}()
								t.Cleanup(func() { cancel(); <-reverseDone })
							}
							dial = func() (*Session, error) { return client.DialRoute(ctx, "tls", "udp", origin, spec) }
						}
						var session *Session
						var err error
						deadline := time.Now().Add(3 * time.Second)
						for time.Now().Before(deadline) {
							session, err = dial()
							if err == nil {
								break
							}
							if route != "reverse" {
								t.Fatal(err)
							}
							time.Sleep(time.Millisecond)
						}
						if err != nil {
							t.Fatal(err)
						}
						flow := protocolSessionFlow(session)
						defer flow.close()
						if phase == "on" {
							protocolUDPBlock(t, flow, wire, received, rejected)
							statuses := inspectingExit.PolicyStatuses()
							if len(statuses) != 1 || statuses[0].Rejected != 1 || statuses[0].DetectedProtocol != "shadowsocks" || statuses[0].Evidence != string(detect.Authenticated) || statuses[0].InspectionLocation != "exit" {
								t.Fatalf("missing independently observed exit rejection: %+v", statuses)
							}
						} else {
							before := received.Load()
							protocolUDPExchange(t, flow, wire)
							if received.Load() != before+1 {
								t.Fatal("off/restored exit did not reach origin")
							}
						}
					})
				}
			})
		}
	}
}

func TestProtocolUDPIndependentQUICExitHotPolicy(t *testing.T) {
	profile, wire := protocolUDPReference(t, "2022-blake3-chacha20-poly1305")
	origin, received := protocolUDPOrigin(t)
	plan := protocolUDPPlan(t, profile, origin)
	pair, roots := testCertificate(t)
	server := &DatagramServer{TLS: &tls.Config{Certificates: []tls.Certificate{pair}}, Token: "udp-quic-exit-fixture-token"}
	if err := server.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); server.ServeBound() }()
	t.Cleanup(func() { server.Close(); <-done })
	server.UpdateAuthorization(func(token, target string) bool { return token == server.Token && target == origin }, nil)
	rejected := new(atomic.Uint64)
	for _, phase := range []string{"off", "on", "restore"} {
		t.Run(phase, func(t *testing.T) {
			if phase == "on" {
				server.UpdateInspection(func(token, target string, p []byte) bool {
					if token != server.Token || target != origin {
						return true
					}
					d := plan.Feed(p, true, "udp")
					if plan.Decision(d) != nil {
						rejected.Add(1)
						return true
					}
					return false
				})
			} else {
				server.UpdateInspection(nil)
			}
			// A fresh entry pool after a policy generation avoids retaining a
			// locally cached carrier whose server has just revoked authorization.
			pool := new(DatagramPool)
			defer pool.Close()
			session, err := pool.Dial(context.Background(), &tls.Config{RootCAs: roots}, server.Addr().String(), "localhost", server.Token, origin)
			if err != nil {
				t.Fatal(err)
			}
			flow := protocolDatagramFlow(session)
			defer flow.close()
			if phase == "on" {
				protocolUDPBlock(t, flow, wire, received, rejected)
			} else {
				before := received.Load()
				protocolUDPExchange(t, flow, wire)
				if received.Load() != before+1 {
					t.Fatal("off/restored QUIC exit did not reach origin")
				}
			}
		})
	}
}

// This measures local detector cost, not end-to-end native UDP performance.
func BenchmarkProtocolUDPAuthenticated(b *testing.B) {
	for _, method := range []string{"aes-128-gcm", "2022-blake3-aes-128-gcm", "2022-blake3-chacha20-poly1305"} {
		b.Run(method, func(b *testing.B) {
			profile, wire := protocolUDPReference(b, method)
			plan := protocolUDPPlan(b, profile, "127.0.0.1:443")
			if d := plan.Feed(wire, true, "udp"); d.Status != detect.Match || d.Evidence != detect.Authenticated {
				b.Fatalf("invalid client fixture: %+v", d)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(wire)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if d := plan.Feed(wire, true, "udp"); d.Status != detect.Match {
					b.Fatal(d)
				}
			}
		})
	}
}
