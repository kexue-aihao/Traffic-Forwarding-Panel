package contract

import (
	"errors"
	"net/netip"
)

// ProxyProtocol accepts headers only from explicitly trusted upstream addresses.
type ProxyProtocol struct {
	Accept       string   `json:"accept"`
	Send         string   `json:"send"`
	TrustedCIDRs []string `json:"trusted_cidrs"`
}

func (r Rule) ValidateProxy() error {
	p := r.ProxyProtocol
	if p == nil {
		return nil
	}
	if r.Network != "tcp" {
		return errors.New("Proxy Protocol supports TCP only")
	}
	for _, v := range []string{p.Accept, p.Send} {
		if v != "" && v != "off" && v != "v1" && v != "v2" {
			return errors.New("Proxy Protocol version must be off, v1 or v2")
		}
	}
	if len(p.TrustedCIDRs) > 32 {
		return errors.New("at most 32 trusted proxy networks")
	}
	if p.Accept != "" && p.Accept != "off" && (len(p.TrustedCIDRs) == 0 || r.SharedTLS != nil) {
		return errors.New("receiving Proxy Protocol requires trusted networks and a dedicated listener")
	}
	for _, cidr := range p.TrustedCIDRs {
		if _, err := netip.ParsePrefix(cidr); err != nil {
			return errors.New("invalid trusted proxy CIDR")
		}
	}
	return nil
}
