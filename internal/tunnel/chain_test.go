package tunnel

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

type chainFixture struct {
	hops     []contract.TunnelHop
	servers  []*Server
	client   Client
	tcp, udp string
}

func setupChain(t *testing.T, transports []string, pair tls.Certificate, roots *x509.CertPool, configure ...func(*chainFixture)) *chainFixture {
	t.Helper()
	tcp, udp := echoServers(t)
	f := &chainFixture{client: Client{TLS: &tls.Config{RootCAs: roots}, Timeout: time.Second}, tcp: tcp, udp: udp}
	listeners := []net.Listener{}
	for i, transport := range transports {
		l, e := net.Listen("tcp", "127.0.0.1:0")
		if e != nil {
			t.Fatal(e)
		}
		listeners = append(listeners, l)
		endpoint := l.Addr().String()
		if transport == "ws" || transport == "wss" {
			endpoint = transport + "://" + endpoint + "/tunnel"
		}
		f.hops = append(f.hops, contract.TunnelHop{Transport: transport, Endpoint: endpoint, ServerName: "localhost", Token: "test-chain-token-" + strings.Repeat("x", i+1)})
	}
	for i, h := range f.hops {
		s := &Server{TLS: &tls.Config{Certificates: []tls.Certificate{pair}}, Token: h.Token, Allowed: map[string]bool{"tcp|" + tcp: true, "udp|" + udp: true}, NodeID: h.Endpoint, Client: f.client}
		if i+1 < len(f.hops) {
			s.NextHops = []contract.TunnelHop{f.hops[i+1]}
		}
		f.servers = append(f.servers, s)
	}
	for _, configure := range configure {
		configure(f)
	}
	for i, s := range f.servers {
		done := make(chan struct{})
		go func(l net.Listener, transport string) { defer close(done); s.Serve(l, transport) }(listeners[i], f.hops[i].Transport)
		t.Cleanup(func() { s.Close(); <-done })
	}
	return f
}
func (f *chainFixture) dial(ctx context.Context, network, target string) (*Session, error) {
	h := f.hops[0]
	return f.client.DialChain(ctx, h.Transport, h.Endpoint, h.ServerName, h.Token, network, target, f.hops[1:])
}
func TestThreeHopAllCarrierCombinationsTCPUDP(t *testing.T) {
	pair, roots := testCertificate(t)
	carriers := []string{"tls", "ws", "wss", "http"}
	for _, a := range carriers {
		for _, b := range carriers {
			for _, c := range carriers {
				t.Run(a+"_"+b+"_"+c, func(t *testing.T) {
					f := setupChain(t, []string{a, b, c}, pair, roots)
					conn, e := f.dial(context.Background(), "tcp", f.tcp)
					if e != nil {
						t.Fatal(e)
					}
					conn.SetDeadline(time.Now().Add(3 * time.Second))
					p := bytes.Repeat([]byte("chain-payload"), 6000)
					done := make(chan error, 1)
					go func() {
						_, e := conn.Write(p)
						if e == nil {
							e = conn.CloseWrite()
						}
						done <- e
					}()
					got, e := io.ReadAll(conn)
					conn.Close()
					if e != nil || !bytes.Equal(got, p) {
						t.Fatalf("TCP chain data %d %v", len(got), e)
					}
					if e = <-done; e != nil {
						t.Fatal(e)
					}
					udp, e := f.dial(context.Background(), "udp", f.udp)
					if e != nil {
						t.Fatal(e)
					}
					defer udp.Close()
					udp.SetDeadline(time.Now().Add(3 * time.Second))
					for _, p := range [][]byte{[]byte("first"), {}, bytes.Repeat([]byte{7}, 60000), []byte("last")} {
						if e = udp.WritePacket(p); e != nil {
							t.Fatal(e)
						}
						got, e := udp.ReadPacket()
						if e != nil || !bytes.Equal(got, p) {
							t.Fatalf("UDP chain boundary %d %v", len(got), e)
						}
					}
				})
			}
		}
	}
}
func TestChainRejectsCyclesLengthsAndUnauthorizedHops(t *testing.T) {
	pair, roots := testCertificate(t)
	t.Run("bounds", func(t *testing.T) {
		f := setupChain(t, []string{"tls", "ws", "http"}, pair, roots)
		first := f.hops[0]
		if e := ValidateChain(first, append(f.hops[1:], first)); e == nil {
			t.Fatal("four exits accepted")
		}
		if e := ValidateChain(first, []contract.TunnelHop{first}); e == nil {
			t.Fatal("cycle accepted")
		}
	})
	t.Run("operator allowlist", func(t *testing.T) {
		f := setupChain(t, []string{"tls", "wss"}, pair, roots)
		h := f.hops[0]
		bad := f.hops[1]
		bad.Token = "wrong-valid-length-token"
		if c, e := f.client.DialChain(context.Background(), h.Transport, h.Endpoint, h.ServerName, h.Token, "tcp", f.tcp, []contract.TunnelHop{bad}); e == nil {
			c.Close()
			t.Fatal("unauthorized next-hop credential accepted")
		}
	})
	t.Run("final target checked on every hop", func(t *testing.T) {
		f := setupChain(t, []string{"tls", "http"}, pair, roots)
		h := f.hops[0]
		if c, e := f.client.DialChain(context.Background(), h.Transport, h.Endpoint, h.ServerName, h.Token, "tcp", "127.0.0.1:1", f.hops[1:]); e == nil {
			c.Close()
			t.Fatal("nonallowlisted final target accepted")
		}
	})
	t.Run("visited identity rejects alias loop", func(t *testing.T) {
		f := setupChain(t, []string{"tls", "ws"}, pair, roots)
		h := f.hops[0]
		if c, e := f.client.dial(context.Background(), h.Transport, h.Endpoint, h.ServerName, h.Token, "tcp", f.tcp, f.hops[1:], []string{f.servers[0].NodeID}); e == nil {
			c.Close()
			t.Fatal("visited server accepted")
		}
	})
	t.Run("middle disconnect", func(t *testing.T) {
		f := setupChain(t, []string{"tls", "wss", "http"}, pair, roots)
		c, e := f.dial(context.Background(), "tcp", f.tcp)
		if e != nil {
			t.Fatal(e)
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(time.Second))
		c.Write([]byte("first"))
		buf := make([]byte, 5)
		if _, e = io.ReadFull(c, buf); e != nil {
			t.Fatal(e)
		}
		f.servers[1].Close()
		if _, e = c.Read(buf); e == nil {
			t.Fatal("middle failure did not propagate")
		}
	})
}

func TestChainChecksEveryExit(t *testing.T) {
	pair, roots := testCertificate(t)
	for hop := 0; hop < 3; hop++ {
		for _, rejection := range []string{"certificate name", "certificate trust", "token", "target"} {
			for _, transport := range []string{"tls", "ws", "wss", "http"} {
				t.Run(fmt.Sprintf("hop%d/%s/%s", hop, rejection, transport), func(t *testing.T) {
					transports := []string{"tls", "tls", "tls"}
					transports[hop] = transport
					f := setupChain(t, transports, pair, roots, func(f *chainFixture) {
						switch rejection {
						case "certificate name":
							f.hops[hop].ServerName = "wrong.example"
						case "certificate trust":
							untrusted := Client{TLS: &tls.Config{RootCAs: x509.NewCertPool()}, Timeout: time.Second}
							if hop == 0 {
								f.client = untrusted
							} else {
								f.servers[hop-1].Client = untrusted
							}
						case "token":
							f.hops[hop].Token = "wrong-token-with-enough-characters"
						case "target":
							f.servers[hop].Allowed = map[string]bool{"tcp|127.0.0.1:1": true}
						}
						if hop > 0 {
							// The local policy accepts the supplied identity; the receiving
							// exit or its certificate must independently reject it.
							f.servers[hop-1].NextHops = []contract.TunnelHop{f.hops[hop]}
						}
					})
					if c, err := f.dial(context.Background(), "tcp", f.tcp); err == nil {
						c.Close()
						t.Fatal("invalid route accepted")
					}
					waitChainEmpty(t, f)
				})
			}
		}
	}
}

func waitChainEmpty(t *testing.T, f *chainFixture) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		active := 0
		for _, s := range f.servers {
			s.mu.Lock()
			active += len(s.conns)
			s.mu.Unlock()
		}
		if active == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d exit connections were not reclaimed", active)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestChainRejectsActualAliasLoop(t *testing.T) {
	pair, roots := testCertificate(t)
	var alias contract.TunnelHop
	f := setupChain(t, []string{"tls", "tls"}, pair, roots, func(f *chainFixture) {
		alias = f.hops[0]
		_, port, _ := net.SplitHostPort(alias.Endpoint)
		alias.Endpoint = net.JoinHostPort("localhost", port)
		f.servers[1].NextHops = []contract.TunnelHop{alias}
	})
	h := f.hops[0]
	chain := []contract.TunnelHop{f.hops[1], alias}
	if err := ValidateChain(h, chain); err != nil {
		t.Fatal("test must reach server identity verification", err)
	}
	if c, err := f.client.DialChain(context.Background(), h.Transport, h.Endpoint, h.ServerName, h.Token, "tcp", f.tcp, chain); err == nil {
		c.Close()
		t.Fatal("DNS alias cycle accepted")
	}
	waitChainEmpty(t, f)
}

func TestChainRequestBoundsCannotBypassClient(t *testing.T) {
	pair, roots := testCertificate(t)
	f := setupChain(t, []string{"tls", "tls", "tls"}, pair, roots)
	h := f.hops[0]
	for _, change := range []func(*openRequest){
		func(r *openRequest) { r.Version = 1 },
		func(r *openRequest) { r.Chain = append(r.Chain, h) },
		func(r *openRequest) { r.Visited = []string{"previous", "previous"}; r.Chain = nil },
		func(r *openRequest) { r.Visited = []string{""}; r.Chain = nil },
	} {
		conn, err := tls.Dial("tcp", h.Endpoint, &tls.Config{RootCAs: roots, ServerName: h.ServerName, MinVersion: tls.VersionTLS13})
		if err != nil {
			t.Fatal(err)
		}
		conn.SetDeadline(time.Now().Add(time.Second))
		req := openRequest{Version: 2, Token: h.Token, Network: "tcp", Target: f.tcp, Chain: f.hops[1:]}
		change(&req)
		p, _ := json.Marshal(req)
		if err = writeFrame(conn, openFrame, p); err != nil {
			t.Fatal(err)
		}
		if _, _, err = readFrame(conn); err == nil {
			t.Fatal("invalid wire route accepted")
		}
		conn.Close()
	}
}

func TestValidateChainCanonicalEndpoints(t *testing.T) {
	for _, endpoints := range [][2]string{{"EXIT.example.:09443", "exit.example:9443"}, {"[::ffff:127.0.0.1]:9443", "127.0.0.1:9443"}, {"[0:0:0:0:0:0:0:1]:9443", "[::1]:9443"}} {
		a := contract.TunnelHop{Transport: "tls", Endpoint: endpoints[0], Token: "sixteen-character-token"}
		b := a
		b.Endpoint = endpoints[1]
		if err := ValidateChain(a, []contract.TunnelHop{b}); err == nil {
			t.Errorf("duplicate endpoints accepted: %v", endpoints)
		}
	}
}

func TestChainCloseReclaimsTCPAndUDPSessions(t *testing.T) {
	pair, roots := testCertificate(t)
	for _, network := range []string{"tcp", "udp"} {
		t.Run(network, func(t *testing.T) {
			f := setupChain(t, []string{"ws", "wss", "http"}, pair, roots)
			target := f.tcp
			if network == "udp" {
				target = f.udp
			}
			c, err := f.dial(context.Background(), network, target)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(2 * time.Second))
			if network == "udp" {
				err = c.WritePacket([]byte("probe"))
			} else {
				_, err = c.Write([]byte("probe"))
			}
			if err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, 5)
			if network == "udp" {
				_, err = c.ReadPacket()
			} else {
				_, err = io.ReadFull(c, buf)
			}
			if err != nil {
				t.Fatal(err)
			}
			f.servers[1].Close()
			if network == "udp" {
				_, err = c.ReadPacket()
			} else {
				_, err = c.Read(buf)
			}
			if err == nil {
				t.Fatal("middle exit close did not propagate")
			}
			if e, ok := err.(net.Error); ok && e.Timeout() {
				t.Fatal("client timed out instead of seeing exit close")
			}
			waitChainEmpty(t, f)
		})
	}
}

func TestChainTargetFailureReclaimsAllHops(t *testing.T) {
	pair, roots := testCertificate(t)
	unreachable, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	target := unreachable.Addr().String()
	unreachable.Close()
	f := setupChain(t, []string{"tls", "ws", "wss"}, pair, roots, func(f *chainFixture) {
		for _, server := range f.servers {
			server.Allowed = map[string]bool{"tcp|" + target: true}
		}
	})
	if c, err := f.dial(context.Background(), "tcp", target); err == nil {
		c.Close()
		t.Fatal("unreachable target reported ready")
	}
	waitChainEmpty(t, f)
}

func TestChainCloseCancelsPendingDownstreamHandshake(t *testing.T) {
	pair, roots := testCertificate(t)
	l, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS13})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	opened, closed := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(closed)
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(5 * time.Second))
		if _, _, err = readFrame(c); err != nil {
			return
		}
		close(opened)
		io.Copy(io.Discard, c) // Never send ready; shutdown must cancel this wait.
	}()
	next := contract.TunnelHop{Transport: "tls", Endpoint: l.Addr().String(), ServerName: "localhost", Token: "test-downstream-token"}
	f := setupChain(t, []string{"tls"}, pair, roots, func(f *chainFixture) {
		f.servers[0].NextHops = []contract.TunnelHop{next}
		f.servers[0].Client.Timeout = 10 * time.Second
		f.client.Timeout = 10 * time.Second
	})
	h := f.hops[0]
	done := make(chan error, 1)
	go func() {
		c, err := f.client.DialChain(context.Background(), h.Transport, h.Endpoint, h.ServerName, h.Token, "tcp", f.tcp, []contract.TunnelHop{next})
		if c != nil {
			c.Close()
		}
		done <- err
	}()
	select {
	case <-opened:
	case <-time.After(2 * time.Second):
		t.Fatal("downstream handshake did not open")
	}
	f.servers[0].Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("unfinished handshake accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("client handshake was not canceled")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("pending downstream connection was not reclaimed")
	}
	waitChainEmpty(t, f)
}
