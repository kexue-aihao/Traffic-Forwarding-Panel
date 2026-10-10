package agent

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

// BusinessProfile belongs to the operator's business service. It is deliberately
// separate from the certificate used to encrypt a TFP tunnel.
type BusinessProfile struct {
	Certificate    string   `json:"certificate,omitempty"`
	PrivateKey     string   `json:"private_key,omitempty"`
	CA             string   `json:"ca,omitempty"`
	ServerName     string   `json:"server_name,omitempty"`
	AllowedListen  []string `json:"allowed_listen,omitempty"`
	AllowedTargets []string `json:"allowed_targets,omitempty"`
}

type preparedBusiness struct {
	inbound    *tls.Config
	upstream   *tls.Config
	websocket  bool
	generation string
}

func LoadBusinessProfiles(path string) (map[string]BusinessProfile, error) {
	if path == "" {
		return nil, nil
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return nil, errors.New("business profile file must be regular and at most 1 MiB")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("business profile file must not be accessible by group or others")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("business profile file cannot be opened")
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || info.Size() > 1<<20 {
		return nil, errors.New("invalid business profile file")
	}
	d := json.NewDecoder(io.LimitReader(f, 1<<20))
	d.DisallowUnknownFields()
	var profiles map[string]BusinessProfile
	if d.Decode(&profiles) != nil || d.Decode(new(any)) != io.EOF || len(profiles) > 128 {
		return nil, errors.New("invalid business profile file")
	}
	for label := range profiles {
		if label == "" || len(label) > 64 || strings.IndexFunc(label, func(c rune) bool {
			return c != '-' && c != '_' && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9')
		}) >= 0 {
			return nil, errors.New("invalid business profile label")
		}
	}
	return profiles, nil
}

func prepareBusiness(v *contract.BusinessInbound, profiles map[string]BusinessProfile, listen, target string, incoming bool) (*preparedBusiness, error) {
	if v == nil {
		return nil, nil
	}
	p := &preparedBusiness{websocket: v.WebSocket}
	h := sha256.New()
	load := func(label string) (BusinessProfile, error) {
		profile, ok := profiles[label]
		if !ok {
			return profile, errors.New("business TLS profile unavailable")
		}
		if !containsLocal(profile.AllowedTargets, target) {
			return profile, errors.New("business target denied by local profile")
		}
		b, _ := json.Marshal(profile)
		h.Write(b)
		return profile, nil
	}
	if v.TLSProfile != "" && incoming {
		profile, err := load(v.TLSProfile)
		if err != nil {
			return nil, err
		}
		if !containsLocal(profile.AllowedListen, listen) {
			return nil, errors.New("business listener denied by local profile")
		}
		cert, err := tls.LoadX509KeyPair(profile.Certificate, profile.PrivateKey)
		if err != nil {
			return nil, errors.New("business certificate cannot be loaded")
		}
		for _, der := range cert.Certificate {
			h.Write(der)
		}
		p.inbound = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}}
	}
	if v.UpstreamTLSProfile != "" {
		profile, err := load(v.UpstreamTLSProfile)
		if err != nil {
			return nil, err
		}
		name := profile.ServerName
		if name == "" {
			name, _, err = net.SplitHostPort(target)
			if err != nil {
				return nil, errors.New("business upstream identity unavailable")
			}
		}
		roots, err := x509.SystemCertPool()
		if err != nil {
			roots = x509.NewCertPool()
		}
		if profile.CA != "" {
			pem, err := os.ReadFile(profile.CA)
			if err != nil || !roots.AppendCertsFromPEM(pem) {
				return nil, errors.New("business upstream CA cannot be loaded")
			}
			h.Write(pem)
		}
		p.upstream = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: name, NextProtos: []string{"http/1.1"}}
	}
	p.generation = hex.EncodeToString(h.Sum(nil))
	return p, nil
}

func (p *preparedBusiness) accept(ctx context.Context, raw net.Conn) (net.Conn, error) {
	if p == nil || p.inbound == nil {
		return raw, nil
	}
	c := tls.Server(raw, p.inbound.Clone())
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := c.HandshakeContext(ctx); err != nil {
		return nil, errors.New("business TLS handshake rejected")
	}
	return c, nil
}

func (p *preparedBusiness) connect(ctx context.Context, raw net.Conn) (net.Conn, error) {
	if p == nil || p.upstream == nil {
		return raw, nil
	}
	c := tls.Client(raw, p.upstream.Clone())
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := c.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, errors.New("business upstream TLS identity rejected")
	}
	return c, nil
}

func businessProfileStatuses(profiles map[string]BusinessProfile) []contract.InspectionProfileStatus {
	labels := make([]string, 0, len(profiles))
	for label := range profiles {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	result := make([]contract.InspectionProfileStatus, 0, len(labels))
	for _, label := range labels {
		p := profiles[label]
		s := contract.InspectionProfileStatus{Label: label, Protocol: "business-tls", Networks: []string{"tcp"}, Ready: len(p.AllowedTargets) > 0}
		if p.Certificate != "" || p.PrivateKey != "" {
			if _, err := tls.LoadX509KeyPair(p.Certificate, p.PrivateKey); err != nil {
				s.Ready = false
				s.Reason = "business_certificate_unavailable"
			} else {
				s.Variants = append(s.Variants, "server")
			}
		}
		if p.CA != "" {
			if pem, err := os.ReadFile(p.CA); err != nil || !x509.NewCertPool().AppendCertsFromPEM(pem) {
				s.Ready = false
				s.Reason = "business_ca_unavailable"
			}
		}
		if s.Ready {
			s.Variants = append(s.Variants, "upstream")
		}
		result = append(result, s)
	}
	return result
}
