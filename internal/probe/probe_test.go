package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestIPObservationAndUnavailableMetrics(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("203.0.113.9\n")) }))
	defer srv.Close()
	// Keep this metrics-only test independent of the built-in network probe.
	c := Collector{HTTP: srv.Client(), DiskPath: "/does-not-exist-for-probe-test", ipSample: time.Now().UTC()}
	ip, e := c.Observe(context.Background(), srv.URL)
	if e != nil || ip.Address != "203.0.113.9" || ip.Family != "ipv4" || ip.Source != srv.URL {
		t.Fatalf("observation: %+v %v", ip, e)
	}
	if _, e = c.Observe(context.Background(), "http://example.com"); e == nil {
		t.Fatal("insecure echo accepted")
	}
	p := c.Sample(context.Background(), "node")
	if p.DiskTotal != nil || p.UploadBPS != nil || p.DownloadBPS != nil || p.CPUPercent != nil {
		t.Fatal("unavailable/first sample must be null")
	}
	if p.NodeID != "node" || p.SampledAt.IsZero() {
		t.Fatal("sample identity missing")
	}
}

func TestDefaultPublicIPProbe(t *testing.T) {
	old := runPublicIPCommand
	t.Cleanup(func() { runPublicIPCommand = old })
	runPublicIPCommand = func(_ context.Context, family string) ([]byte, error) {
		switch family {
		case "4":
			return []byte("198.51.100.8\n"), nil
		case "6":
			return []byte("2001:db8::8\n"), nil
		default:
			t.Fatalf("unexpected address family %q", family)
			return nil, context.Canceled
		}
	}
	c := Collector{DiskPath: "/does-not-exist-for-probe-test"}
	p := c.Sample(context.Background(), "node")
	if len(p.PublicIPs) != 2 {
		t.Fatalf("public IP observations: %+v", p.PublicIPs)
	}
	if p.PublicIPs[0].Family != "ipv4" || p.PublicIPs[0].Address != "198.51.100.8" || p.PublicIPs[1].Family != "ipv6" || p.PublicIPs[1].Address != "2001:db8::8" {
		t.Fatalf("unexpected public IP observations: %+v", p.PublicIPs)
	}
	if p.PublicIPs[0].Source != publicIP4EchoURL || p.PublicIPs[1].Source != publicIP6EchoURL || p.PublicIPs[0].ObservedAt.IsZero() {
		t.Fatalf("missing source/time: %+v", p.PublicIPs[0])
	}
}

func TestDefaultPublicIPProbeIPv6FailureIsIgnored(t *testing.T) {
	old := runPublicIPCommand
	t.Cleanup(func() { runPublicIPCommand = old })
	runPublicIPCommand = func(_ context.Context, family string) ([]byte, error) {
		if family == "6" {
			return nil, context.DeadlineExceeded
		}
		return []byte("198.51.100.9"), nil
	}
	c := Collector{DiskPath: "/does-not-exist-for-probe-test"}
	p := c.Sample(context.Background(), "node")
	if len(p.PublicIPs) != 1 || p.PublicIPs[0].Family != "ipv4" {
		t.Fatalf("IPv4 should survive IPv6 failure: %+v", p.PublicIPs)
	}
}

func TestDefaultPublicIPProbeDropsInvalidOutput(t *testing.T) {
	old := runPublicIPCommand
	t.Cleanup(func() { runPublicIPCommand = old })
	runPublicIPCommand = func(_ context.Context, family string) ([]byte, error) {
		if family == "4" {
			return []byte("192.168.1.10"), nil
		}
		return []byte("not-an-ip"), nil
	}
	c := Collector{DiskPath: "/does-not-exist-for-probe-test"}
	p := c.Sample(context.Background(), "node")
	if len(p.PublicIPs) != 0 {
		t.Fatalf("invalid/private command output accepted: %+v", p.PublicIPs)
	}
}

func TestIPObservationRejectsNonPublicResponse(t *testing.T) {
	for _, raw := range []string{"10.0.0.1", "127.0.0.1", "fe80::1", "ff02::1", "0.0.0.0", "not-an-ip"} {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(raw)) }))
		c := Collector{HTTP: srv.Client()}
		if _, err := c.Observe(context.Background(), srv.URL); err == nil {
			t.Errorf("non-public response accepted: %q", raw)
		}
		srv.Close()
	}
}

func TestIPObservationPreservesCustomSource(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("198.51.100.10\n")) }))
	defer srv.Close()
	c := Collector{HTTP: srv.Client(), EchoURLs: []string{srv.URL}}
	p := c.Sample(context.Background(), "node")
	if len(p.PublicIPs) != 1 || p.PublicIPs[0].Source != srv.URL {
		t.Fatalf("custom source not preserved: %+v", p.PublicIPs)
	}
	if p.PublicIPs[0].ObservedAt.IsZero() {
		t.Fatalf("unexpected observation time: %+v", p.PublicIPs[0])
	}
}

func TestDefaultEchoURLCarriesTheAddressFamily(t *testing.T) {
	// api.ipify.org 没有 AAAA 记录。这一条守着「IPv6 问 IPv4-only 的域名」这类
	// 缺陷：它不会让编译失败，只会让 IPv6 那一路永远停在 DNS 解析失败上。
	if got := defaultEchoURL("4"); got != "https://api.ipify.org" {
		t.Fatalf("IPv4 回显地址: %q", got)
	}
	if got := defaultEchoURL("6"); got != "https://api6.ipify.org" {
		t.Fatalf("IPv6 回显地址: %q", got)
	}
	if defaultEchoURL("6") == defaultEchoURL("4") {
		t.Fatal("两个地址族不能问同一个域名：api.ipify.org 解析不出 AAAA")
	}
}
