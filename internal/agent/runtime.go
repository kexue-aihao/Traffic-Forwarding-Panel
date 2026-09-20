package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/tunnel"
)

type Runtime struct {
	mu        sync.Mutex
	Store     *Store
	Client    tunnel.Client
	listeners map[string]*binding
	version   int64
	closed    bool
}
type binding struct {
	mu       sync.Mutex
	rule     contract.Rule
	until    time.Time
	tcp      net.Listener
	udp      *net.UDPConn
	conns    map[net.Conn]struct{}
	sessions map[string]*udpSession
	closed   bool
	runtime  *Runtime
	slots    chan struct{}
}
type udpSession struct {
	conn   net.Conn
	tunnel *tunnel.Session
	peer   *net.UDPAddr
	rule   contract.Rule
	until  time.Time
}

func NewRuntime(store *Store, client tunnel.Client) *Runtime {
	return &Runtime{Store: store, Client: client, listeners: map[string]*binding{}}
}
func (r *Runtime) Version() int64 { r.mu.Lock(); defer r.mu.Unlock(); return r.version }
func key(v contract.Rule) string  { return v.Network + "|" + v.Listen }
func validate(v contract.Rule) error {
	if v.ID == "" {
		return errors.New("rule ID missing")
	}
	if v.Network != "tcp" && v.Network != "udp" {
		return errors.New("network unsupported")
	}
	if _, _, e := net.SplitHostPort(v.Listen); e != nil {
		return e
	}
	if _, _, e := net.SplitHostPort(v.Target); e != nil {
		return e
	}
	switch v.Transport {
	case "direct":
		if v.Tunnel != nil {
			return errors.New("direct transport cannot contain a tunnel")
		}
	case "tls", "ws", "wss", "http":
		if v.Tunnel == nil || v.Tunnel.Endpoint == "" || v.Tunnel.Token == "" {
			return errors.New("tunnel credentials missing")
		}
		if e := tunnel.ValidateChain(contract.TunnelHop{Transport: v.Transport, Endpoint: v.Tunnel.Endpoint, ServerName: v.Tunnel.ServerName, Token: v.Tunnel.Token}, v.Tunnel.Chain); e != nil {
			return e
		}
	default:
		return errors.New("transport unsupported")
	}
	for _, p := range v.BlockedProtocols {
		if p != "http" && p != "socks" {
			return fmt.Errorf("unsupported protocol detector %q", p)
		}
	}
	if v.Lease == nil || v.Lease.ID == "" || v.Lease.Bytes <= 0 {
		return errors.New("finite lease required")
	}
	return nil
}

// Apply stages all new listeners before committing any live configuration.
func (r *Runtime) Apply(c contract.Config, persist bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("runtime closed")
	}
	if c.ContractVersion != contract.Version || c.NodeID != r.Store.Identity().NodeID {
		return errors.New("configuration identity/version mismatch")
	}
	if c.Version < r.version {
		return errors.New("stale configuration")
	}
	if !time.Now().Before(c.ValidUntil) || c.ValidUntil.After(time.Now().Add(24*time.Hour)) {
		return errors.New("configuration validity must be within 24 hours")
	}
	next := map[string]*binding{}
	rules := map[string]contract.Rule{}
	ids := map[string]bool{}
	staged := []*binding{}
	fail := func(e error) error {
		for _, b := range staged {
			b.close()
		}
		return e
	}
	for _, v := range c.Rules {
		if !v.Enabled {
			continue
		}
		if e := validate(v); e != nil {
			return fail(fmt.Errorf("rule %s: %w", v.ID, e))
		}
		k := key(v)
		if _, exists := next[k]; exists || ids[v.ID] {
			return fail(errors.New("duplicate listener or rule"))
		}
		ids[v.ID] = true
		rules[k] = v
		if old := r.listeners[k]; old != nil {
			next[k] = old
			continue
		}
		b := &binding{rule: v, until: c.ValidUntil, conns: map[net.Conn]struct{}{}, sessions: map[string]*udpSession{}, runtime: r, slots: make(chan struct{}, 256)}
		var e error
		if v.Network == "tcp" {
			b.tcp, e = net.Listen("tcp", v.Listen)
		} else {
			var addr *net.UDPAddr
			addr, e = net.ResolveUDPAddr("udp", v.Listen)
			if e == nil {
				b.udp, e = net.ListenUDP("udp", addr)
			}
		}
		if e != nil {
			return fail(e)
		}
		next[k] = b
		staged = append(staged, b)
	}
	if persist {
		if e := r.Store.SetConfig(c); e != nil {
			return fail(e)
		}
	}
	for k, b := range r.listeners {
		if next[k] != b {
			b.close()
		}
	}
	for k, b := range next {
		b.mu.Lock()
		changed := !reflect.DeepEqual(b.rule, rules[k])
		b.rule = rules[k]
		b.until = c.ValidUntil
		if changed {
			for conn := range b.conns {
				conn.Close()
			}
			for _, session := range b.sessions {
				session.conn.Close()
			}
		}
		b.mu.Unlock()
	}
	r.listeners = next
	r.version = c.Version
	for _, b := range staged {
		if b.tcp != nil {
			go b.serveTCP()
		} else {
			go b.serveUDP()
		}
	}
	return nil
}
func (r *Runtime) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	for _, b := range r.listeners {
		b.close()
	}
}
func (r *Runtime) StopLease(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, b := range r.listeners {
		b.mu.Lock()
		if b.rule.Lease != nil && b.rule.Lease.ID == id {
			for c := range b.conns {
				c.Close()
			}
			for _, s := range b.sessions {
				s.conn.Close()
			}
		}
		b.mu.Unlock()
	}
}
func (b *binding) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	if b.tcp != nil {
		b.tcp.Close()
	}
	if b.udp != nil {
		b.udp.Close()
	}
	for c := range b.conns {
		c.Close()
	}
	for _, s := range b.sessions {
		s.conn.Close()
	}
}
func (b *binding) snapshot() (contract.Rule, time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.rule, b.until
}
func (b *binding) track(c net.Conn) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		c.Close()
		return false
	}
	b.conns[c] = struct{}{}
	return true
}
func (b *binding) untrack(c net.Conn) { b.mu.Lock(); delete(b.conns, c); b.mu.Unlock(); c.Close() }
func (b *binding) dial(v contract.Rule) (net.Conn, *tunnel.Session, error) {
	if v.Transport == "direct" {
		c, e := net.DialTimeout(v.Network, v.Target, 10*time.Second)
		return c, nil, e
	}
	s, e := b.runtime.Client.DialChain(context.Background(), v.Transport, v.Tunnel.Endpoint, v.Tunnel.ServerName, v.Tunnel.Token, v.Network, v.Target, v.Tunnel.Chain)
	if e != nil {
		return nil, nil, e
	}
	return s, s, nil
}
func (b *binding) serveTCP() {
	for {
		c, e := b.tcp.Accept()
		if e != nil {
			return
		}
		select {
		case b.slots <- struct{}{}:
			go func() { defer func() { <-b.slots }(); b.handleTCP(c) }()
		default:
			c.Close()
		}
	}
}
func (b *binding) handleTCP(c net.Conn) {
	if !b.track(c) {
		return
	}
	defer b.untrack(c)
	v, until := b.snapshot()
	if e := b.runtime.Store.Available(v, until); e != nil {
		return
	}
	var client net.Conn = c
	if len(v.BlockedProtocols) > 0 {
		c.SetReadDeadline(time.Now().Add(10 * time.Second))
		reader := bufio.NewReader(c)
		first, e := reader.Peek(1)
		if e != nil {
			return
		}
		n := 1
		if strings.ContainsRune("GHPDOC T", rune(first[0])) {
			n = 8
		}
		prefix, e := reader.Peek(n)
		if e != nil && len(prefix) == 0 {
			return
		}
		if blocked(prefix, v.BlockedProtocols) {
			return
		}
		client = &readConn{Conn: c, reader: reader}
	}
	target, _, e := b.dial(v)
	if e != nil {
		return
	}
	defer target.Close()
	if !b.track(target) {
		return
	}
	defer b.untrack(target)
	tunnel.Relay(client, target, 2*time.Minute, func(up bool, n int) error { return b.runtime.Store.Charge(v, until, up, n) })
}

type readConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *readConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
func (c *readConn) CloseWrite() error {
	if v, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return v.CloseWrite()
	}
	return c.Conn.Close()
}
func blocked(p []byte, policies []string) bool {
	for _, policy := range policies {
		if policy == "socks" && len(p) > 0 && (p[0] == 4 || p[0] == 5) {
			return true
		}
		if policy == "http" {
			for _, method := range []string{"GET ", "POST ", "HEAD ", "PUT ", "DELETE ", "OPTIONS ", "CONNECT ", "TRACE ", "PATCH "} {
				if strings.HasPrefix(string(p), method) {
					return true
				}
			}
		}
	}
	return false
}
func (b *binding) serveUDP() {
	buf := make([]byte, 65535)
	for {
		n, peer, e := b.udp.ReadFromUDP(buf)
		if e != nil {
			return
		}
		v, until := b.snapshot()
		if blocked(buf[:n], v.BlockedProtocols) || b.runtime.Store.Available(v, until) != nil {
			continue
		}
		k := peer.String()
		b.mu.Lock()
		s := b.sessions[k]
		b.mu.Unlock()
		if s == nil {
			b.mu.Lock()
			full := len(b.sessions) >= 256
			b.mu.Unlock()
			if full {
				continue
			}
			conn, t, e := b.dial(v)
			if e != nil {
				continue
			}
			s = &udpSession{conn: conn, tunnel: t, peer: peer, rule: v, until: until}
			b.mu.Lock()
			if b.closed {
				b.mu.Unlock()
				conn.Close()
				return
			}
			b.sessions[k] = s
			b.mu.Unlock()
			go b.readUDP(k, s)
		}
		if b.runtime.Store.Charge(s.rule, s.until, true, n) != nil {
			s.conn.Close()
			continue
		}
		s.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if s.tunnel != nil {
			e = s.tunnel.WritePacket(buf[:n])
		} else {
			_, e = s.conn.Write(buf[:n])
		}
		if e != nil {
			s.conn.Close()
		}
	}
}
func (b *binding) readUDP(k string, s *udpSession) {
	defer func() {
		s.conn.Close()
		b.mu.Lock()
		if b.sessions[k] == s {
			delete(b.sessions, k)
		}
		b.mu.Unlock()
	}()
	buf := make([]byte, 65535)
	for {
		s.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		var p []byte
		var e error
		if s.tunnel != nil {
			p, e = s.tunnel.ReadPacket()
		} else {
			var n int
			n, e = s.conn.Read(buf)
			p = buf[:n]
		}
		if e != nil {
			return
		}
		if e = b.runtime.Store.Charge(s.rule, s.until, false, len(p)); e != nil {
			return
		}
		if _, e = b.udp.WriteToUDP(p, s.peer); e != nil {
			return
		}
	}
}
