package contract

import (
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
)

type Exit struct {
	ServiceReady *bool    `json:"service_ready,omitempty"`
	Managed      bool     `json:"managed,omitempty"`
	ReverseHub   bool     `json:"reverse_hub,omitempty"`
	LocalProfile string   `json:"local_profile,omitempty"`
	Listen       string   `json:"listen,omitempty"`
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	GroupID      string   `json:"group_id"`
	NodeID       string   `json:"node_id"`
	Transport    string   `json:"transport"`
	Tunnel       Tunnel   `json:"tunnel"`
	UDP          *UDPExit `json:"udp,omitempty"`
	Weight       int      `json:"weight"`
	Enabled      bool     `json:"enabled"`
	Version      int64    `json:"version"`
	Online       bool     `json:"online"`
}

// UDPExit enables an independent authenticated QUIC DATAGRAM listener.
type UDPExit struct {
	Endpoint         string `json:"endpoint"`
	ServerName       string `json:"server_name"`
	Token            string `json:"token,omitempty"`
	AllowTCPFallback bool   `json:"allow_tcp_fallback,omitempty"`
}

func ValidateUDPExit(u *UDPExit) error {
	if u == nil {
		return nil
	}
	h, p, e := net.SplitHostPort(u.Endpoint)
	port, _ := strconv.Atoi(p)
	if e != nil || h == "" || port < 1 || port > 65535 || strings.ContainsAny(h, " /\\\r\n") || len(u.Token) < 16 || len(u.Token) > 128 || len(u.ServerName) > 253 {
		return errors.New("UDP exit requires host:port, verification name and token (16-128 chars)")
	}
	return nil
}

// ExitListeningPorts describes physical listener reservations independently of
// endpoint IP or certificate name. TCP and UDP may share a numeric port.
func ExitListeningPorts(e Exit) (map[string]int, error) {
	if e.Tunnel.Reverse != "" {
		return map[string]int{}, nil
	}
	address := e.Tunnel.Endpoint
	if e.Managed {
		address = e.Listen
	}
	transport := e.Transport
	if e.Managed {
		transport = "tls"
	}
	port, err := exitEndpointPort(transport, address)
	if err != nil {
		return nil, err
	}
	ports := map[string]int{"tcp": port}
	if e.UDP != nil {
		port, err = exitEndpointPort("quic", e.UDP.Endpoint)
		if err != nil {
			return nil, err
		}
		ports["udp"] = port
	}
	return ports, nil
}

func exitEndpointPort(transport, endpoint string) (int, error) {
	address := endpoint
	if transport == "ws" || transport == "wss" {
		u, e := url.Parse(endpoint)
		if e != nil {
			return 0, e
		}
		address = u.Host
		if u.Port() == "" {
			if transport == "wss" {
				return 443, nil
			}
			return 80, nil
		}
	}
	_, p, e := net.SplitHostPort(address)
	n, _ := strconv.Atoi(p)
	if e != nil || n < 1 || n > 65535 {
		return 0, errors.New("invalid exit listening port")
	}
	return n, nil
}
