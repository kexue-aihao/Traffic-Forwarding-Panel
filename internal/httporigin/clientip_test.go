package httporigin

import (
	"net/http/httptest"
	"testing"
)

func TestClientIPRequiresTrustedSingleAddress(t *testing.T) {
	for _, tc := range []struct {
		trust  bool
		header []string
		want   string
	}{
		{false, []string{"203.0.113.4"}, "127.0.0.1"},
		{true, []string{"203.0.113.4"}, "203.0.113.4"},
		{true, []string{"2001:db8::4"}, "2001:db8::4"},
		{true, []string{"203.0.113.4, 203.0.113.5"}, "127.0.0.1"},
		{true, []string{"203.0.113.4", "203.0.113.5"}, "127.0.0.1"},
		{true, []string{"not-an-ip"}, "127.0.0.1"},
	} {
		r := httptest.NewRequest("GET", "http://panel/", nil)
		r.RemoteAddr = "127.0.0.1:54321"
		for _, value := range tc.header {
			r.Header.Add("X-Real-IP", value)
		}
		if got := ClientIP(r, tc.trust); got != tc.want {
			t.Fatalf("client IP got %s, want %s", got, tc.want)
		}
	}
}
