package main

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"

	"trafficpanel/internal/config"
	"trafficpanel/internal/domain"
)

func TestUDPForwarderRelaysDatagrams(t *testing.T) {
	target, err := net.ListenUDP("udp", mustResolveUDPAddr(t, "127.0.0.1:0"))
	if err != nil {
		t.Fatalf("listen target udp: %v", err)
	}
	defer target.Close()

	go func() {
		buf := make([]byte, 64*1024)
		for {
			n, addr, err := target.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = target.WriteToUDP(buf[:n], addr)
		}
	}()

	forwarder := newLocalForwarder(nil, config.Config{AgentUDPIdleTimeout: 100 * time.Millisecond})
	service := domain.ForwardService{
		ServiceKey: "udp-test",
		Protocol:   domain.ProtocolUDP,
		ListenAddr: "127.0.0.1:0",
		TargetAddr: target.LocalAddr().String(),
		MaxConn:    4,
	}
	if err := forwarder.startService(service); err != nil {
		t.Fatalf("start udp service: %v", err)
	}
	defer forwarder.stopService(service.ServiceKey)

	forwarder.mu.Lock()
	listenAddr := forwarder.udpForwarders[service.ServiceKey].listenConn.LocalAddr().String()
	forwarder.mu.Unlock()

	client, err := net.Dial("udp", listenAddr)
	if err != nil {
		t.Fatalf("dial udp forwarder: %v", err)
	}
	defer client.Close()

	payload := []byte("udp-ping")
	if _, err := client.Write(payload); err != nil {
		t.Fatalf("write udp payload: %v", err)
	}
	if err := client.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	buf := make([]byte, 64*1024)
	n, err := client.Read(buf)
	if err != nil {
		t.Fatalf("read udp response: %v", err)
	}
	if !bytes.Equal(buf[:n], payload) {
		t.Fatalf("unexpected udp response: got %q want %q", buf[:n], payload)
	}

	bytesIn, bytesOut, active := forwarder.counters(service.ServiceKey)
	if bytesIn == nil || bytesOut == nil || active == nil {
		t.Fatal("missing udp counters")
	}
	if bytesIn.Load() == 0 || bytesOut.Load() == 0 {
		t.Fatalf("expected udp byte counters to increase, in=%d out=%d", bytesIn.Load(), bytesOut.Load())
	}
	if active.Load() != 1 {
		t.Fatalf("expected one active udp session, got %d", active.Load())
	}
}

func TestTCPForwarderHonorsMaxConn(t *testing.T) {
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen target tcp: %v", err)
	}
	defer target.Close()

	hold := make(chan struct{})
	defer close(hold)
	go func() {
		for {
			conn, err := target.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				<-hold
			}(conn)
		}
	}()

	forwarder := newLocalForwarder(nil, config.Config{})
	service := domain.ForwardService{
		ServiceKey: "tcp-max-test",
		Protocol:   domain.ProtocolTCP,
		ListenAddr: "127.0.0.1:0",
		TargetAddr: target.Addr().String(),
		MaxConn:    1,
	}
	if err := forwarder.startService(service); err != nil {
		t.Fatalf("start tcp service: %v", err)
	}
	defer forwarder.stopService(service.ServiceKey)

	forwarder.mu.Lock()
	listenAddr := forwarder.tcpListeners[service.ServiceKey].Addr().String()
	forwarder.mu.Unlock()

	first, err := net.Dial("tcp", listenAddr)
	if err != nil {
		t.Fatalf("dial first tcp client: %v", err)
	}
	defer first.Close()
	waitForActiveConns(t, forwarder, service.ServiceKey, 1)

	second, err := net.Dial("tcp", listenAddr)
	if err != nil {
		t.Fatalf("dial second tcp client: %v", err)
	}
	defer second.Close()
	if err := second.SetReadDeadline(time.Now().Add(500 * time.Millisecond)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	buf := make([]byte, 1)
	_, err = second.Read(buf)
	if err == nil || err == io.EOF {
		return
	}
	if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		t.Fatal("second tcp connection stayed open despite max_conn=1")
	}
}

func waitForActiveConns(t *testing.T, forwarder *localForwarder, serviceKey string, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, _, active := forwarder.counters(serviceKey)
		if active != nil && active.Load() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, _, active := forwarder.counters(serviceKey)
	if active == nil {
		t.Fatalf("missing active connection counter for %s", serviceKey)
	}
	t.Fatalf("active connections = %d, want %d", active.Load(), want)
}

func mustResolveUDPAddr(t *testing.T, addr string) *net.UDPAddr {
	t.Helper()
	resolved, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		t.Fatalf("resolve udp addr %s: %v", addr, err)
	}
	return resolved
}
