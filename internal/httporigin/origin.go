// Package httporigin determines the browser-facing scheme behind a proxy.
package httporigin

import "net/http"

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
