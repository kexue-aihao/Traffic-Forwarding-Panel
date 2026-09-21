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
	sessions map[string]*yamux.Session
	closed   bool
}

func (p *MuxPool) Close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	for _, s := range p.sessions {
		s.Close()
	}
	p.sessions = nil
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
	p := c.Pool
	// A structured key prevents credentials or endpoint boundaries from colliding.
	raw, _ := json.Marshal([]string{transport, endpoint, serverName, token})
	key := string(raw)
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, net.ErrClosed
	}
	if p.sessions == nil {
		p.sessions = map[string]*yamux.Session{}
	}
	for k, s := range p.sessions {
		if s.IsClosed() {
			delete(p.sessions, k)
		}
	}
	session := p.sessions[key]
	if session == nil {
		if len(p.sessions) >= 64 {
			p.mu.Unlock()
			return nil, errors.New("Mux carrier capacity reached")
		}
		plain := c
		plain.useMux = false
		plain.reverse = ""
		conn, e := plain.dial(ctx, transport, endpoint, serverName, token, "mux", "", nil, nil)
		if e != nil {
			p.mu.Unlock()
			return nil, e
		}
		session, e = yamux.Client(conn.Conn, muxConfig())
		if e != nil {
			conn.Close()
			p.mu.Unlock()
			return nil, e
		}
		p.sessions[key] = session
	}
	if session.NumStreams() >= 256 {
		p.mu.Unlock()
		return nil, errors.New("Mux stream capacity reached")
	}
	stream, e := session.OpenStream()
	p.mu.Unlock()
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
