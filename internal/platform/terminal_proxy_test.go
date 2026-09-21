package platform

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

func TestTerminalProxyOrigin(t *testing.T) {
	s := New(nil, Options{TrustProxy: true})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.SetPathValue("id", "proxy-origin-test")
		s.attachTerminal(w, r, false, "user", "session")
	})
	server := httptest.NewServer(handler)
	defer server.Close()
	url := "ws" + strings.TrimPrefix(server.URL, "http")
	headers := http.Header{"X-Forwarded-Proto": {"https"}, "X-Forwarded-Host": {"attacker.example"}}
	for _, origin := range []string{"https://attacker.example", "http" + strings.TrimPrefix(server.URL, "http")} {
		headers.Set("Origin", origin)
		conn, response, err := websocket.DefaultDialer.Dial(url, headers)
		if conn != nil {
			conn.Close()
			s.closeTerminal("proxy-origin-test")
		}
		if response != nil {
			response.Body.Close()
		}
		if err == nil || response == nil || response.StatusCode != http.StatusForbidden {
			t.Fatalf("invalid WebSocket origin %s accepted: %v", origin, err)
		}
	}
	headers.Set("Origin", "https"+strings.TrimPrefix(server.URL, "http"))
	conn, _, err := websocket.DefaultDialer.Dial(url, headers)
	if err != nil {
		t.Fatal("proxy WebSocket origin rejected:", err)
	}
	conn.Close()
	s.closeTerminal("proxy-origin-test")
}
