package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDirectionalLoadVerifiesActualUDPAndReport(t *testing.T) {
	server, e := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if e != nil {
		t.Fatal(e)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 65535)
		for {
			n, peer, e := server.ReadFromUDP(buf)
			if e != nil {
				return
			}
			if reply := referenceReply(buf[:n]); reply != nil {
				server.WriteToUDP(reply, peer)
			}
		}
	}()
	defer func() { server.Close(); <-done }()
	for _, direction := range []string{"echo", "upload", "download"} {
		t.Run(direction, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "report.json")
			o := options{target: server.LocalAddr().String(), size: 1200, sessions: 4, pps: 100, warmup: 50 * time.Millisecond, duration: 200 * time.Millisecond, drain: time.Second, output: path, direction: direction}
			if e := load(context.Background(), o); e != nil {
				t.Fatal(e)
			}
			b, e := os.ReadFile(path)
			if e != nil {
				t.Fatal(e)
			}
			var result report
			if e := json.Unmarshal(b, &result); e != nil {
				t.Fatal(e)
			}
			if result.Sent == 0 || result.Sent != result.Received || result.Corrupt != 0 || result.Direction != direction {
				t.Fatal("invalid verified load report", result)
			}
			if direction == "upload" && result.DownloadMbps != 0 || direction == "download" && result.UploadMbps != 0 {
				t.Fatal("control packets counted as bulk throughput", result)
			}
		})
	}
}

func TestReferenceUploadRejectsCorruptPayloadBeforeACK(t *testing.T) {
	p := make([]byte, 1200)
	binary.BigEndian.PutUint32(p[8:12], descriptor(0, 1200, "upload"))
	binary.BigEndian.PutUint32(p[28:32], payloadCRC(p))
	p[100] ^= 1
	if referenceReply(p) != nil {
		t.Fatal("corrupt uploaded payload acknowledged")
	}
}
