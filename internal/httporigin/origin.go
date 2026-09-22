// Package httporigin determines the browser-facing scheme behind a proxy.
package httporigin

import (
	"net"
	"net/http"
	"net/netip"
)

// ClientIP accepts only one validated address, overwritten by the trusted proxy.
// X-Forwarded-For lists and vendor-specific headers are not trusted here.
func ClientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		values := r.Header.Values("X-Real-IP")
		if len(values) == 1 {
			if address, err := netip.ParseAddr(values[0]); err == nil {
				return address.Unmap().String()
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Scheme accepts a single X-Forwarded-Proto only when the operator explicitly
// enables proxy mode. The proxy must overwrite this header and preserve Host;
// forwarded host headers are never used to authorize a browser origin.
func Scheme(r *http.Request, trustProxy bool) string {
	if r.TLS != nil {
		return "https"
	}
	if trustProxy {
		values := r.Header.Values("X-Forwarded-Proto")
		if len(values) == 1 && values[0] == "https" {
			return "https"
		}
	}
	return "http"
}
