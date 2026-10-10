package agent

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/netx"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/policy"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/tunnel"
)

type Runtime struct {
	Services        *ServiceManager
	mu              sync.Mutex
	Store           *Store
	Client          tunnel.Client
	Datagrams       *tunnel.DatagramPool
	listeners       map[string]*binding
	pools           map[string]*resourcePool
	version         int64
	closed          bool
	udpDialSlots    chan struct{}
	udpSessionSlots chan struct{}
	udpQueued       atomic.Int64
}
type binding struct {
	policyStatus map[string]contract.RuleRuntimeStatus
	changed      chan struct{}
	ctx          context.Context
	cancel       context.CancelFunc
	pool         *resourcePool
	mu           sync.Mutex
	rule         contract.Rule
	until        time.Time
	tcp          net.Listener
	udp          *net.UDPConn
	conns        map[net.Conn]struct{}
	sessions     map[string]*udpSession
	closed       bool
	runtime      *Runtime
	slots        chan struct{}
	routes       map[string]route
	backends     map[string]*backendState
	udpStats     udpCounters
}
type udpSession struct {
	ctx        context.Context
	pool       *resourcePool
	release    func()
	conn       net.Conn
	tunnel     *tunnel.Session
	peer       *net.UDPAddr
	rule       contract.Rule
	mu         sync.Mutex
	cancel     context.CancelFunc
	out        chan *udpPacket
	activity   atomic.Int64
	finishOnce sync.Once
}

func NewRuntime(store *Store, client tunnel.Client) *Runtime {
	if client.Pool == nil {
		client.Pool = &tunnel.MuxPool{}
	}
	return &Runtime{Services: &ServiceManager{BaseTLS: client.TLS}, Store: store, Client: client, Datagrams: &tunnel.DatagramPool{}, listeners: map[string]*binding{}, pools: map[string]*resourcePool{}, udpDialSlots: make(chan struct{}, 32), udpSessionSlots: make(chan struct{}, 4096)}
}
func (r *Runtime) Version() int64 { r.mu.Lock(); defer r.mu.Unlock(); return r.version }
func key(v contract.Rule) string  { return v.Network + "|" + v.Listen }
func validate(v contract.Rule) error {
	if v.EffectivePolicy != nil {
		h := v.EffectivePolicy.Hash
		if err := policy.Seal(v.EffectivePolicy); err != nil {
			return err
		}
		if h != v.EffectivePolicy.Hash {
			return errors.New("policy hash mismatch")
		}
	}
	if len(v.RouteCandidates) > 16 {
		return errors.New("at most 16 route candidates")
	}
	seenCandidates := map[string]bool{}
	for _, c := range v.RouteCandidates {
		if c.ID == "" || seenCandidates[c.ID] || c.Weight < 1 || c.Weight > 100 || c.ExitGroupID != v.ExitGroupID || c.BillingMultiplier != v.BillingMultiplier {
			return errors.New("invalid route candidate scope")
		}
		seenCandidates[c.ID] = true
		if (c.EffectivePolicy == nil) != (v.EffectivePolicy == nil) || c.EffectivePolicy != nil && c.EffectivePolicy.Hash != v.EffectivePolicy.Hash {
			return errors.New("candidate policy must match the compiled authorized group path")
		}
		next := v
		next.RouteCandidates = nil
		next.Backends = nil
		next.Target, next.Transport, next.Tunnel, next.EffectivePolicy = c.Target, c.Transport, c.Tunnel, c.EffectivePolicy
		if e := validate(next); e != nil {
			return e
		}
	}
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
	case "quic":
		if v.Network != "udp" || v.Tunnel == nil || v.Tunnel.Mux || v.Tunnel.Reverse != "" || len(v.Tunnel.Chain) > 0 || v.Tunnel.Obfuscation != nil {
			return errors.New("QUIC DATAGRAM requires single-exit UDP")
		}
		if e := contract.ValidateUDPExit(&contract.UDPExit{Endpoint: v.Tunnel.Endpoint, ServerName: v.Tunnel.ServerName, Token: v.Tunnel.Token}); e != nil {
			return e
		}
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
	case "tls", "tls_simple", "ws", "wss", "http", "secure-direct":
		if v.Tunnel == nil || v.Tunnel.Endpoint == "" || v.Tunnel.Token == "" {
			return errors.New("tunnel credentials missing")
		}
		if v.Transport == "secure-direct" && v.Network != "tcp" {
			return errors.New("secure-direct supports tcp only")
		}
		if v.Transport == "secure-direct" && (v.Tunnel.Mux || v.Tunnel.Reverse != "") {
			return errors.New("secure-direct does not support mux or reverse routing")
		}
		if v.Transport == "secure-direct" && v.Tunnel.Obfuscation == nil {
			return errors.New("secure-direct requires obfuscation")
		}
		if v.Transport == "secure-direct" {
			if e := tunnel.ValidateObfuscation(v.Tunnel.Obfuscation); e != nil {
				return fmt.Errorf("invalid secure-direct obfuscation: %w", e)
			}
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
	if v.UDP != nil && (v.Network != "udp" || v.UDP.MaxSessions < 0 || v.UDP.MaxSessions > 4096) {
		return errors.New("invalid UDP session limit")
	}
	if v.StandbyLease != nil {
		l := v.StandbyLease
		if !v.LeasePipeline || l.ID == "" || l.ID == v.Lease.ID || l.EntitlementID != v.Lease.EntitlementID || l.Bytes <= 0 || !time.Now().Before(l.ExpiresAt) || l.ExpiresAt.After(time.Now().Add(5*time.Minute+time.Second)) {
			return errors.New("invalid standby lease")
		}
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
	udpListeners := 0
	for _, rule := range c.Rules {
		if rule.Enabled && rule.Network == "udp" {
			udpListeners++
		}
	}
	if udpListeners > 128 {
		return errors.New("UDP listener capacity exceeded (128 per Agent)")
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
		b := &binding{changed: make(chan struct{}), rule: v, until: c.ValidUntil, conns: map[net.Conn]struct{}{}, sessions: map[string]*udpSession{}, runtime: r, slots: make(chan struct{}, 256), backends: map[string]*backendState{}}
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
	commitServices, abortServices, e := r.Services.Prepare(c.Services, c.ValidUntil)
	if e != nil {
		return fail(e)
	}
	committedServices := false
	defer func() {
		if !committedServices {
			abortServices()
		}
	}()
	if persist {
		if e := r.Store.SetConfig(c); e != nil {
			return fail(e)
		}
	}
	for _, v := range c.Rules {
		if v.Enabled && v.Network == "udp" {
			if e := r.Store.PrimeUDPCredits(v, c.ValidUntil); e != nil {
				return fail(e)
			}
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
		changed := b.ctx.Err() != nil || !sameForwardingRule(b.rule, rules[k]) || !sameRoutes(b.routes, routes[k])
		b.routes = routes[k]
		for name, v := range b.routes {
			v.pool = r.pools[limitOwner(v.rule)]
			b.routes[name] = v
		}
		b.rule = rules[k]
		statusIDs := map[string]bool{b.rule.ID: true}
		for _, route := range b.routes {
			statusIDs[route.rule.ID] = true
		}
		for id := range b.policyStatus {
			if !statusIDs[id] {
				delete(b.policyStatus, id)
			}
		}
		b.until = c.ValidUntil
		b.pool = r.pools[limitOwner(b.rule)]
		close(b.changed)
		b.changed = make(chan struct{})
		if changed {
			b.backends = map[string]*backendState{}
			b.cancel()
			b.ctx, b.cancel = context.WithCancel(context.Background())
			for conn := range b.conns {
				conn.Close()
			}
			for _, session := range b.sessions {
				session.stop()
			}
		}
		b.mu.Unlock()
	}
	commitServices()
	committedServices = true
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
	r.Services.Close()
	r.Client.Pool.Close()
	r.Datagrams.Close()
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
				s.stop()
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
		s.stop()
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
	if len(v.Backends) > 0 || len(v.RouteCandidates) > 0 || v.EffectivePolicy != nil && v.EffectivePolicy.Failover != nil {
		return b.dialBackends(ctx, v)
	}
	c, s, e := b.dialTarget(ctx, v)
	if e == nil {
		b.recordDial(v, "direct", c)
	}
	return c, s, e
}
func (b *binding) dialTarget(ctx context.Context, v contract.Rule) (net.Conn, *tunnel.Session, error) {
	if v.Transport == "quic" {
		c, e := b.runtime.Datagrams.DialPolicy(ctx, b.runtime.Client.TLS, v.Tunnel.Endpoint, v.Tunnel.ServerName, v.Tunnel.Token, v.Target, v.EffectivePolicy != nil && v.EffectivePolicy.PreferIPv6)
		return c, nil, e
	}
	if v.Transport == "direct" {
		c, e := netx.Dial(ctx, v.Network, v.Target, v.EffectivePolicy != nil && v.EffectivePolicy.PreferIPv6)
		return c, nil, e
	}
	if v.Transport == "direct-tls" {
		return b.dialTargetTLS(ctx, v)
	}
	tc := b.runtime.Client
	if v.Tunnel.ServiceID != "" {
		var err error
		tc.TLS, err = b.runtime.Services.RouteTLS(*v.Tunnel)
		if err != nil {
			return nil, nil, err
		}
	}
	tc.RuleID = v.ID
	if p, ok := ctx.Value(inspectionContextKey{}).([]byte); ok {
		tc.InspectionPrefix = p
	}
	tc.PreferIPv6 = v.EffectivePolicy != nil && v.EffectivePolicy.PreferIPv6
	s, e := tc.DialRoute(ctx, v.Transport, v.Network, v.Target, *v.Tunnel)
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
	raw, e := netx.Dial(ctx, "tcp", v.Target, v.EffectivePolicy != nil && v.EffectivePolicy.PreferIPv6)
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
	v, _, ctx, pool := b.snapshot()
	client, err := receiveProxy(c, v.ProxyProtocol)
	if err != nil {
		return
	}
	var inspected policy.Inspection
	layers := []contract.InboundPolicy{}
	if v.EffectivePolicy != nil {
		layers = v.EffectivePolicy.InboundLayers
	}
	if len(v.BlockedProtocols) > 0 {
		layers = append(layers, contract.InboundPolicy{GroupID: "rule", BlockedApps: v.BlockedProtocols})
	}
	if v.SharedTLS != nil || policy.NeedsInspect(layers) {
		inspected, err = policy.Inspect(client)
		if err != nil {
			return
		}
		client = inspected.Conn
		if v.SharedTLS != nil {
			name, e := policy.Host(inspected.Host)
			if e != nil || inspected.Kind != "tls" {
				return
			}
			b.mu.Lock()
			selected, ok := b.routes[name]
			ctx = b.ctx
			b.mu.Unlock()
			if !ok {
				return
			}
			v, pool = selected.rule, selected.pool
			layers = nil
			if v.EffectivePolicy != nil {
				layers = v.EffectivePolicy.InboundLayers
			}
			if len(v.BlockedProtocols) > 0 {
				layers = append(layers, contract.InboundPolicy{GroupID: "rule", BlockedApps: v.BlockedProtocols})
			}
		}
		if err = inspected.Check(layers); err != nil {
			b.policyRejected(v.ID)
			return
		}
		client = policy.FilterWithRejection(client, inspected, layers, func() { b.policyRejected(v.ID) })
	}
	release, ok := pool.acquire(client.RemoteAddr())
	if !ok {
		return
	}
	defer release()
	if e := b.awaitAvailable(ctx, v.ID); e != nil {
		return
	}
	if len(inspected.Prefix) > 0 {
		ctx = context.WithValue(ctx, inspectionContextKey{}, inspected.Prefix)
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
	tunnel.Relay(&cancelConn{Conn: client, cancel: cancel}, &cancelConn{Conn: target, cancel: cancel}, 2*time.Minute, func(up bool, n int) error { return b.charge(flowCtx, pool, v, up, n) })
}

type inspectionContextKey struct{}

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
			for _, method := range []string{"GET ", "POST ", "HEAD ", "PUT ", "DELETE ", "OPTIONS ", "CONNECT ", "TRACE ", "PATCH ", "PRI "} {
				if strings.HasPrefix(string(p), method) {
					return true
				}
			}
		}
	}
	return false
}
