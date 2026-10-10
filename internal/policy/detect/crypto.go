package detect

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5" // EVP_BytesToKey is prescribed by Shadowsocks2017 only.
	"encoding/base64"
	"errors"
	"net/netip"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/crypto/chacha20poly1305"
)

func compileProfile(p Profile) (compiledProfile, error) {
	c := compiledProfile{protocol: p.Protocol, method: p.Method, networks: []string{"tcp"}}
	if e := profileSecretBounds(p); e != nil {
		return c, e
	}
	if p.Protocol != "socks5" && (!requireEmpty(p.RelayEndpoint) || len(p.ControlRuleIDs) > 0) {
		return c, errors.New("unexpected association configuration")
	}
	switch p.Protocol {
	case "shadowsocks":
		if !requireEmpty(p.UUID, p.TrojanHash, p.UDPMode) {
			return c, errors.New("unexpected credential field")
		}
		var size int
		switch p.Method {
		case "aes-128-gcm", "2022-blake3-aes-128-gcm":
			size = 16
		case "aes-256-gcm", "chacha20-ietf-poly1305", "2022-blake3-aes-256-gcm", "2022-blake3-chacha20-poly1305":
			size = 32
		default:
			return c, errors.New("unsupported Shadowsocks cipher; legacy stream cannot be authenticated")
		}
		if strings.HasPrefix(p.Method, "2022-") {
			if p.Key == "" || p.Password != "" {
				return c, errors.New("Shadowsocks2022 requires base64 PSK key")
			}
			keys := strings.Split(p.Key, ":")
			if len(keys) > 4 {
				return c, errors.New("identity chain exceeds limit")
			}
			if len(keys) > 1 && strings.Contains(p.Method, "chacha") {
				return c, errors.New("SIP023 requires AES-GCM")
			}
			for _, s := range keys {
				b, e := base64.StdEncoding.Strict().DecodeString(s)
				if e != nil || len(b) != size {
					return c, errors.New("invalid PSK key length or encoding")
				}
				c.keys = append(c.keys, b)
			}
			c.variant = "aead2022"
			if len(keys) > 1 {
				c.variant = "aead2022-sip023"
			}
		} else {
			if (p.Key == "") == (p.Password == "") {
				return c, errors.New("exactly one Shadowsocks key or password required")
			}
			if p.Key != "" {
				b, e := base64.StdEncoding.Strict().DecodeString(p.Key)
				if e != nil || len(b) != size {
					return c, errors.New("invalid master key length or encoding")
				}
				c.keys = [][]byte{b}
			} else {
				c.keys = [][]byte{ssPasswordKey([]byte(p.Password), size)}
			}
			c.variant = "aead2017"
		}
		c.networks = []string{"tcp", "udp"}
	case "vmess":
		if !requireEmpty(p.Key, p.Password, p.TrojanHash, p.UDPMode) {
			return c, errors.New("unexpected credential field")
		}
		id, e := uuid.Parse(p.UUID)
		if e != nil {
			return c, errors.New("invalid VMess UUID")
		}
		c.keys = [][]byte{append([]byte(nil), id[:]...)}
		// CmdKey is the exact reference protocol KDF, not a new password scheme.
		c.vmessKey = vmessCommandKey(id[:])
		if p.Method == "" || p.Method == "aead" {
			c.variant = "aead"
		} else if p.Method == "legacy" {
			c.variant = "legacy"
			c.legacy = true
		} else {
			return c, errors.New("unsupported VMess authentication version")
		}
	case "trojan":
		if !requireEmpty(p.Key, p.UUID, p.Method, p.UDPMode) {
			return c, errors.New("unexpected credential field")
		}
		var e error
		c.trojanHash, e = decodeTrojanHash(p)
		if e != nil {
			return c, e
		}
		c.variant = "trojan-sha224"
	case "socks5":
		if !requireEmpty(p.Key, p.UUID, p.TrojanHash, p.Password, p.Method) {
			return c, errors.New("unexpected SOCKS profile credential")
		}
		switch p.UDPMode {
		case "structural":
			if p.RelayEndpoint != "" || len(p.ControlRuleIDs) > 0 {
				return c, errors.New("structural profile cannot claim an association")
			}
			c.udpStructural = true
			c.variant = "socks5-udp-structural"
		case "associated":
			endpoint, e := netip.ParseAddrPort(p.RelayEndpoint)
			if e != nil || endpoint.Addr().IsUnspecified() || endpoint.Port() == 0 || len(p.ControlRuleIDs) == 0 || len(p.ControlRuleIDs) > 16 || len(p.RuleIDs) == 0 || len(p.Targets) == 0 {
				return c, errors.New("associated SOCKS profile requires explicit numeric relay endpoint, control rules, UDP rules and targets")
			}
			c.udpAssociated = true
			c.relayEndpoint = netip.AddrPortFrom(endpoint.Addr().Unmap(), endpoint.Port())
			c.controlRules = append([]string(nil), p.ControlRuleIDs...)
			c.variant = "socks5-udp-associated"
		default:
			return c, errors.New("SOCKS profile must explicitly select udp_mode structural or associated")
		}
		c.networks = []string{"tcp", "udp"}
	default:
		return c, errors.New("unsupported local inspection profile protocol")
	}
	return c, nil
}

// Shadowsocks2017 uses EVP_BytesToKey's MD5 expansion. This is wire-protocol
// compatibility, not a password-storage scheme. 2022 never uses this function.
func ssPasswordKey(password []byte, size int) []byte {
	out := make([]byte, 0, size+16)
	var previous []byte
	for len(out) < size {
		h := md5.New()
		h.Write(previous)
		h.Write(password)
		previous = h.Sum(nil)
		out = append(out, previous...)
	}
	return out[:size]
}

func newAEAD(method string, key []byte) (cipher.AEAD, error) {
	if strings.Contains(method, "chacha20") {
		return chacha20poly1305.New(key)
	}
	b, e := aes.NewCipher(key)
	if e != nil {
		return nil, e
	}
	return cipher.NewGCM(b)
}
