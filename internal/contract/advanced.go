package contract

import (
	"errors"
	"net"
	"strconv"
	"strings"
)

type Backend struct {
	Target   string `json:"target"`
	Weight   int    `json:"weight"`
	Disabled bool   `json:"disabled"`
}

// SharedTLS routes the unmodified TLS handshake to the selected rule's target.
type SharedTLS struct {
	ParentID   string `json:"parent_id,omitempty"`
	ServerName string `json:"server_name"`
}

func (r Rule) Advanced() bool {
	return len(r.Backends) > 0 || r.SharedTLS != nil || r.Tunnel != nil && (r.Tunnel.Mux || r.Tunnel.Reverse != "")
}

func (r Rule) ValidateAdvanced() error {
	if err := r.ValidateProxy(); err != nil {
		return err
	}
	if len(r.Backends) > 16 {
		return errors.New("at most 16 backends")
	}
	seen := map[string]bool{}
	active := 0
	for _, b := range r.Backends {
		h, p, e := net.SplitHostPort(b.Target)
		n, _ := strconv.Atoi(p)
		if e != nil || h == "" || n < 1 || n > 65535 || b.Weight < 1 || b.Weight > 100 || seen[b.Target] {
			return errors.New("invalid or repeated backend target/weight")
		}
		seen[b.Target] = true
		if !b.Disabled {
			active++
		}
	}
	if len(r.Backends) > 0 && (active == 0 || r.Network != "tcp") {
		return errors.New("weighted health-checked backends require TCP and an enabled target")
	}
	if t := r.SharedTLS; t != nil {
		if r.Network != "tcp" || t.ParentID == r.ID && r.ID != "" || !ValidServerName(t.ServerName) {
			return errors.New("shared TLS requires TCP, an exact DNS SNI and a distinct parent")
		}
	}
	if t := r.Tunnel; t != nil {
		if len(t.Reverse) > 128 || strings.TrimSpace(t.Reverse) != t.Reverse {
			return errors.New("invalid reverse identity")
		}
		if t.Reverse != "" && len(t.Chain) > 0 {
			return errors.New("reverse route cannot contain additional exits")
		}
	}
	return nil
}

func ValidServerName(s string) bool {
	if len(s) == 0 || len(s) > 253 || s != strings.ToLower(s) || net.ParseIP(s) != nil {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}
