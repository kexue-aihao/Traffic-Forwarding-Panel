package platform

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/httporigin"
)

type RateLimit struct {
	Period time.Duration
	Limit  int
}

// RequestLimits counts HTTP requests, not individual frames on SSE/WebSockets.
// Node traffic, signed payment callbacks and health checks retain their existing
// controls and do not share the browser's per-IP allowance.
func (s *Server) RequestLimits(next http.Handler) http.Handler {
	if s.opts.UserRateLimit == nil && s.opts.DefaultRateLimit == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if (!strings.HasPrefix(p, "/api/v1/") && !strings.HasPrefix(p, "/online/")) || p == "/api/v1/health" || strings.HasPrefix(p, "/api/v1/agent/") || (strings.HasPrefix(p, "/api/v1/payments/") && strings.HasSuffix(p, "/notify")) {
			next.ServeHTTP(w, r)
			return
		}
		setting := s.opts.DefaultRateLimit
		key := "request-ip:" + httporigin.ClientIP(r, s.opts.TrustProxy)
		if user, err := s.Authenticate(r); err == nil {
			setting = s.opts.UserRateLimit
			key = "request-user:" + user.ID
		}
		if setting != nil && !s.allowWindow(key, setting.Limit, setting.Period) {
			w.Header().Set("Retry-After", strconv.Itoa(max(1, int(setting.Period.Seconds()))))
			fail(w, http.StatusTooManyRequests, "请求过于频繁，请稍后重试")
			return
		}
		next.ServeHTTP(w, r)
	})
}
