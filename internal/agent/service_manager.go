package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/policy"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/policy/detect"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/tunnel"
)

// Paths and bind permissions are provided locally, never by the control plane.
type ServiceProfile struct {
	Certificate              string   `json:"certificate,omitempty"`
	PrivateKey               string   `json:"private_key,omitempty"`
	CA                       string   `json:"ca,omitempty"`
	RequireClientCertificate bool     `json:"require_client_certificate,omitempty"`
	AllowedListen            []string `json:"allowed_listen,omitempty"`
}

func LoadServiceProfiles(path string) (map[string]ServiceProfile, error) {
	if path == "" {
		return nil, nil
	}
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 1<<20))
	d.DisallowUnknownFields()
	var out map[string]ServiceProfile
	if e = d.Decode(&out); e != nil {
		return nil, e
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, errors.New("one service profile object required")
	}
	return out, nil
}

type serviceEntry struct {
	inspectionGeneration string
	config               contract.ServiceConfig
	server               *tunnel.Server
	routeTLS             *tls.Config
	udp                  *tunnel.DatagramServer
	udpAddress           string
	listener             *serviceListener
	cancel               context.CancelFunc
	status               contract.ServiceStatus
	reusedUDP            bool
}
type serviceSocket struct {
	listener net.Listener
	mu       sync.Mutex
	active   *serviceListener
}

// Each generation owns a virtual listener. Replacing it leaves the prebound
// physical socket available, and closes every old generation's accepted stream.
type serviceListener struct {
	socket   *serviceSocket
	incoming chan net.Conn
	done     chan struct{}
	once     sync.Once
	closed   bool
}

func (l *serviceListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.incoming:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *serviceListener) Close() error {
	l.once.Do(func() {
		l.socket.mu.Lock()
		defer l.socket.mu.Unlock()
		l.closed = true
		if l.socket.active == l {
			l.socket.active = nil
		}
		close(l.done)
		for {
			select {
			case c := <-l.incoming:
				c.Close()
			default:
				return
			}
		}
	})
	return nil
}
func (l *serviceListener) Addr() net.Addr { return l.socket.listener.Addr() }
func (s *serviceSocket) serve() {
	for {
		c, e := s.listener.Accept()
		if e != nil {
			return
		}
		s.mu.Lock()
		l := s.active
		if l == nil || l.closed {
			c.Close()
		} else {
			select {
			case l.incoming <- c:
			default:
				c.Close()
			}
		}
		s.mu.Unlock()
	}
}

type ServiceManager struct {
	InspectionProfiles detect.Profiles
	BusinessProfiles   map[string]BusinessProfile
	mu                 sync.Mutex
	Profiles           map[string]ServiceProfile
	BaseTLS            *tls.Config
	entries            map[string]*serviceEntry
	sockets            map[string]*serviceSocket
	expiry             *time.Timer
	generation         uint64
	closed             bool
}

func (m *ServiceManager) tlsConfig(v contract.ServiceConfig) (*tls.Config, error) {
	tc := &tls.Config{MinVersion: tls.VersionTLS13}
	if m.BaseTLS != nil {
		tc = m.BaseTLS.Clone()
	}
	tc.MinVersion = tls.VersionTLS13
	tc.InsecureSkipVerify = false
	tc.ServerName = v.TLS.ServerName
	tc.NextProtos = v.TLS.ALPN
	if len(tc.NextProtos) > 0 {
		previous := tc.VerifyConnection
		allowed := append([]string(nil), tc.NextProtos...)
		tc.VerifyConnection = func(state tls.ConnectionState) error {
			if previous != nil {
				if err := previous(state); err != nil {
					return err
				}
			}
			if !slices.Contains(allowed, state.NegotiatedProtocol) {
				return errors.New("required reverse ALPN was not negotiated")
			}
			return nil
		}
	}
	if v.TLS.Enabled != nil && !*v.TLS.Enabled {
		return nil, errors.New("reverse TLS disabled")
	}
	load := func(label string) (ServiceProfile, error) {
		p, ok := m.Profiles[label]
		if !ok {
			return p, errors.New("local service profile missing")
		}
		return p, nil
	}
	profile, e := load(v.Profile)
	if e != nil {
		return nil, e
	}
	if v.Kind != "reverse" {
		allowed := false
		for _, address := range profile.AllowedListen {
			if address == v.Listen {
				allowed = true
			}
		}
		if !allowed {
			return nil, errors.New("listener denied by local profile")
		}
	}
	if v.TLS.CAProfile != "" {
		p, e := load(v.TLS.CAProfile)
		if e != nil {
			return nil, e
		}
		data, e := os.ReadFile(p.CA)
		if e != nil {
			return nil, errors.New("local CA cannot be loaded")
		}
		pool, e := x509.SystemCertPool()
		if e != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(data) {
			return nil, errors.New("invalid local CA")
		}
		tc.RootCAs = pool
		tc.ClientCAs = pool
	}
	certLabel := v.TLS.CertificateProfile
	if v.Kind != "reverse" && certLabel == "" {
		certLabel = v.Profile
	}
	if v.Kind == "reverse" {
		certLabel = v.TLS.ClientCertificateProfile
	}
	if certLabel != "" {
		p, e := load(certLabel)
		if e != nil {
			return nil, e
		}
		cert, e := tls.LoadX509KeyPair(p.Certificate, p.PrivateKey)
		if e != nil {
			return nil, errors.New("local certificate cannot be loaded")
		}
		tc.Certificates = []tls.Certificate{cert}
	}
	if v.Kind != "reverse" {
		if len(tc.Certificates) == 0 {
			return nil, errors.New("listener certificate missing")
		}
		if profile.RequireClientCertificate {
			if tc.ClientCAs == nil {
				return nil, errors.New("mTLS requires a CA profile")
			}
			tc.ClientAuth = tls.RequireAndVerifyClientCert
		}
	}
	return tc, nil
}

// Compare loaded material, including trust roots, so replacing files under an
// unchanged local profile creates a new generation on the next config poll.
func sameServiceTLS(a, b *tls.Config) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.MinVersion != b.MinVersion || a.MaxVersion != b.MaxVersion || a.ClientAuth != b.ClientAuth || a.ServerName != b.ServerName || !slices.Equal(a.NextProtos, b.NextProtos) || !a.RootCAs.Equal(b.RootCAs) || !a.ClientCAs.Equal(b.ClientCAs) || len(a.Certificates) != len(b.Certificates) {
		return false
	}
	for i, c := range a.Certificates {
		other := b.Certificates[i]
		if !reflect.DeepEqual(c.Certificate, other.Certificate) || !reflect.DeepEqual(c.OCSPStaple, other.OCSPStaple) || !reflect.DeepEqual(c.SignedCertificateTimestamps, other.SignedCertificateTimestamps) {
			return false
		}
	}
	return true
}

func liveService(e *serviceEntry) bool {
	return e != nil && e.status.Error != "configuration expired" && e.status.Error != "managed service failed" && e.status.Error != "managed datagram service failed"
}

func validateService(v contract.ServiceConfig) error {
	if v.ID == "" || len(v.ID) > 128 || len(v.Token) < 16 || len(v.Token) > 128 || len(v.Grants) > 2048 || len(v.NextHops) > 2048 {
		return errors.New("invalid managed service")
	}
	for _, hop := range v.NextHops {
		if err := tunnel.ValidateChain(hop, nil); err != nil {
			return err
		}
	}
	count := 0
	for _, grant := range v.Grants {
		if grant.Identity == "" || len(grant.Identity) > 128 || len(grant.StreamToken) < 16 || len(grant.StreamToken) > 128 || len(grant.Token) > 128 || v.Kind != "exit" && len(grant.Token) < 16 {
			return errors.New("invalid service grant")
		}
		for _, target := range grant.Targets {
			count++
			h, p, err := net.SplitHostPort(target.Target)
			port, pe := strconv.Atoi(p)
			if count > 8192 || target.RuleID == "" || len(target.RuleID) > 190 || target.Network != "tcp" && target.Network != "udp" || err != nil || h == "" || pe != nil || port < 1 || port > 65535 {
				return errors.New("invalid service target grant")
			}
		}
	}
	return nil
}

// Prepare holds the manager lock until commit/abort. All profiles and socket
// bindings are checked before the runtime persists or switches configuration.
func (m *ServiceManager) Prepare(configs []contract.ServiceConfig, until time.Time) (func(), func(), error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, nil, net.ErrClosed
	}
	if len(configs) > 128 || !until.After(time.Now()) {
		m.mu.Unlock()
		return nil, nil, errors.New("managed service capacity or validity exceeded")
	}
	if m.entries == nil {
		m.entries = map[string]*serviceEntry{}
		m.sockets = map[string]*serviceSocket{}
	}
	next := map[string]*serviceEntry{}
	created := map[string]*serviceSocket{}
	staged := []*serviceEntry{}
	abort := func() {
		for _, v := range staged {
			if v.udp != nil && !v.reusedUDP {
				v.udp.Close()
			}
		}
		for _, s := range created {
			s.listener.Close()
		}
		m.mu.Unlock()
	}
	fail := func(e error) (func(), func(), error) { abort(); return nil, nil, e }
	used := map[string]bool{}
	reports := len(configs)
	for _, v := range configs {
		if validateService(v) != nil || next[v.ID] != nil {
			return fail(errors.New("invalid managed service"))
		}
		if v.Kind != "exit" && v.Kind != "hub" && v.Kind != "reverse" {
			return fail(errors.New("invalid service kind"))
		}
		if v.Kind == "hub" {
			reports += relationshipCount(v.Grants)
			if reports > 2048 {
				return fail(errors.New("managed service report capacity exceeded"))
			}
		}
		if v.Transport != "tls" && v.Transport != "tls_simple" && v.Transport != "ws" && v.Transport != "http" {
			return fail(errors.New("invalid managed carrier"))
		}
		if v.Transport == "tls_simple" && v.Kind == "hub" && relationshipCount(v.Grants) > 1 {
			return fail(errors.New("tls_simple requires a dedicated relationship"))
		}
		if v.Policy != nil {
			h := v.Policy.Hash
			if e := policy.Seal(v.Policy); e != nil || h != v.Policy.Hash {
				return fail(errors.New("invalid service policy"))
			}
		}
		tc, e := m.tlsConfig(v)
		if e != nil {
			return fail(fmt.Errorf("service %s: %w", v.ID, e))
		}
		var routeTLS *tls.Config
		if v.Kind == "hub" {
			outbound := v
			outbound.Kind = "reverse"
			routeTLS, e = m.tlsConfig(outbound)
			if e != nil {
				return fail(fmt.Errorf("service %s outbound TLS: %w", v.ID, e))
			}
		}
		plans, origins, inspectionGeneration, e := prepareServiceInspection(v, m.InspectionProfiles, m.BusinessProfiles)
		if e != nil {
			return fail(fmt.Errorf("service %s inspection: %w", v.ID, e))
		}
		if old := m.entries[v.ID]; liveService(old) && old.inspectionGeneration == inspectionGeneration && reflect.DeepEqual(old.config, v) && sameServiceTLS(old.server.TLS, tc) && sameServiceTLS(old.routeTLS, routeTLS) {
			next[v.ID] = old
			if v.Kind != "reverse" {
				if used[v.Listen] {
					return fail(errors.New("duplicate service listen"))
				}
				used[v.Listen] = true
			}
			continue
		}
		entry := &serviceEntry{config: v, inspectionGeneration: inspectionGeneration, routeTLS: routeTLS, status: contract.ServiceStatus{ID: v.ID}}
		entry.server = &tunnel.Server{InspectionPlans: plans, OriginTLS: origins, Managed: true, Token: v.Token, Grants: v.Grants, Policy: v.Policy, TLS: tc, NodeID: v.ID, NextHops: v.NextHops, Client: tunnel.Client{TLS: tc}}
		entry.server.InspectionLocation = "exit"
		if v.Kind == "reverse" {
			entry.server.InspectionLocation = "reverse"
		}
		if v.Kind != "reverse" {
			if used[v.Listen] {
				return fail(errors.New("duplicate service listen"))
			}
			used[v.Listen] = true
			socket := m.sockets[v.Listen]
			if socket == nil {
				socket = created[v.Listen]
			}
			if socket == nil {
				l, e := net.Listen("tcp", v.Listen)
				if e != nil {
					return fail(e)
				}
				socket = &serviceSocket{listener: l}
				created[v.Listen] = socket
			}
			entry.listener = &serviceListener{socket: socket, incoming: make(chan net.Conn, 64), done: make(chan struct{})}
			if v.UDP != nil {
				profile := m.Profiles[v.Profile]
				_, port, e := net.SplitHostPort(v.UDP.Endpoint)
				if e != nil {
					return fail(e)
				}
				h, _, _ := net.SplitHostPort(v.Listen)
				addr := net.JoinHostPort(h, port)
				if !containsLocal(profile.AllowedListen, addr) {
					return fail(errors.New("UDP listener denied by profile"))
				}
				entry.udpAddress = addr
				old := m.entries[v.ID]
				if liveService(old) && old.udp != nil && old.udpAddress == addr {
					entry.udp = old.udp
					entry.reusedUDP = true
				}
				ds := &tunnel.DatagramServer{TLS: tc, Token: v.UDP.Token, Authorize: func(token, target string) bool {
					for _, g := range v.Grants {
						if token != g.StreamToken {
							continue
						}
						for _, t := range g.Targets {
							if t.Network == "udp" && t.Target == target {
								return true
							}
						}
					}
					return false
				}}
				if entry.udp == nil {
					if e = ds.Listen(addr); e != nil {
						return fail(e)
					}
					entry.udp = ds
				}
			}
		}
		next[v.ID] = entry
		staged = append(staged, entry)
	}
	commit := func() {
		for addr, s := range created {
			m.sockets[addr] = s
			go s.serve()
		}
		for _, entry := range staged {
			if entry.listener != nil {
				s := entry.listener.socket
				s.mu.Lock()
				s.active = entry.listener
				s.mu.Unlock()
			}
		}
		for id, entry := range m.entries {
			if next[id] != entry {
				if replacement := next[id]; replacement != nil && replacement.reusedUDP {
					entry.udp = nil
				}
				entry.close()
			}
		}
		m.entries = next
		for addr, s := range m.sockets {
			if !used[addr] {
				s.listener.Close()
				delete(m.sockets, addr)
			}
		}
		for _, entry := range staged {
			ctx, cancel := context.WithCancel(context.Background())
			entry.cancel = cancel
			entry.server.OnReverseState = func(ready bool, e error) {
				m.mu.Lock()
				defer m.mu.Unlock()
				entry.status.Ready = ready
				entry.status.Error = ""
				if e != nil {
					entry.status.Error = "reverse carrier unavailable"
				}
			}
			if entry.config.Kind == "reverse" {
				go func() {
					e := entry.server.RunReverse(ctx, tunnel.Client{TLS: entry.server.TLS, PreferIPv6: entry.config.PreferIPv6}, entry.config.Transport, entry.config.Endpoint, entry.config.TLS.ServerName, entry.config.Identity)
					if e != nil {
						m.failed(entry)
					}
				}()
			} else {
				entry.status.Ready = true
				go func() {
					if e := entry.server.Serve(entry.listener, entry.config.Transport); e != nil && ctx.Err() == nil {
						m.failed(entry)
					}
				}()
				if entry.udp != nil {
					entry.udp.UpdateTLS(entry.server.TLS)
					config := entry.config
					apps := []string{}
					if config.Policy != nil {
						for _, p := range config.Policy.InboundLayers {
							apps = append(apps, p.BlockedApps...)
						}
					}
					entry.udp.UpdateAuthorization(func(token, target string) bool {
						for _, g := range config.Grants {
							if token != g.StreamToken {
								continue
							}
							for _, t := range g.Targets {
								if t.Network == "udp" && t.Target == target {
									return true
								}
							}
						}
						return false
					}, apps)
					entry.udp.UpdateInspection(func(token, target string, payload []byte) bool {
						for _, g := range config.Grants {
							if token == g.StreamToken {
								for _, t := range g.Targets {
									if t.Network == "udp" && t.Target == target {
										var layers []contract.InboundPolicy
										if config.Policy != nil {
											layers = config.Policy.InboundLayers
										}
										plan := entry.server.InspectionPlans[tunnel.InspectionKey(t.RuleID, t.Network, t.Target)]
										detection, blockedPacket := policy.DatagramDecision(payload, layers, plan)
										if plan != nil && !plan.Empty() {
											entry.server.RecordInspection(t.RuleID, detection, "raw")
										}
										if blockedPacket {
											entry.server.RejectPolicy(t.RuleID)
											return true
										}
										return false
									}
								}
							}
						}
						return true
					})
					if !entry.reusedUDP {
						ds := entry.udp
						go func() {
							if e := ds.ServeBound(); e != nil {
								m.failedDatagram(entry.config.ID, ds)
							}
						}()
					}
				}
			}
		}
		if m.expiry != nil {
			m.expiry.Stop()
		}
		m.generation++
		generation := m.generation
		m.expiry = time.AfterFunc(time.Until(until), func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			if m.closed || m.generation != generation {
				return
			}
			for _, v := range m.entries {
				v.close()
				v.status.Ready = false
				v.status.Error = "configuration expired"
			}
		})
		m.mu.Unlock()
	}
	return commit, abort, nil
}
func containsLocal(values []string, s string) bool {
	for _, v := range values {
		if v == s {
			return true
		}
	}
	return false
}

// The entry connects to its own managed hub using the same locally prepared
// trust, ALPN and client-certificate policy as the reverse carrier.
func (m *ServiceManager) RouteTLS(route contract.Tunnel) (*tls.Config, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.entries {
		if e.config.ID == route.ServiceID && e.config.Kind == "hub" && e.config.Endpoint == route.Endpoint && e.routeTLS != nil && e.status.Ready && liveService(e) {
			return e.routeTLS.Clone(), nil
		}
	}
	return nil, errors.New("managed reverse hub TLS is not ready")
}
func (e *serviceEntry) close() {
	if e.cancel != nil {
		e.cancel()
	}
	if e.listener != nil {
		e.listener.Close()
	}
	e.server.Close()
	if e.udp != nil {
		e.udp.Close()
	}
}
func (m *ServiceManager) failed(e *serviceEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e.status.Ready = false
	e.status.Error = "managed service failed"
	e.close()
}
func (m *ServiceManager) failedDatagram(id string, ds *tunnel.DatagramServer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.entries[id]; !m.closed && e != nil && e.udp == ds && e.status.Error == "" {
		e.status.Ready = false
		e.status.Error = "managed datagram service failed"
		e.close()
	}
}
func (m *ServiceManager) Statuses() []contract.ServiceStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []contract.ServiceStatus{}
	for _, e := range m.entries {
		out = append(out, e.status)
		if e.config.Kind == "hub" && e.status.Ready {
			seen := map[string]bool{}
			for _, g := range e.config.Grants {
				if !seen[g.Identity] {
					seen[g.Identity] = true
					out = append(out, contract.ServiceStatus{ID: g.Identity, Ready: e.server.ReverseReady(g.Identity)})
				}
			}
		}
	}
	return out
}
func (m *ServiceManager) RuleStatuses() []contract.RuleRuntimeStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []contract.RuleRuntimeStatus
	for _, e := range m.entries {
		out = append(out, e.server.PolicyStatuses()...)
	}
	return out
}
func (m *ServiceManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	if m.expiry != nil {
		m.expiry.Stop()
	}
	for _, e := range m.entries {
		e.close()
	}
	for _, s := range m.sockets {
		s.listener.Close()
	}
}

func relationshipCount(grants []contract.ServiceGrant) int {
	ids := map[string]bool{}
	for _, g := range grants {
		ids[g.Identity] = true
	}
	return len(ids)
}
