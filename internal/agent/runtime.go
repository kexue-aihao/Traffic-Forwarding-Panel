package agent

import (
	"bufio"
	"context"
	"crypto/tls"
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
	pools     map[string]*resourcePool
	version   int64
	closed    bool
}
type binding struct {
	ctx      context.Context
	cancel   context.CancelFunc
	pool     *resourcePool
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
	routes   map[string]route
	backends map[string]*backendState
}
type udpSession struct {
	ctx     context.Context
	pool    *resourcePool
	release func()
	conn    net.Conn
	tunnel  *tunnel.Session
	peer    *net.UDPAddr
	rule    contract.Rule
	until   time.Time
}

func NewRuntime(store *Store, client tunnel.Client) *Runtime {
	if client.Pool == nil {
		client.Pool = &tunnel.MuxPool{}
	}
	return &Runtime{Store: store, Client: client, listeners: map[string]*binding{}, pools: map[string]*resourcePool{}}
}
func (r *Runtime) Version() int64 { r.mu.Lock(); defer r.mu.Unlock(); return r.version }
func key(v contract.Rule) string  { return v.Network + "|" + v.Listen }
func validate(v contract.Rule) error {
	if err := v.ValidateAdvanced(); err != nil {
		return err
	}
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
	case "direct-tls":
		// 没有出口，加密在对端（目标自己）终止。只允许带一个 server_name
		// 作为校验名 —— endpoint 与 token 是隧道才有的东西。
		if v.Tunnel != nil && (v.Tunnel.Endpoint != "" || v.Tunnel.Token != "") {
			return errors.New("direct-tls transport cannot contain a tunnel")
		}
		// TLS 只承载 TCP；UDP 要走 TLS 得用 DTLS，那是另一套协议。
		if v.Network != "tcp" {
			return errors.New("direct-tls supports tcp only")
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
	if err := v.Lease.Limits.Validate(); err != nil {
		return err
	}
	if v.Lease.Limits != (contract.ResourceLimits{}) && v.UserID == "" {
		return errors.New("limited rule requires account identity")
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
	policies := map[string]contract.ResourceLimits{}
	staged := []*binding{}
	routes := map[string]map[string]route{}
	fail := func(e error) error {
		for _, b := range staged {
			b.close()
		}
		return e
	}
	if err := validateSharedRules(c.Rules); err != nil {
		return err
	}
	for _, v := range c.Rules {
		if !v.Enabled {
			continue
		}
		if e := validate(v); e != nil {
			return fail(fmt.Errorf("rule %s: %w", v.ID, e))
		}
		owner := limitOwner(v)
		if prior, exists := policies[owner]; exists && prior != v.Lease.Limits {
			return fail(errors.New("inconsistent account limits"))
		}
		policies[owner] = v.Lease.Limits
		k := key(v)
		if ids[v.ID] {
			return fail(errors.New("duplicate listener or rule"))
		}
		ids[v.ID] = true
		if v.SharedTLS != nil {
			if routes[k] == nil {
				routes[k] = map[string]route{}
			}
			routes[k][v.SharedTLS.ServerName] = route{rule: v, until: c.ValidUntil}
			if v.SharedTLS.ParentID != "" {
				continue
			}
		}
		if _, exists := next[k]; exists {
			return fail(errors.New("duplicate listener"))
		}
		rules[k] = v
		if old := r.listeners[k]; old != nil {
			next[k] = old
			continue
		}
		b := &binding{rule: v, until: c.ValidUntil, conns: map[net.Conn]struct{}{}, sessions: map[string]*udpSession{}, runtime: r, slots: make(chan struct{}, 256), backends: map[string]*backendState{}}
		b.ctx, b.cancel = context.WithCancel(context.Background())
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
			b.close()
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
	for owner, limits := range policies {
		pool := r.pools[owner]
		if pool == nil {
			pool = &resourcePool{}
			r.pools[owner] = pool
		}
		pool.configure(limits)
	}
	for owner, pool := range r.pools {
		pool.mu.Lock()
		idle := pool.connections == 0
		pool.mu.Unlock()
		if _, exists := policies[owner]; !exists && idle {
			delete(r.pools, owner)
		}
	}
	for k, b := range r.listeners {
		if next[k] != b {
			b.close()
		}
	}
	for k, b := range next {
		b.mu.Lock()
		changed := !reflect.DeepEqual(b.rule, rules[k]) || !sameRoutes(b.routes, routes[k])
		b.routes = routes[k]
		for name, v := range b.routes {
			v.pool = r.pools[limitOwner(v.rule)]
			b.routes[name] = v
		}
		b.rule = rules[k]
		b.until = c.ValidUntil
		b.pool = r.pools[limitOwner(b.rule)]
		if changed {
			b.cancel()
			b.ctx, b.cancel = context.WithCancel(context.Background())
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
		go b.healthLoop()
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
	r.Client.Pool.Close()
}
func (r *Runtime) StopLease(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, b := range r.listeners {
		b.mu.Lock()
		matches := b.rule.Lease != nil && b.rule.Lease.ID == id
		for _, route := range b.routes {
			matches = matches || route.rule.Lease != nil && route.rule.Lease.ID == id
		}
		if matches {
			b.cancel()
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
	b.cancel()
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
func (b *binding) snapshot() (contract.Rule, time.Time, context.Context, *resourcePool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.rule, b.until, b.ctx, b.pool
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
func (b *binding) dial(ctx context.Context, v contract.Rule) (net.Conn, *tunnel.Session, error) {
	if len(v.Backends) > 0 {
		return b.dialBackends(ctx, v)
	}
	return b.dialTarget(ctx, v)
}
func (b *binding) dialTarget(ctx context.Context, v contract.Rule) (net.Conn, *tunnel.Session, error) {
	if v.Transport == "direct" {
		c, e := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, v.Network, v.Target)
		return c, nil, e
	}
	if v.Transport == "direct-tls" {
		return b.dialTargetTLS(ctx, v)
	}
	s, e := b.runtime.Client.DialRoute(ctx, v.Transport, v.Network, v.Target, *v.Tunnel)
	if e != nil {
		return nil, nil, e
	}
	return s, s, nil
}

// dialTargetTLS 把「入口到目标」这一段包进 TLS。
//
// 与隧道不同，这里没有出口参与：加密直接在对端终止，所以目标必须自己会说
// TLS。信任库沿用 -ca 装进来的那一套（系统根 + 私有 CA），和目标证书的校验
// 名默认取 target 的主机部分，规则里写了 tunnel.server_name 就用它 —— 目标
// 是纯 IP、证书签的却是域名时需要后者。
//
// 计费口径不变：经过这条连接的仍是业务有效载荷，TLS 记录头不额外计入。
func (b *binding) dialTargetTLS(ctx context.Context, v contract.Rule) (net.Conn, *tunnel.Session, error) {
	raw, e := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", v.Target)
	if e != nil {
		return nil, nil, e
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS13}
	if base := b.runtime.Client.TLS; base != nil {
		cfg = base.Clone()
		cfg.MinVersion = tls.VersionTLS13
	}
	host, _, e := net.SplitHostPort(v.Target)
	if e != nil {
		raw.Close()
		return nil, nil, e
	}
	cfg.ServerName = host
	if v.Tunnel != nil && v.Tunnel.ServerName != "" {
		cfg.ServerName = v.Tunnel.ServerName
	}
	c := tls.Client(raw, cfg)
	if e := c.HandshakeContext(ctx); e != nil {
		raw.Close()
		return nil, nil, e
	}
	return c, nil, nil
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
	v, until, ctx, pool := b.snapshot()
	client, err := receiveProxy(c, v.ProxyProtocol)
	if err != nil {
		return
	}
	if v.SharedTLS != nil {
		name, replay, err := readServerName(c)
		if err != nil {
			return
		}
		b.mu.Lock()
		selected, ok := b.routes[name]
		ctx = b.ctx
		b.mu.Unlock()
		if !ok {
			return
		}
		v, until, pool = selected.rule, selected.until, selected.pool
		client = replay
	}
	if e := b.runtime.Store.Available(v, until); e != nil {
		return
	}
	release, ok := pool.acquire(client.RemoteAddr())
	if !ok {
		return
	}
	defer release()
	if len(v.BlockedProtocols) > 0 {
		c.SetReadDeadline(time.Now().Add(10 * time.Second))
		reader := bufio.NewReader(client)
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
		client = &readConn{Conn: client, reader: reader}
	}
	target, _, e := b.dial(ctx, v)
	if e != nil {
		return
	}
	defer target.Close()
	if !b.track(target) {
		return
	}
	defer b.untrack(target)
	if e := sendProxy(target, client, v.ProxyProtocol); e != nil {
		return
	}
	flowCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	tunnel.Relay(&cancelConn{Conn: client, cancel: cancel}, &cancelConn{Conn: target, cancel: cancel}, 2*time.Minute, func(up bool, n int) error { return b.charge(flowCtx, pool, v, until, up, n) })
}

type cancelConn struct {
	net.Conn
	cancel context.CancelFunc
}

func (c *cancelConn) Close() error { c.cancel(); return c.Conn.Close() }
func (c *cancelConn) CloseWrite() error {
	if conn, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return conn.CloseWrite()
	}
	return c.Close()
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
		v, until, ctx, pool := b.snapshot()
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
			release, ok := pool.acquire(peer)
			if !ok {
				continue
			}
			conn, t, e := b.dial(ctx, v)
			if e != nil {
				release()
				continue
			}
			s = &udpSession{ctx: ctx, pool: pool, release: release, conn: conn, tunnel: t, peer: peer, rule: v, until: until}
			b.mu.Lock()
			if b.closed {
				b.mu.Unlock()
				conn.Close()
				release()
				return
			}
			b.sessions[k] = s
			b.mu.Unlock()
			go b.readUDP(k, s)
		}
		if err := b.charge(s.ctx, s.pool, s.rule, s.until, true, n); err != nil {
			if errors.Is(err, errRateDrop) {
				continue
			}
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
	defer s.release()
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
		if e = b.charge(s.ctx, s.pool, s.rule, s.until, false, len(p)); e != nil {
			if errors.Is(e, errRateDrop) {
				continue
			}
			return
		}
		if _, e = b.udp.WriteToUDP(p, s.peer); e != nil {
			return
		}
	}
}
