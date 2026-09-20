package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIPObservationAndUnavailableMetrics(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("203.0.113.9\n")) }))
	defer srv.Close()
	c := Collector{HTTP: srv.Client(), DiskPath: "/does-not-exist-for-probe-test"}
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
