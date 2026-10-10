package association

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"
)

func endpoint(b []byte, command byte, addr netip.AddrPort) []byte {
	b = append(b, 5, command, 0)
	if addr.Addr().Unmap().Is4() {
		a := addr.Addr().Unmap().As4()
		b = append(b, 1)
		b = append(b, a[:]...)
	} else {
		a := addr.Addr().As16()
		b = append(b, 4)
		b = append(b, a[:]...)
	}
	var p [2]byte
	binary.BigEndian.PutUint16(p[:], addr.Port())
	return append(b, p[:]...)
}
func testBinding() Binding {
	return Binding{ControlRuleID: "control", UDPRuleID: "udp", Generation: "epoch", RelayEndpoint: netip.MustParseAddrPort("127.0.0.1:6000"), RelayTarget: "127.0.0.1:7000"}
}
func udpDatagram() []byte { return []byte{0, 0, 0, 1, 127, 0, 0, 1, 0, 53, 'x'} }

// liveObserver routes real TCP sockets in both directions. The server receives
// all original bytes; the registry learns only completed forwarded Writes.
func liveObserver(t *testing.T, r *Registry, binding Binding, method byte, bnd netip.AddrPort, requestedPort uint16) (net.Conn, func()) {
	t.Helper()
	origin, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	entry, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		origin.Close()
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	serverDone := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(serverDone)
		c, e := origin.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(5 * time.Second))
		greeting := make([]byte, 3)
		if _, e = io.ReadFull(c, greeting); e != nil {
			return
		}
		if !bytes.Equal(greeting, []byte{5, 1, method}) {
			t.Error("greeting changed")
		}
		if _, e = c.Write([]byte{5, method}); e != nil {
			return
		}
		if method == 2 {
			var header [2]byte
			if _, e = io.ReadFull(c, header[:]); e != nil {
				return
			}
			body := make([]byte, int(header[1])+1)
			if _, e = io.ReadFull(c, body); e != nil {
				return
			}
			password := make([]byte, int(body[len(body)-1]))
			if _, e = io.ReadFull(c, password); e != nil {
				return
			}
			if _, e = c.Write([]byte{1, 0}); e != nil {
				return
			}
		}
		request := make([]byte, 10)
		if _, e = io.ReadFull(c, request); e != nil {
			return
		}
		if !bytes.Equal(request, endpoint(nil, 3, netip.AddrPortFrom(netip.IPv4Unspecified(), requestedPort))) {
			t.Error("association request changed")
		}
		reply := endpoint(nil, 0, bnd)
		for _, value := range reply {
			if _, e = c.Write([]byte{value}); e != nil {
				return
			}
		}
		c.SetDeadline(time.Time{})
		<-stop
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		client, e := entry.Accept()
		if e != nil {
			return
		}
		target, e := net.Dial("tcp", origin.Addr().String())
		if e != nil {
			client.Close()
			return
		}
		wrappedClient, wrappedTarget, cleanup, e := r.Observe(client, target, binding)
		if e != nil {
			client.Close()
			target.Close()
			return
		}
		defer cleanup()
		defer wrappedClient.Close()
		defer wrappedTarget.Close()
		done := make(chan struct{})
		go func() { io.Copy(wrappedTarget, wrappedClient); close(done) }()
		io.Copy(wrappedClient, wrappedTarget)
		wrappedClient.Close()
		wrappedTarget.Close()
		<-done
	}()
	client, e := net.Dial("tcp", entry.Addr().String())
	if e != nil {
		t.Fatal(e)
	}
	client.SetDeadline(time.Now().Add(5 * time.Second))
	write := func(b []byte) {
		for _, v := range b {
			if _, e := client.Write([]byte{v}); e != nil {
				t.Fatal(e)
			}
		}
	}
	write([]byte{5, 1, method})
	var reply [2]byte
	if _, e = io.ReadFull(client, reply[:]); e != nil {
		t.Fatal(e)
	}
	if method == 2 {
		write([]byte{1, 1, 'u', 1, 'p'})
		if _, e = io.ReadFull(client, reply[:]); e != nil {
			t.Fatal(e)
		}
	}
	write(endpoint(nil, 3, netip.AddrPortFrom(netip.IPv4Unspecified(), requestedPort)))
	buffer := make([]byte, len(endpoint(nil, 0, bnd)))
	if _, e = io.ReadFull(client, buffer); e != nil {
		t.Fatal(e)
	}
	client.SetDeadline(time.Time{})
	cleanup := func() { client.Close(); close(stop); origin.Close(); entry.Close(); wg.Wait() }
	return client, cleanup
}

func awaitProof(r *Registry, b Binding, source netip.AddrPort) Proof {
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		p := r.Admission(b.UDPRuleID, b.Generation, source, b.RelayTarget, udpDatagram())
		if p.ValidFor(b.UDPRuleID, b.Generation, b.RelayTarget) {
			return p
		}
		time.Sleep(time.Millisecond)
	}
	return Proof{}
}

func TestLiveTCPAssociationBothAuthenticationMethods(t *testing.T) {
	for _, method := range []byte{0, 2} {
		t.Run(string(rune('0'+method)), func(t *testing.T) {
			r := New(16, time.Minute)
			b := testBinding()
			if e := r.Configure([]Binding{b}); e != nil {
				t.Fatal(e)
			}
			c, cleanup := liveObserver(t, r, b, method, b.RelayEndpoint, 0)
			defer cleanup()
			source := netip.MustParseAddrPort("127.0.0.1:41000")
			proof := awaitProof(r, b, source)
			if !proof.ValidFor(b.UDPRuleID, b.Generation, b.RelayTarget) {
				t.Fatal("successful actual SOCKS exchange did not register")
			}
			if r.Admission(b.UDPRuleID, b.Generation, netip.MustParseAddrPort("127.0.0.1:41001"), b.RelayTarget, udpDatagram()).ValidFor(b.UDPRuleID, b.Generation, b.RelayTarget) {
				t.Fatal("wildcard association moved to new UDP port")
			}
			if r.Admission(b.UDPRuleID, b.Generation, netip.MustParseAddrPort("127.0.0.2:41000"), b.RelayTarget, udpDatagram()).ValidFor(b.UDPRuleID, b.Generation, b.RelayTarget) {
				t.Fatal("other UDP source IP inherited association")
			}
			if proof.ValidFor("other", b.Generation, b.RelayTarget) || proof.ValidFor(b.UDPRuleID, "changed", b.RelayTarget) || proof.ValidFor(b.UDPRuleID, b.Generation, "other-target") {
				t.Fatal("proof crossed scope")
			}
			c.Close()
			deadline := time.Now().Add(time.Second)
			for r.Count() != 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if proof.ValidFor(b.UDPRuleID, b.Generation, b.RelayTarget) {
				t.Fatal("closed control retained proof")
			}
		})
	}
}

func TestLiveBNDAndRequestedSourcePort(t *testing.T) {
	b := testBinding()
	r := New(16, time.Minute)
	r.Configure([]Binding{b})
	_, cleanup := liveObserver(t, r, b, 0, netip.MustParseAddrPort("127.0.0.1:6001"), 0)
	cleanup()
	if r.Count() != 0 {
		t.Fatal("untrusted advertised BND registered")
	}
	r = New(16, time.Minute)
	r.Configure([]Binding{b})
	_, cleanup = liveObserver(t, r, b, 0, b.RelayEndpoint, 41000)
	defer cleanup()
	if awaitProof(r, b, netip.MustParseAddrPort("127.0.0.1:41000")).registry == nil {
		t.Fatal("requested fixed port missing")
	}
	if r.Admission(b.UDPRuleID, b.Generation, netip.MustParseAddrPort("127.0.0.1:41001"), b.RelayTarget, udpDatagram()).registry != nil {
		t.Fatal("fixed source port ignored")
	}
}

func TestExpiryRevokeConfigureAndAmbiguousWildcard(t *testing.T) {
	b := testBinding()
	r := New(16, time.Minute)
	r.Configure([]Binding{b})
	_, cleanup := liveObserver(t, r, b, 0, b.RelayEndpoint, 0)
	defer cleanup()
	proof := awaitProof(r, b, netip.MustParseAddrPort("127.0.0.1:41000"))
	if proof.registry == nil {
		t.Fatal("proof missing")
	}
	r.Configure([]Binding{b})
	if !proof.ValidFor(b.UDPRuleID, b.Generation, b.RelayTarget) {
		t.Fatal("identical config revoked proof")
	}
	r.RevokeRule(b.ControlRuleID)
	if proof.ValidFor(b.UDPRuleID, b.Generation, b.RelayTarget) {
		t.Fatal("rule revoked but proof survived")
	}
	r = New(16, time.Minute)
	r.Configure([]Binding{b})
	_, cleanup2 := liveObserver(t, r, b, 0, b.RelayEndpoint, 0)
	defer cleanup2()
	proof = awaitProof(r, b, netip.MustParseAddrPort("127.0.0.1:41000"))
	r.mu.Lock()
	r.sessions[proof.sessionID].expires = time.Now().Add(-time.Millisecond)
	r.mu.Unlock()
	if proof.ValidFor(b.UDPRuleID, b.Generation, b.RelayTarget) {
		t.Fatal("expired proof survived")
	}
	r = New(16, time.Minute)
	r.Configure([]Binding{b})
	_, c1 := liveObserver(t, r, b, 0, b.RelayEndpoint, 0)
	defer c1()
	_, c2 := liveObserver(t, r, b, 0, b.RelayEndpoint, 0)
	defer c2()
	time.Sleep(10 * time.Millisecond)
	if r.Admission(b.UDPRuleID, b.Generation, netip.MustParseAddrPort("127.0.0.1:41000"), b.RelayTarget, udpDatagram()).registry != nil {
		t.Fatal("ambiguous zero-port sessions guessed association")
	}
}

type fixtureConn struct {
	net.Conn
	bytes.Buffer
	peer netip.AddrPort
	fail bool
}

func (c *fixtureConn) RemoteAddr() net.Addr       { return net.TCPAddrFromAddrPort(c.peer) }
func (c *fixtureConn) Read(b []byte) (int, error) { return 0, io.EOF }
func (c *fixtureConn) Write(b []byte) (int, error) {
	if c.fail {
		return 0, io.ErrClosedPipe
	}
	return c.Buffer.Write(b)
}
func (c *fixtureConn) Close() error { return nil }

func TestUnwrittenHandshakeMalformedCapacityAndRevoke(t *testing.T) {
	b := testBinding()
	r := New(1, time.Minute)
	r.Configure([]Binding{b})
	c := &fixtureConn{peer: netip.MustParseAddrPort("127.0.0.1:40000")}
	target := &fixtureConn{peer: netip.MustParseAddrPort("127.0.0.1:50000"), fail: true}
	wc, wt, cleanup, e := r.Observe(c, target, b)
	if e != nil {
		t.Fatal(e)
	}
	defer cleanup()
	if _, _, _, e = r.Observe(c, target, b); e == nil {
		t.Fatal("session capacity unbounded")
	}
	if _, e = wt.Write([]byte{5, 1, 0}); e == nil {
		t.Fatal("expected failed write")
	}
	if r.Count() != 0 {
		t.Fatal("failed forwarding retained observer")
	}
	wc.Close()
	r = New(4, time.Minute)
	r.Configure([]Binding{b})
	target.fail = false
	wc, wt, cleanup, e = r.Observe(c, target, b)
	if e != nil {
		t.Fatal(e)
	}
	wt.Write([]byte{5, 0})
	if r.Count() != 0 {
		t.Fatal("invalid greeting retained observer")
	}
	cleanup()
	wc.Close()
	r.Configure(nil)
	if _, _, _, e = r.Observe(c, target, b); e == nil {
		t.Fatal("revoked config allowed stale observer")
	}
	if validUDP([]byte{0, 0, 0, 3, 0, 0, 80}) || validUDP([]byte{5, 1, 0}) || !validUDP([]byte{0, 0, 1, 3, 1, 'a', 0, 80}) {
		t.Fatal("RFC UDP header bounds incorrect")
	}
}

func TestConcurrentAdmissionConfigureRevoke(t *testing.T) {
	b := testBinding()
	r := New(4, time.Minute)
	r.Configure([]Binding{b})
	_, cleanup := liveObserver(t, r, b, 0, b.RelayEndpoint, 0)
	defer cleanup()
	source := netip.MustParseAddrPort("127.0.0.1:41000")
	proof := awaitProof(r, b, source)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				r.Admission(b.UDPRuleID, b.Generation, source, b.RelayTarget, udpDatagram())
				proof.ValidFor(b.UDPRuleID, b.Generation, b.RelayTarget)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			r.Configure([]Binding{b})
		}
		r.RevokeGeneration(b.Generation)
	}()
	wg.Wait()
	if proof.ValidFor(b.UDPRuleID, b.Generation, b.RelayTarget) {
		t.Fatal("concurrent revoke ineffective")
	}
}

func FuzzBoundedControlObservation(f *testing.F) {
	f.Add([]byte{5, 1, 0}, []byte{5, 0})
	f.Add([]byte{5, 0}, []byte{5, 0})
	f.Add([]byte{1, 255}, []byte{1, 0})
	f.Fuzz(func(t *testing.T, client, server []byte) {
		if len(client) > 2048 {
			client = client[:2048]
		}
		if len(server) > 2048 {
			server = server[:2048]
		}
		r := New(4, time.Minute)
		b := testBinding()
		r.Configure([]Binding{b})
		c := &fixtureConn{peer: netip.MustParseAddrPort("127.0.0.1:40000")}
		target := &fixtureConn{peer: netip.MustParseAddrPort("127.0.0.1:50000")}
		wc, wt, cleanup, e := r.Observe(c, target, b)
		if e != nil {
			t.Fatal(e)
		}
		defer cleanup()
		for n := 0; n < max(len(client), len(server)); n++ {
			if n < len(client) {
				wt.Write(client[n : n+1])
			}
			if n < len(server) {
				wc.Write(server[n : n+1])
			}
		}
		validUDP(client)
		r.Admission(b.UDPRuleID, b.Generation, netip.MustParseAddrPort("127.0.0.1:41000"), b.RelayTarget, client)
		wc.Close()
		if r.Count() != 0 {
			t.Fatal("closed fuzzed observer survived")
		}
	})
}
