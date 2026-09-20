package tunnel

import (
	"crypto/subtle"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

func hopAddress(h contract.TunnelHop) (string, error) {
	address := h.Endpoint
	switch h.Transport {
	case "tls", "http":
	case "ws", "wss":
		u, e := url.Parse(address)
		if e != nil || u.Scheme != h.Transport || u.User != nil || u.Host == "" || u.Fragment != "" {
			return "", errors.New("invalid chain endpoint")
		}
		address = u.Host
		if u.Port() == "" {
			port := "80"
			if h.Transport == "wss" {
				port = "443"
			}
			address = net.JoinHostPort(u.Hostname(), port)
		}
	default:
		return "", errors.New("unsupported chain transport")
	}
	host, port, e := net.SplitHostPort(address)
	if e != nil || host == "" || port == "" {
		return "", errors.New("invalid chain address")
	}
	p, e := strconv.Atoi(port)
	if e != nil || p < 1 || p > 65535 {
		return "", errors.New("invalid chain port")
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if ip, e := netip.ParseAddr(host); e == nil {
		host = ip.Unmap().String()
	}
	return net.JoinHostPort(host, strconv.Itoa(p)), nil
}

// ValidateChain validates the complete route before opening any connection.
// DNS aliases are caught by each exit's stable NodeID during the handshake.
func ValidateChain(first contract.TunnelHop, remaining []contract.TunnelHop) error {
	if len(remaining) > 2 {
		return errors.New("at most three tunnel exits are supported")
	}
	seen := map[string]bool{}
	for _, h := range append([]contract.TunnelHop{first}, remaining...) {
		address, e := hopAddress(h)
		if e != nil {
			return e
		}
		if len(h.Token) < 16 {
			return errors.New("tunnel token must contain at least 16 characters")
		}
		if seen[address] {
			return errors.New("tunnel chain cycle")
		}
		seen[address] = true
	}
	return nil
}

func validNodeID(id string) bool {
	return id != "" && len(id) <= 128 && strings.TrimSpace(id) == id
}

func (s *Server) validateRoute(req openRequest) error {
	if req.Version == 1 {
		if len(req.Chain) > 0 || len(req.Visited) > 0 {
			return errors.New("chain requires data plane version 2")
		}
		return nil
	}
	if !validNodeID(s.NodeID) {
		return errors.New("chain requires a stable exit identity")
	}
	if len(req.Visited)+len(req.Chain)+1 > 3 {
		return errors.New("at most three tunnel exits are supported")
	}
	seen := map[string]bool{s.NodeID: true}
	for _, id := range req.Visited {
		if !validNodeID(id) || seen[id] {
			return errors.New("invalid or repeated tunnel exit identity")
		}
		seen[id] = true
	}
	if len(req.Chain) > 0 {
		return ValidateChain(req.Chain[0], req.Chain[1:])
	}
	return nil
}
func (s *Server) authorizedNext(requested contract.TunnelHop) (contract.TunnelHop, bool) {
	for _, allowed := range s.NextHops {
		if allowed.Transport == requested.Transport && allowed.Endpoint == requested.Endpoint && allowed.ServerName == requested.ServerName && subtle.ConstantTimeCompare([]byte(allowed.Token), []byte(requested.Token)) == 1 {
			return allowed, true
		}
	}
	return contract.TunnelHop{}, false
}
func relayPackets(a, b *Session, idle time.Duration) {
	var wg sync.WaitGroup
	wg.Add(2)
	copyOne := func(dst, src *Session) {
		defer wg.Done()
		defer a.Close()
		defer b.Close()
		for {
			src.SetReadDeadline(time.Now().Add(idle))
			p, e := src.ReadPacket()
			if e != nil {
				return
			}
			dst.SetWriteDeadline(time.Now().Add(idle))
			if e = dst.WritePacket(p); e != nil {
				return
			}
		}
	}
	go copyOne(a, b)
	go copyOne(b, a)
	wg.Wait()
}
