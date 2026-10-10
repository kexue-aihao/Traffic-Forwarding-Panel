// Package policy compiles and executes the common control-plane/Agent policy.
package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"golang.org/x/net/idna"
)

var ErrDenied = errors.New("inbound policy denied")

func Host(value string) (string, error) {
	value = strings.TrimSpace(value)
	if h, p, err := net.SplitHostPort(value); err == nil {
		if p == "" {
			return "", errors.New("empty host port")
		}
		for _, c := range p {
			if c < '0' || c > '9' {
				return "", errors.New("invalid host port")
			}
		}
		port, e := strconv.Atoi(p)
		if e != nil || port < 1 || port > 65535 {
			return "", errors.New("invalid host port")
		}
		value = h
	}
	if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
		inner := value[1 : len(value)-1]
		if ip, e := netip.ParseAddr(inner); e == nil {
			return ip.Unmap().String(), nil
		}
		return "", errors.New("invalid bracketed host")
	}
	value = strings.TrimSuffix(value, ".")
	if ip, err := netip.ParseAddr(value); err == nil {
		return ip.Unmap().String(), nil
	}
	value, err := idna.Lookup.ToASCII(strings.ToLower(value))
	if err != nil || !contract.ValidServerName(value) {
		return "", errors.New("invalid host name")
	}
	return value, nil
}

func HostPattern(value string) (string, error) {
	value = strings.TrimSpace(value)
	prefix := ""
	if strings.HasPrefix(value, "*.") {
		prefix, value = "*.", value[2:]
	}
	value, err := Host(value)
	if err != nil {
		return "", err
	}
	if prefix != "" {
		if _, err := netip.ParseAddr(value); err == nil {
			return "", errors.New("IP wildcard not supported")
		}
	}
	return prefix + value, nil
}

func MatchHost(pattern, host string) bool {
	if strings.HasPrefix(pattern, "*.") {
		return strings.HasSuffix(host, pattern[1:]) && len(host) > len(pattern)-1
	}
	return pattern == host
}

// MatchPath uses a bounded glob algorithm: '*' crosses '/' and '?' consumes
// one byte. No regexp backtracking and no filesystem-specific semantics.
func MatchPath(pattern, path string) bool {
	i, j, star, retry := 0, 0, -1, 0
	for j < len(path) {
		if i < len(pattern) && (pattern[i] == '?' || pattern[i] == path[j]) {
			i++
			j++
			continue
		}
		if i < len(pattern) && pattern[i] == '*' {
			star, retry = i, j
			i++
			continue
		}
		if star < 0 {
			return false
		}
		retry++
		j, i = retry, star+1
	}
	for i < len(pattern) && pattern[i] == '*' {
		i++
	}
	return i == len(pattern)
}

func Paths(raw string) ([]string, error) {
	if len(raw) > 8192 || !strings.HasPrefix(raw, "/") {
		return nil, errors.New("invalid HTTP path")
	}
	decoded, err := url.PathUnescape(raw)
	if err != nil || strings.ContainsAny(decoded, "\x00\r\n\\") {
		return nil, errors.New("invalid encoded path")
	}
	// A second decoding by the origin must not change the meaning.
	if strings.Contains(decoded, "%") {
		return nil, errors.New("ambiguous double encoded path")
	}
	segments := []string{}
	for _, p := range strings.Split(decoded, "/") {
		switch p {
		case "", ".":
		case "..":
			if len(segments) > 0 {
				segments = segments[:len(segments)-1]
			}
		default:
			segments = append(segments, p)
		}
	}
	canonical := "/" + strings.Join(segments, "/")
	if strings.HasSuffix(decoded, "/") && canonical != "/" {
		canonical += "/"
	}
	return []string{raw, decoded, canonical}, nil
}

func NormalizeLayers(layers []contract.InboundPolicy) error {
	if len(layers) > 8 {
		return errors.New("at most 8 policy layers")
	}
	for i := range layers {
		p := &layers[i]
		if err := NormalizeInspection(p.Inspection); err != nil {
			return err
		}
		for _, values := range []*[]string{&p.AllowedHosts, &p.BlockedHosts} {
			if len(*values) > 256 {
				return errors.New("at most 256 host patterns")
			}
			for j, value := range *values {
				n, err := HostPattern(value)
				if err != nil {
					return fmt.Errorf("host %q: %w", value, err)
				}
				(*values)[j] = n
			}
			slices.Sort(*values)
			*values = slices.Compact(*values)
		}
		if len(p.BlockedPaths) > 256 {
			return errors.New("at most 256 path patterns")
		}
		for _, path := range p.BlockedPaths {
			if len(path) > 2048 || !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "\x00\r\n\\") {
				return errors.New("invalid HTTP Path pattern")
			}
		}
		slices.Sort(p.BlockedPaths)
		p.BlockedPaths = slices.Compact(p.BlockedPaths)
		for j, app := range p.BlockedApps {
			canonical, ok := NormalizeApplication(app)
			if !ok {
				return errors.New("unsupported application detector")
			}
			p.BlockedApps[j] = canonical
		}
		slices.Sort(p.BlockedApps)
		p.BlockedApps = slices.Compact(p.BlockedApps)
	}
	return nil
}

func Seal(p *contract.EffectivePolicy) error {
	if p.Version != contract.GroupPolicyVersion {
		return errors.New("unsupported group policy version")
	}
	if err := NormalizeLayers(p.InboundLayers); err != nil {
		return err
	}
	if f := p.Failover; f != nil && (f.MaxFail < 0 || f.MaxFail > 1000 || f.CooldownSec < 0 || f.CooldownSec > 86400) {
		return errors.New("invalid failover policy")
	}
	p.Hash = ""
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	h := sha256.Sum256(data)
	p.Hash = hex.EncodeToString(h[:])
	return nil
}

func NeedsInspect(layers []contract.InboundPolicy) bool {
	for _, p := range layers {
		if p.Inspection != nil || p.TLSRequired || p.RejectEmptySNI || len(p.AllowedHosts)+len(p.BlockedHosts)+len(p.BlockedPaths)+len(p.BlockedApps) > 0 {
			return true
		}
	}
	return false
}

func NeedsHTTP(layers []contract.InboundPolicy) bool {
	for _, p := range layers {
		if len(p.AllowedHosts)+len(p.BlockedHosts)+len(p.BlockedPaths) > 0 {
			return true
		}
	}
	return false
}

func CheckHost(layers []contract.InboundPolicy, value string) error {
	host, err := Host(value)
	for _, p := range layers {
		if len(p.AllowedHosts) > 0 {
			matched := false
			for _, pattern := range p.AllowedHosts {
				if err == nil && MatchHost(pattern, host) {
					matched = true
					break
				}
			}
			if !matched {
				return fmt.Errorf("%w: allowed_host", ErrDenied)
			}
		}
		for _, pattern := range p.BlockedHosts {
			if err == nil && MatchHost(pattern, host) {
				return fmt.Errorf("%w: blocked_host", ErrDenied)
			}
		}
	}
	return nil
}

func CheckHTTP(layers []contract.InboundPolicy, host, rawPath string, upgrade bool) error {
	if err := CheckHost(layers, host); err != nil {
		return err
	}
	for _, p := range layers {
		if len(p.BlockedPaths) == 0 {
			continue
		}
		if upgrade {
			return fmt.Errorf("%w: opaque HTTP upgrade with path policy", ErrDenied)
		}
		paths, err := Paths(rawPath)
		if err != nil {
			return err
		}
		for _, pattern := range p.BlockedPaths {
			for _, path := range paths {
				if MatchPath(pattern, path) {
					return fmt.Errorf("%w: blocked_path", ErrDenied)
				}
			}
		}
	}
	return nil
}

func ParseTLS(value map[string]any) (contract.ReverseTLS, error) {
	var v contract.ReverseTLS
	b, err := json.Marshal(value)
	if err != nil {
		return v, err
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if len(value) > 0 {
		if err = d.Decode(&v); err != nil {
			return v, fmt.Errorf("tls: %w", err)
		}
	}
	if v.MinVersion != "" && v.MinVersion != "1.3" {
		return v, errors.New("tls.min_version must be 1.3")
	}
	if v.ServerName != "" {
		if v.ServerName, err = Host(v.ServerName); err != nil {
			return v, err
		}
	}
	for _, name := range []string{v.CAProfile, v.CertificateProfile, v.ClientCertificateProfile} {
		if len(name) > 64 || strings.ContainsAny(name, "/\\\r\n. ") {
			return v, errors.New("TLS profile must be a local label")
		}
	}
	for _, protocol := range v.ALPN {
		if protocol != "tfp-reverse-v1" {
			return v, errors.New("unsupported reverse ALPN")
		}
	}
	return v, nil
}
