package main

import (
	"bytes"
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

func mustResolveUDPAddr(t *testing.T, addr string) *net.UDPAddr {
	t.Helper()
	resolved, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		t.Fatalf("resolve udp addr %s: %v", addr, err)
	}
	return resolved
}
