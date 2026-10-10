package detect

import (
	"bytes"
	"strings"
)

// Structural validates complete visible SOCKS/HTTP structures. UDP results are
// explicitly structural evidence, never authentication; strict versioned Plans
// require an explicit UDP structural profile or a trusted association.
func Structural(b []byte, end bool, network string) Detection {
	var next Detection
	for _, app := range []string{"socks4", "socks5", "http"} {
		d := structural(b, end, network, app, true)
		if d.Status == Match {
			return d
		}
		if d.Status == NeedMore && (next.Status != NeedMore || d.Need < next.Need) {
			next = d
		}
	}
	if next.Status == NeedMore {
		return next
	}
	return noMatch("unknown_protocol")
}

func structural(b []byte, end bool, network, app string, udpConfirmed bool) Detection {
	if len(b) > MaxPrefix {
		return Detection{Status: Unavailable, Reason: "prefix_budget_exceeded"}
	}
	match := func(variant string) Detection {
		return Detection{Protocol: app, Variant: variant, Status: Match, Evidence: StructuralEvidence, Reason: "complete_structure"}
	}
	if network == "udp" {
		if app != "socks5" {
			return noMatch("not_datagram_protocol")
		}
		if len(b) < 4 || b[0] != 0 || b[1] != 0 {
			return noMatch("not_socks5_udp")
		}
		_, state := addressLength(b, 3)
		if state != 0 {
			return noMatch("invalid_socks5_udp_address")
		}
		d := match("socks5-udp")
		if !udpConfirmed {
			d.Evidence = Probable
			d.Reason = "unassociated_udp_structure"
		}
		return d
	}
	if network != "tcp" {
		return noMatch("unsupported_network")
	}
	if len(b) == 0 {
		return need(1, end)
	}
	switch app {
	case "socks5":
		if b[0] != 5 {
			return noMatch("not_socks5")
		}
		if len(b) < 2 {
			return need(2, end)
		}
		if b[1] == 0 {
			return noMatch("invalid_socks5_method_count")
		}
		n := 2 + int(b[1])
		if len(b) < n {
			return need(n, end)
		}
		// 0xff is a server refusal, never an offered client method.
		for _, m := range b[2:n] {
			if m == 255 {
				return noMatch("invalid_socks5_method")
			}
		}
		return match("socks5-greeting")
	case "socks4":
		if b[0] != 4 {
			return noMatch("not_socks4")
		}
		if len(b) < 2 {
			return need(2, end)
		}
		if b[1] != 1 && b[1] != 2 {
			return noMatch("invalid_socks4_command")
		}
		if len(b) < 9 {
			return need(9, end)
		}
		i := bytes.IndexByte(b[8:], 0)
		if i < 0 {
			if len(b) >= 1033 {
				return noMatch("socks4_user_too_large")
			}
			return need(len(b)+1, end)
		}
		n := 9 + i
		if b[4] == 0 && b[5] == 0 && b[6] == 0 && b[7] != 0 {
			if len(b) <= n {
				return need(n+1, end)
			}
			j := bytes.IndexByte(b[n:], 0)
			if j < 0 {
				if len(b)-n >= 256 {
					return noMatch("socks4a_domain_too_large")
				}
				return need(len(b)+1, end)
			}
			if j == 0 || j > 255 {
				return noMatch("invalid_socks4a_domain")
			}
			for _, c := range b[n : n+j] {
				if c <= 32 || c >= 127 {
					return noMatch("invalid_socks4a_domain")
				}
			}
			return match("socks4a-request")
		}
		return match("socks4-request")
	case "http":
		preface := []byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")
		if bytes.HasPrefix(b, preface) {
			return match("h2c")
		}
		if bytes.HasPrefix(preface, b) {
			if len(b) < len(preface) {
				return need(len(preface), end)
			}
			return match("h2c")
		}
		space := bytes.IndexByte(b, ' ')
		if space < 0 {
			for _, c := range b {
				if !httpToken(c) {
					return noMatch("not_http")
				}
			}
			if len(b) >= 32 {
				return noMatch("http_method_too_large")
			}
			return need(len(b)+1, end)
		}
		if space == 0 || space > 32 {
			return noMatch("not_http")
		}
		for _, c := range b[:space] {
			if !httpToken(c) {
				return noMatch("not_http")
			}
		}
		line := bytes.Index(b, []byte("\r\n"))
		if line < 0 {
			if len(b) > 8192 {
				return noMatch("http_line_too_large")
			}
			return need(len(b)+1, end)
		}
		fields := strings.Split(string(b[:line]), " ")
		if len(fields) != 3 || fields[1] == "" || (fields[2] != "HTTP/1.0" && fields[2] != "HTTP/1.1") {
			return noMatch("not_http")
		}
		return match("http1")
	}
	return noMatch("unsupported_protocol")
}

func httpToken(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c))
}

// addressLength returns the exclusive end of a SOCKS ATYP/ADDR/PORT field.
// state is -1 for invalid, +N for the next total byte count, 0 for complete.
func addressLength(b []byte, offset int) (int, int) {
	if len(b) <= offset {
		return 0, offset + 1
	}
	n := offset + 1
	switch b[offset] {
	case 1:
		n += 4
	case 4:
		n += 16
	case 3:
		if len(b) <= n {
			return 0, n + 1
		}
		if b[n] == 0 {
			return 0, -1
		}
		n += 1 + int(b[n])
	default:
		return 0, -1
	}
	n += 2
	if len(b) < n {
		return 0, n
	}
	return n, 0
}
