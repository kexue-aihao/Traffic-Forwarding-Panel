package tunnel

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

func datagramFixture(t testing.TB) (*DatagramServer, *DatagramPool, *tls.Config, string) {
	t.Helper()
	pair, roots := testCertificate(t)
	_, target := echoServers(t)
	s := &DatagramServer{TLS: &tls.Config{Certificates: []tls.Certificate{pair}}, Token: "datagram-test-token-0123456"}
	done := make(chan error, 1)
	go func() { done <- s.Serve("127.0.0.1:0") }()
	deadline := time.Now().Add(3 * time.Second)
	for s.Addr() == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.Addr() == nil {
		t.Fatal("datagram server did not start")
	}
	p := &DatagramPool{}
	t.Cleanup(func() { p.Close(); s.Close(); <-done })
	return s, p, &tls.Config{RootCAs: roots}, target
}

func TestDatagramRealUDPBoundariesAuthenticationAndPooling(t *testing.T) {
	s, p, tc, target := datagramFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, e := p.Dial(ctx, tc, s.Addr().String(), "localhost", s.Token, target)
	if e != nil {
		t.Fatal(e)
	}
	defer first.Close()
	second, e := p.Dial(ctx, tc, s.Addr().String(), "localhost", s.Token, target)
	if e != nil {
		t.Fatal(e)
	}
	defer second.Close()
	if first.link != second.link || first.flow == second.flow {
		t.Fatal("business sessions did not multiplex distinct IDs on one connection")
	}
	for _, size := range []int{0, 1, 64, 1200, 8192, 60000} {
		before := first.link.sentFrames.Load()
		payload := bytes.Repeat([]byte{byte(size + 7)}, size)
		if _, e = first.Write(payload); e != nil {
			t.Fatal(e)
		}
		first.SetReadDeadline(time.Now().Add(3 * time.Second))
		buf := make([]byte, 65535)
		n, e := first.Read(buf)
		if e != nil || !bytes.Equal(buf[:n], payload) {
			t.Fatalf("size %d: n=%d error=%v", size, n, e)
		}
		if size == 1200 && first.link.sentFrames.Load()-before != 1 {
			t.Fatal("1200-byte business datagram unnecessarily fragmented")
		}
	}
	if _, e = p.Dial(ctx, tc, s.Addr().String(), "localhost", "incorrect-token-0123456", target); e == nil {
		t.Fatal("invalid token accepted")
	}
	if _, e = p.Dial(ctx, tc, s.Addr().String(), "wrong.example", s.Token, target); e == nil {
		t.Fatal("certificate name mismatch accepted")
	}
	first.link.admission.Lock()
	first.link.queued.Store(maxDatagramMemory)
	first.link.admission.Unlock()
	charged := false
	if _, e = first.WriteAuthorized([]byte("a"), func() error { charged = true; return nil }); !errors.Is(e, ErrDatagramQueueFull) || charged {
		t.Fatal("queue rejection charged business bytes")
	}
	first.link.queued.Store(0)
	cancel()
	first.SetReadDeadline(time.Now().Add(time.Second))
	if _, e = first.Read(make([]byte, 1)); !errors.Is(e, net.ErrClosed) {
		t.Fatal("cancel did not close session", e)
	}
}

func TestDatagramBindFailureIsSynchronous(t *testing.T) {
	pair, _ := testCertificate(t)
	occupied, e := net.ListenPacket("udp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer occupied.Close()
	s := &DatagramServer{TLS: &tls.Config{Certificates: []tls.Certificate{pair}}, Token: "datagram-bind-token-012345"}
	defer s.Close()
	if e := s.Listen(occupied.LocalAddr().String()); e == nil || s.Addr() != nil {
		t.Fatal("occupied UDP port advertised as bound")
	}
}

func TestDatagramMissingFragmentDoesNotBlockLaterPacket(t *testing.T) {
	s, p, tc, target := datagramFixture(t)
	ctx := context.Background()
	flow, e := p.Dial(ctx, tc, s.Addr().String(), "localhost", s.Token, target)
	if e != nil {
		t.Fatal(e)
	}
	defer flow.Close()
	partial := make([]byte, datagramHeader+10)
	partial[0] = 1
	binary.BigEndian.PutUint64(partial[1:9], flow.flow)
	binary.BigEndian.PutUint64(partial[9:17], 100)
	binary.BigEndian.PutUint32(partial[17:21], 20)
	binary.BigEndian.PutUint16(partial[23:25], 2)
	if e = flow.link.conn.SendDatagram(partial); e != nil {
		t.Fatal(e)
	}
	if _, e = flow.Write([]byte("later")); e != nil {
		t.Fatal(e)
	}
	flow.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 32)
	n, e := flow.Read(buf)
	if e != nil || string(buf[:n]) != "later" {
		t.Fatal("missing fragment blocked unrelated business packet", n, e)
	}
}

func TestDatagramParallelSessions(t *testing.T) {
	s, p, tc, target := datagramFixture(t)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Go(func() {
			flow, e := p.Dial(context.Background(), tc, s.Addr().String(), "localhost", s.Token, target)
			if e != nil {
				t.Error(e)
				return
			}
			defer flow.Close()
			payload := []byte("parallel")
			flow.Write(payload)
			flow.SetReadDeadline(time.Now().Add(3 * time.Second))
			buf := make([]byte, 64)
			n, e := flow.Read(buf)
			if e != nil || !bytes.Equal(buf[:n], payload) {
				t.Error(n, e)
			}
		})
	}
	wg.Wait()
}
