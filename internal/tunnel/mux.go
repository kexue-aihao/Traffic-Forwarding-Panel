package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

func muxConfig() *yamux.Config {
	c := yamux.DefaultConfig()
	c.AcceptBacklog = 256
	c.MaxStreamWindowSize = 256 * 1024
	c.ConnectionWriteTimeout = 10 * time.Second
	c.StreamOpenTimeout = 10 * time.Second
	c.StreamCloseTimeout = 10 * time.Second
	c.KeepAliveInterval = 15 * time.Second
	c.LogOutput = io.Discard
	return c
}

type MuxPool struct {
	mu       sync.Mutex
	sessions map[string]*muxEntry
	done     chan struct{}
	closed   bool
}

// One in-flight carrier dial per key. Entries and opening reservations are
// protected by the pool mutex; ready publishes the completed session/error.
type muxEntry struct {
	ready   chan struct{}
	cancel  context.CancelFunc
	session *yamux.Session
	err     error
	opening int
}

func (p *MuxPool) Close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	if p.done != nil {
		close(p.done)
	}
	entries := p.sessions
	p.sessions = nil
	// Cancel pending dials before releasing the lock. Their completion cannot
	// repopulate a closed pool. Closing live carriers may wait for I/O.
	var sessions []*yamux.Session
	for _, entry := range entries {
		entry.cancel()
		if entry.session != nil {
			sessions = append(sessions, entry.session)
		}
	}
	p.mu.Unlock()
	for _, session := range sessions {
		session.Close()
	}
}

func (c Client) DialRoute(ctx context.Context, transport, network, target string, t contract.Tunnel) (*Session, error) {
	c.useMux = t.Mux
	c.reverse = t.Reverse
	return c.DialChain(ctx, transport, t.Endpoint, t.ServerName, t.Token, network, target, t.Chain)
}

func (c Client) open(conn net.Conn, token, network, target string, chain []contract.TunnelHop, visited []string) error {
	version := 1
	if len(chain) > 0 || len(visited) > 0 {
		version = 2
	}
	p, e := json.Marshal(openRequest{Version: version, Token: token, Network: network, Target: target, Chain: chain, Visited: visited, Reverse: c.reverse})
	if e != nil {
		return e
	}
	if e = writeFrame(conn, openFrame, p); e != nil {
		return e
	}
	k, p, e := readFrame(conn)
	if e != nil {
		return e
	}
	if k != readyFrame || string(p) != "ok" {
		return errors.New("tunnel authorization or target rejected")
	}
	return nil
}

func (c Client) dialMux(ctx context.Context, transport, endpoint, serverName, token, network, target string, chain []contract.TunnelHop, visited []string) (*Session, error) {
	if c.Pool == nil {
		return nil, errors.New("Mux pool required")
	}
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// A structured key prevents credentials or endpoint boundaries from colliding.
	raw, _ := json.Marshal([]string{transport, endpoint, serverName, token})
	key := string(raw)
	entry, e := c.muxCarrier(ctx, timeout, key, transport, endpoint, serverName, token)
	if e != nil {
		return nil, e
	}
	p := c.Pool
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, net.ErrClosed
	}
	if entry.session.NumStreams()+entry.opening >= 256 {
		p.mu.Unlock()
		return nil, errors.New("Mux stream capacity reached")
	}
	entry.opening++
	p.mu.Unlock()
	stream, e := p.openStream(ctx, entry)
	if e != nil {
		return nil, e
	}
	stop := context.AfterFunc(ctx, func() { stream.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	stream.SetDeadline(deadline)
	if e = c.open(stream, token, network, target, chain, visited); e != nil {
		stream.Close()
		return nil, e
	}
	if !stop() || ctx.Err() != nil {
		stream.Close()
		return nil, ctx.Err()
	}
	stream.SetDeadline(time.Time{})
	return &Session{Conn: stream}, nil
}

func (c Client) muxCarrier(ctx context.Context, timeout time.Duration, key, transport, endpoint, serverName, token string) (*muxEntry, error) {
	p := c.Pool
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, net.ErrClosed
	}
	if p.sessions == nil {
		p.sessions = map[string]*muxEntry{}
		p.done = make(chan struct{})
	}
	for k, entry := range p.sessions {
		if entry.session != nil && entry.session.IsClosed() {
			delete(p.sessions, k)
		}
	}
	entry := p.sessions[key]
	if entry == nil {
		// Pending dials count toward the carrier limit as well.
		if len(p.sessions) >= 64 {
			p.mu.Unlock()
			return nil, errors.New("Mux carrier capacity reached")
		}
		// Each waiter has its own deadline. Cancelling one must not abort a
		// carrier needed by the other waiters. Pool.Close cancels the dial.
		dialCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
		entry = &muxEntry{ready: make(chan struct{}), cancel: cancel}
		p.sessions[key] = entry
		go c.connectMux(dialCtx, key, entry, transport, endpoint, serverName, token)
	}
	done := p.done
	p.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-done:
		return nil, net.ErrClosed
	case <-entry.ready:
		return entry, entry.err
	}
}

func (c Client) connectMux(ctx context.Context, key string, entry *muxEntry, transport, endpoint, serverName, token string) {
	defer entry.cancel()
	plain := c
	plain.useMux = false
	plain.reverse = ""
	conn, err := plain.dial(ctx, transport, endpoint, serverName, token, "mux", "", nil, nil)
	var session *yamux.Session
	if err == nil {
		session, err = yamux.Client(conn.Conn, muxConfig())
		if err != nil {
			conn.Close()
		}
	}
	p := c.Pool
	p.mu.Lock()
	if p.closed {
		err = net.ErrClosed
	}
	entry.err = err
	if err == nil {
		entry.session = session
	} else if p.sessions[key] == entry {
		delete(p.sessions, key)
	}
	close(entry.ready)
	p.mu.Unlock()
	if err != nil && session != nil {
		session.Close()
	}
}

// yamux.OpenStream has no context API. Bound outstanding opens with opening,
// let the caller cancel promptly, and close any stream delivered too late.
// yamux's connection write/open timeouts bound the worker's lifetime.
func (p *MuxPool) openStream(ctx context.Context, entry *muxEntry) (*yamux.Stream, error) {
	type result struct {
		stream *yamux.Stream
		err    error
	}
	ready := make(chan result)
	go func() {
		stream, err := entry.session.OpenStream()
		p.mu.Lock()
		entry.opening--
		p.mu.Unlock()
		select {
		case ready <- result{stream, err}:
		case <-ctx.Done():
			if stream != nil {
				stream.Close()
			}
		}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-ready:
		return result.stream, result.err
	}
}

func (s *Server) serveMultiplex(conn net.Conn, req openRequest) {
	if len(req.Chain) > 0 || len(req.Visited) > 0 || req.Reverse != "" {
		return
	}
	if req.Network == "reverse" && (!validNodeID(req.Target) || !s.Allowed["reverse|"+req.Target]) {
		return
	}
	if req.Network == "mux" && req.Target != "" {
		return
	}
	if writeFrame(conn, readyFrame, []byte("ok")) != nil {
		return
	}
	conn.SetDeadline(time.Time{})
	if req.Network == "mux" {
		session, e := yamux.Server(conn, muxConfig())
		if e != nil {
			return
		}
		defer session.Close()
		s.serveStreams(session)
		return
	}
	session, e := yamux.Client(conn, muxConfig())
	if e != nil {
		return
	}
	defer session.Close()
	s.mu.Lock()
	if s.reverse == nil {
		s.reverse = map[string]*yamux.Session{}
	}
	if prior := s.reverse[req.Target]; prior != nil && !prior.IsClosed() {
		s.mu.Unlock()
		return
	}
	s.reverse[req.Target] = session
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.reverse[req.Target] == session {
			delete(s.reverse, req.Target)
		}
		s.mu.Unlock()
	}()
	<-session.CloseChan()
}

func (s *Server) serveStreams(session *yamux.Session) {
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, e := session.AcceptStream()
		if e != nil {
			return
		}
		select {
		case s.slots <- struct{}{}:
			wg.Add(1)
			go func() { defer wg.Done(); defer func() { <-s.slots }(); defer conn.Close(); s.serveRequest(conn, false) }()
		default:
			conn.Close()
		}
	}
}

func (s *Server) openReverse(req openRequest) (*Session, error) {
	s.mu.Lock()
	session := s.reverse[req.Reverse]
	s.mu.Unlock()
	if session == nil || session.IsClosed() || session.NumStreams() >= 256 {
		return nil, errors.New("reverse exit unavailable")
	}
	conn, e := session.OpenStream()
	if e != nil {
		return nil, e
	}
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	c := s.Client
	c.reverse = ""
	if e = c.open(conn, req.Token, req.Network, req.Target, nil, nil); e != nil {
		conn.Close()
		return nil, e
	}
	conn.SetDeadline(time.Time{})
	return &Session{Conn: conn}, nil
}

// RunReverse keeps an outbound authenticated carrier open and reconnects after
// disconnect. The local exit still checks each stream's token and target.
func (s *Server) RunReverse(ctx context.Context, c Client, transport, endpoint, serverName, identity string) error {
	if !validNodeID(identity) || len(s.Token) < 16 || len(s.Allowed) == 0 {
		return errors.New("reverse identity, token and allowlist required")
	}
	s.mu.Lock()
	s.ctx = ctx
	s.slots = make(chan struct{}, 256)
	s.mu.Unlock()
	for ctx.Err() == nil {
		conn, e := c.Dial(ctx, transport, endpoint, serverName, s.Token, "reverse", identity)
		if e == nil {
			session, err := yamux.Server(conn.Conn, muxConfig())
			if err == nil {
				stop := context.AfterFunc(ctx, func() { session.Close() })
				s.serveStreams(session)
				stop()
				session.Close()
			}
			conn.Close()
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	return nil
}
