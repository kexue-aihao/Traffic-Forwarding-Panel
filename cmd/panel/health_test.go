package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
