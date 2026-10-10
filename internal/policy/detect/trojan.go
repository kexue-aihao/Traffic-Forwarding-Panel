package detect

import (
	"crypto/subtle"
)

func detectTrojan(b []byte, end bool, network string, p compiledProfile) Detection {
	if network != "tcp" {
		return Detection{Status: Unavailable, Reason: "trojan_udp_is_inside_tcp"}
	}
	for _, c := range b[:min(len(b), 56)] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return noMatch("not_trojan_hash")
		}
	}
	if len(b) < 58 {
		return need(58, end)
	}
	if b[56] != 13 || b[57] != 10 || subtle.ConstantTimeCompare(b[:56], p.trojanHash) != 1 {
		return noMatch("authentication_failed")
	}
	if len(b) < 60 {
		return need(60, end)
	}
	if b[58] != 1 && b[58] != 3 {
		return noMatch("invalid_trojan_command")
	}
	n, state := addressLength(b, 59)
	if state < 0 {
		return noMatch("invalid_trojan_address")
	}
	if state > 0 {
		return need(state+2, end)
	}
	if len(b) < n+2 {
		return need(n+2, end)
	}
	if b[n] != 13 || b[n+1] != 10 {
		return noMatch("invalid_trojan_crlf")
	}
	variant := "trojan-tcp"
	if b[58] == 3 {
		variant = "trojan-udp-over-tcp"
	}
	return Detection{Protocol: "trojan", Variant: variant, Status: Match, Evidence: Authenticated, Reason: "known_authentication_hash"}
}
