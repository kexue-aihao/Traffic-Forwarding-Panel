// Package tunnel provides the v1/v2 authenticated forwarding data plane.
// ws and http use TLS inside their carrier. wss uses TLS outside WebSocket.
package tunnel

import (
	"bufio"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

const maxFrame = 65535
const (
	dataFrame  byte = 1
	endFrame   byte = 2
	openFrame  byte = 3
	readyFrame byte = 4
)

// Session preserves TCP half close and UDP packet boundaries over every carrier.
type Session struct {
	net.Conn
	writeMu sync.Mutex
	pending []byte
	ended   bool
}

func writeFrame(w io.Writer, kind byte, p []byte) error {
	if len(p) > maxFrame {
		return errors.New("frame too large")
	}
	h := make([]byte, 5)
	h[0] = kind
	binary.BigEndian.PutUint32(h[1:], uint32(len(p)))
	if err := writeAll(w, h); err != nil {
		return err
	}
	return writeAll(w, p)
}
func writeAll(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, e := w.Write(p)
		if e != nil {
			return e
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}
func readFrame(r io.Reader) (byte, []byte, error) {
	h := make([]byte, 5)
	if _, e := io.ReadFull(r, h); e != nil {
		return 0, nil, e
	}
	n := binary.BigEndian.Uint32(h[1:])
	if n > maxFrame {
		return 0, nil, errors.New("frame too large")
	}
	p := make([]byte, n)
	_, e := io.ReadFull(r, p)
	return h[0], p, e
}
func (s *Session) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(s.pending) == 0 {
		if s.ended {
			return 0, io.EOF
		}
		k, b, e := readFrame(s.Conn)
		if e != nil {
			return 0, e
		}
		if k == endFrame {
			if len(b) != 0 {
				return 0, errors.New("invalid close frame")
			}
			s.ended = true
			return 0, io.EOF
		}
		if k != dataFrame {
			return 0, errors.New("invalid data frame")
		}
		s.pending = b
	}
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}
func (s *Session) Write(p []byte) (int, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	written := 0
	for len(p) > 0 {
		n := len(p)
		if n > maxFrame {
			n = maxFrame
		}
		if e := writeFrame(s.Conn, dataFrame, p[:n]); e != nil {
			return written, e
		}
		written += n
		p = p[n:]
	}
	return written, nil
}
func (s *Session) CloseWrite() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return writeFrame(s.Conn, endFrame, nil)
}
func (s *Session) WritePacket(p []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return writeFrame(s.Conn, dataFrame, p)
}
func (s *Session) ReadPacket() ([]byte, error) {
	k, p, e := readFrame(s.Conn)
	if e != nil {
		return nil, e
	}
	if k == endFrame {
		return nil, io.EOF
	}
	if k != dataFrame {
		return nil, errors.New("invalid packet frame")
	}
	return p, nil
}

type openRequest struct {
	Version int                  `json:"version"`
	Token   string               `json:"token"`
	Network string               `json:"network"`
	Target  string               `json:"target"`
	Chain   []contract.TunnelHop `json:"chain,omitempty"`
	Visited []string             `json:"visited,omitempty"`
	Reverse string               `json:"reverse,omitempty"`
}
type Client struct {
	TLS     *tls.Config
	Timeout time.Duration
	Pool    *MuxPool
	useMux  bool
	reverse string
}

func (c Client) Dial(ctx context.Context, transport, endpoint, serverName, token, network, target string) (*Session, error) {
	return c.dial(ctx, transport, endpoint, serverName, token, network, target, nil, nil)
}

// DialChain keeps the original first-hop API and treats chain as the remaining
// one or two exits. Each receiving exit applies its own local next-hop policy.
func (c Client) DialChain(ctx context.Context, transport, endpoint, serverName, token, network, target string, chain []contract.TunnelHop) (*Session, error) {
	if e := ValidateChain(contract.TunnelHop{Transport: transport, Endpoint: endpoint, ServerName: serverName, Token: token}, chain); e != nil {
		return nil, e
	}
	return c.dial(ctx, transport, endpoint, serverName, token, network, target, chain, nil)
}
func (c Client) dial(ctx context.Context, transport, endpoint, serverName, token, network, target string, chain []contract.TunnelHop, visited []string) (*Session, error) {
	if c.useMux {
		return c.dialMux(ctx, transport, endpoint, serverName, token, network, target, chain, visited)
	}
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	tc := &tls.Config{MinVersion: tls.VersionTLS13}
	if c.TLS != nil {
		tc = c.TLS.Clone()
	}
	tc.MinVersion = tls.VersionTLS13
	tc.ServerName = serverName
	if tc.InsecureSkipVerify {
		return nil, errors.New("certificate verification cannot be disabled")
	}
	var conn net.Conn
	var err error
	if transport == "ws" || transport == "wss" {
		u, e := url.Parse(endpoint)
		if e != nil {
			return nil, e
		}
		want := "ws"
		if transport == "wss" {
			want = "wss"
		}
		if u.Scheme != want || u.Host == "" {
			return nil, errors.New("carrier URL scheme mismatch")
		}
		if tc.ServerName == "" {
			tc.ServerName = u.Hostname()
		}
		d := websocket.Dialer{TLSClientConfig: tc, HandshakeTimeout: timeout}
		w, resp, e := d.DialContext(ctx, endpoint, nil)
		if e != nil {
			if resp != nil && resp.Body != nil {
				resp.Body.Close()
			}
			return nil, e
		}
		w.SetReadLimit(maxFrame * 4)
		conn = &wsConn{Conn: w}
	} else if transport == "tls" || transport == "http" {
		host, _, e := net.SplitHostPort(endpoint)
		if e != nil {
			return nil, e
		}
		if tc.ServerName == "" {
			tc.ServerName = host
		}
		conn, err = (&net.Dialer{}).DialContext(ctx, "tcp", endpoint)
		if err != nil {
			return nil, err
		}
	} else {
		return nil, errors.New("unsupported transport")
	}
	ok := false
	carrier := conn
	stopCancel := context.AfterFunc(ctx, func() { carrier.Close() })
	defer stopCancel()
	defer func() {
		if !ok {
			conn.Close()
		}
	}()
	deadline, _ := ctx.Deadline()
	conn.SetDeadline(deadline)
	if transport == "http" {
		if _, err = fmt.Fprintf(conn, "CONNECT /tunnel HTTP/1.1\r\nHost: %s\r\n\r\n", endpoint); err != nil {
			return nil, err
		}
		br := bufio.NewReader(conn)
		res, e := http.ReadResponse(br, &http.Request{Method: "CONNECT"})
		if e != nil {
			return nil, e
		}
		if res.StatusCode != 200 {
			return nil, errors.New("CONNECT rejected")
		}
		conn = &bufferedConn{Conn: conn, r: br}
	}
	if transport != "wss" {
		t := tls.Client(conn, tc)
		if err = t.HandshakeContext(ctx); err != nil {
			return nil, err
		}
		conn = t
	}
	if err = c.open(conn, token, network, target, chain, visited); err != nil {
		return nil, err
	}
	if !stopCancel() || ctx.Err() != nil {
		return nil, ctx.Err()
	}
	conn.SetDeadline(time.Time{})
	ok = true
	return &Session{Conn: conn}, nil
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

type wsConn struct {
	*websocket.Conn
	reader io.Reader
	mu     sync.Mutex
}

func (c *wsConn) Read(p []byte) (int, error) {
	for {
		if c.reader == nil {
			kind, r, e := c.NextReader()
			if e != nil {
				return 0, e
			}
			if kind != websocket.BinaryMessage {
				return 0, errors.New("binary WebSocket required")
			}
			c.reader = r
		}
		n, e := c.reader.Read(p)
		if e == io.EOF {
			c.reader = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, e
	}
}
func (c *wsConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.WriteMessage(websocket.BinaryMessage, p); e != nil {
		return 0, e
	}
	return len(p), nil
}
func (c *wsConn) LocalAddr() net.Addr  { return c.UnderlyingConn().LocalAddr() }
func (c *wsConn) RemoteAddr() net.Addr { return c.UnderlyingConn().RemoteAddr() }
func (c *wsConn) SetDeadline(t time.Time) error {
	if e := c.SetReadDeadline(t); e != nil {
		return e
	}
	return c.SetWriteDeadline(t)
}

type Server struct {
	TLS   *tls.Config
	Token string
	// Allowed contains exact "tcp|host:port" or "udp|host:port" destinations.
	Allowed map[string]bool
	// NextHops is an operator-owned allowlist, including local outbound secrets.
	// Requested hops must match these entries; an empty list disables chaining.
	NextHops []contract.TunnelHop
	// NodeID must be stable and unique across logical exits used in a chain.
	NodeID      string
	Client      Client
	IdleTimeout time.Duration
	mu          sync.Mutex
	conns       map[net.Conn]struct{}
	listener    net.Listener
	stopped     bool
	slots       chan struct{}
	ctx         context.Context
	cancel      context.CancelFunc
	reverse     map[string]*yamux.Session
}

func (s *Server) Serve(l net.Listener, transport string) error {
	if s.TLS == nil || len(s.TLS.Certificates) == 0 || len(s.Token) < 16 || len(s.Allowed) == 0 {
		return errors.New("certificate, token (16+ chars) and explicit target allowlist required")
	}
	if transport != "tls" && transport != "ws" && transport != "wss" && transport != "http" {
		return errors.New("unsupported transport")
	}
	if (s.NodeID != "" && !validNodeID(s.NodeID)) || (len(s.NextHops) > 0 && !validNodeID(s.NodeID)) {
		return errors.New("chaining requires a stable unique exit ID (1-128 characters)")
	}
	if len(s.NextHops) > 64 {
		return errors.New("at most 64 next-hop entries")
	}
	for _, hop := range s.NextHops {
		if err := ValidateChain(hop, nil); err != nil {
			return err
		}
	}
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		l.Close()
		return net.ErrClosed
	}
	s.listener = l
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.conns = make(map[net.Conn]struct{})
	s.slots = make(chan struct{}, 256)
	s.mu.Unlock()
	defer s.Close()
	if transport == "ws" || transport == "wss" {
		if transport == "wss" {
			tc := s.TLS.Clone()
			tc.MinVersion = tls.VersionTLS13
			l = tls.NewListener(l, tc)
		}
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/tunnel" {
				http.NotFound(w, r)
				return
			}
			select {
			case s.slots <- struct{}{}:
			default:
				http.Error(w, "capacity", 503)
				return
			}
			defer func() { <-s.slots }()
			up := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "" }}
			c, e := up.Upgrade(w, r, nil)
			if e != nil {
				return
			}
			c.SetReadLimit(maxFrame * 4)
			s.handle(&wsConn{Conn: c}, transport)
		})
		srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: 8192}
		return srv.Serve(l)
	}
	for {
		c, e := l.Accept()
		if e != nil {
			return e
		}
		select {
		case s.slots <- struct{}{}:
			go func() { defer func() { <-s.slots }(); s.handle(c, transport) }()
		default:
			c.Close()
		}
	}
}
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = true
	if s.cancel != nil {
		s.cancel()
	}
	for c := range s.conns {
		c.Close()
	}
	if s.listener != nil {
		return s.listener.Close()
	}
	return nil
}
func (s *Server) handle(raw net.Conn, transport string) {
	defer raw.Close()
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.conns[raw] = struct{}{}
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.conns, raw); s.mu.Unlock() }()
	raw.SetDeadline(time.Now().Add(10 * time.Second))
	var conn net.Conn = raw
	if transport == "http" {
		// Read the header with a hard bound; do not consume TLS bytes that
		// follow the CONNECT response on the same socket.
		br := bufio.NewReaderSize(raw, 8192)
		var header strings.Builder
		for {
			part, err := br.ReadSlice('\n')
			if err != nil {
				return
			}
			line := string(part)
			if header.Len()+len(line) > 8192 {
				return
			}
			header.WriteString(line)
			if line == "\r\n" {
				break
			}
		}
		req, e := http.ReadRequest(bufio.NewReader(strings.NewReader(header.String())))
		if e != nil || req.Method != "CONNECT" || req.RequestURI != "/tunnel" {
			return
		}
		if req.ContentLength > 0 || len(req.TransferEncoding) > 0 {
			return
		}
		if _, e = io.WriteString(raw, "HTTP/1.1 200 Connection Established\r\n\r\n"); e != nil {
			return
		}
		conn = &bufferedConn{Conn: raw, r: br}
	}
	if transport != "wss" {
		tc := s.TLS.Clone()
		tc.MinVersion = tls.VersionTLS13
		t := tls.Server(conn, tc)
		if e := t.Handshake(); e != nil {
			return
		}
		conn = t
	}
	s.serveRequest(conn, true)
}

func (s *Server) serveRequest(conn net.Conn, special bool) {
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	k, p, e := readFrame(conn)
	if e != nil || k != openFrame || len(p) > 8192 {
		return
	}
	var req openRequest
	dec := json.NewDecoder(strings.NewReader(string(p)))
	dec.DisallowUnknownFields()
	if e = dec.Decode(&req); e != nil || (req.Version != 1 && req.Version != 2) || subtle.ConstantTimeCompare([]byte(req.Token), []byte(s.Token)) != 1 {
		return
	}
	if dec.Decode(new(any)) != io.EOF {
		return
	}
	if special && (req.Network == "mux" || req.Network == "reverse") {
		s.serveMultiplex(conn, req)
		return
	}
	if !s.Allowed[req.Network+"|"+req.Target] || (req.Network != "tcp" && req.Network != "udp") {
		return
	}
	if s.validateRoute(req) != nil {
		return
	}
	var target net.Conn
	var next *Session
	if req.Reverse != "" {
		if len(req.Chain) > 0 {
			return
		}
		next, e = s.openReverse(req)
		target = next
	} else if len(req.Chain) > 0 {
		hop, allowed := s.authorizedNext(req.Chain[0])
		if !allowed {
			return
		}
		visited := append(append([]string(nil), req.Visited...), s.NodeID)
		next, e = s.Client.dial(s.ctx, hop.Transport, hop.Endpoint, hop.ServerName, hop.Token, req.Network, req.Target, req.Chain[1:], visited)
		target = next
	} else {
		target, e = (&net.Dialer{Timeout: 10 * time.Second}).DialContext(s.ctx, req.Network, req.Target)
	}
	if e != nil {
		return
	}
	defer target.Close()
	if e = writeFrame(conn, readyFrame, []byte("ok")); e != nil {
		return
	}
	conn.SetDeadline(time.Time{})
	session := &Session{Conn: conn}
	idle := s.IdleTimeout
	if idle == 0 {
		idle = 2 * time.Minute
	}
	if req.Network == "udp" {
		if next != nil {
			relayPackets(session, next, idle)
			return
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer target.Close()
			for {
				p, e := session.ReadPacket()
				if e != nil {
					return
				}
				target.SetWriteDeadline(time.Now().Add(idle))
				if _, e = target.Write(p); e != nil {
					return
				}
			}
		}()
		buf := make([]byte, maxFrame)
		for {
			target.SetReadDeadline(time.Now().Add(idle))
			n, e := target.Read(buf)
			if e != nil {
				break
			}
			session.SetWriteDeadline(time.Now().Add(idle))
			if e = session.WritePacket(buf[:n]); e != nil {
				break
			}
		}
		session.Close()
		<-done
		return
	}
	Relay(session, target, idle, nil)
}

// Relay forwards TCP streams with bounded buffers and half-close propagation.
// charge reserves bytes durably before sending, providing a fail-closed budget.
func Relay(client, target net.Conn, idle time.Duration, charge func(upload bool, n int) error) {
	var wg sync.WaitGroup
	wg.Add(2)
	copyOne := func(dst, src net.Conn, up bool) {
		defer wg.Done()
		buf := make([]byte, 32*1024)
		for {
			src.SetReadDeadline(time.Now().Add(idle))
			n, e := src.Read(buf)
			if n > 0 {
				if charge != nil {
					if err := charge(up, n); err != nil {
						client.Close()
						target.Close()
						return
					}
				}
				dst.SetWriteDeadline(time.Now().Add(idle))
				if err := writeAll(dst, buf[:n]); err != nil {
					client.Close()
					target.Close()
					return
				}
			}
			if e != nil {
				if e == io.EOF {
					if cw, ok := dst.(interface{ CloseWrite() error }); ok {
						cw.CloseWrite()
					} else {
						dst.Close()
					}
				} else {
					client.Close()
					target.Close()
				}
				return
			}
		}
	}
	go copyOne(target, client, true)
	go copyOne(client, target, false)
	wg.Wait()
}
