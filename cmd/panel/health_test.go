package main

import (
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestHealthcheckDirectTLSVerifiesConfiguredCertificate(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer server.Close()
	cert := filepath.Join(t.TempDir(), "certificate.pem")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := healthcheckTLS(server.Listener.Addr().String(), cert); err != nil {
		t.Fatal(err)
	}
	if err := healthcheckTLS(server.Listener.Addr().String(), cert+"-missing"); err == nil {
		t.Fatal("TLS healthcheck ignored missing trust certificate")
	}
}

func TestHealthcheckRunningPanel(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable, http.StatusFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/health" {
					t.Errorf("unexpected path %s", r.URL.Path)
				}
				w.Header().Set("Location", "/")
				w.WriteHeader(status)
			}))
			defer server.Close()
			_, port, err := net.SplitHostPort(server.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			for _, addr := range []string{server.Listener.Addr().String(), ":" + port, "0.0.0.0:" + port} {
				if err := healthcheck(addr); (err == nil) != (status == http.StatusOK) {
					t.Fatalf("status %d, address %s: %v", status, addr, err)
				}
			}
		})
	}
}
