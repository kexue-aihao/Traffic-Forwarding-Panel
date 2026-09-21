package httporigin

import (
	"crypto/tls"
	"net/http/httptest"
	"testing"
)

func TestProxyScheme(t *testing.T) {
	for _, tc := range []struct {
		name    string
		trust   bool
		tls     bool
		headers []string
		want    string
	}{
		{"direct", false, false, nil, "http"},
		{"untrusted", false, false, []string{"https"}, "http"},
		{"trusted HTTPS", true, false, []string{"https"}, "https"},
		{"trusted HTTP", true, false, []string{"http"}, "http"},
		{"missing", true, false, nil, "http"},
		{"multiple values", true, false, []string{"https", "http"}, "http"},
		{"comma separated", true, false, []string{"https, http"}, "http"},
		{"invalid", true, false, []string{"ftp"}, "http"},
		{"direct TLS", true, true, []string{"http"}, "https"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "http://panel.example", nil)
			if tc.tls {
				r.TLS = &tls.ConnectionState{}
			}
			for _, value := range tc.headers {
				r.Header.Add("X-Forwarded-Proto", value)
			}
			if got := Scheme(r, tc.trust); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}
