package tunnel

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/policy"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/policy/detect"
)

type wsInspectionState struct {
	mu                     sync.Mutex
	request, ready         chan struct{}
	requestOnce, readyOnce sync.Once
	key                    string
	err                    error
}

var businessInspectionSlots = make(chan struct{}, 128)

func (s *wsInspectionState) fail(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
	s.requestOnce.Do(func() { close(s.request) })
	s.readyOnce.Do(func() { close(s.ready) })
}
func (s *wsInspectionState) wait(ch <-chan struct{}) error {
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-ch:
	case <-timer.C:
		return errors.New("business WebSocket handshake timed out")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// WebSocketInspection forwards the original wire frames. Only the bounded,
// unmasked payload view enters the detector. The response reader validates 101
// before the request reader releases any inner application frames.
func WebSocketInspection(client, target net.Conn, layers []contract.InboundPolicy, plan *detect.Plan, reject func(), observers ...func(detect.Detection)) (net.Conn, net.Conn) {
	return WebSocketBusinessInspection(client, target, layers, plan, nil, reject, observers...)
}
func WebSocketBusinessInspection(client, target net.Conn, layers []contract.InboundPolicy, plan *detect.Plan, business *contract.BusinessInbound, reject func(), observers ...func(detect.Detection)) (net.Conn, net.Conn) {
	s := &wsInspectionState{request: make(chan struct{}), ready: make(chan struct{})}
	c := &wsClientGate{Conn: client, r: bufio.NewReaderSize(client, policy.HeaderLimit+1), state: s, layers: layers, plan: plan, session: plan.NewSession(), reject: reject, observers: observers}
	if business != nil {
		c.earlyData = business.WebSocketEarlyData
	}
	t := &wsResponseGate{Conn: target, r: bufio.NewReaderSize(target, policy.HeaderLimit+1), state: s}
	return c, t
}

func wsHeader(r *bufio.Reader) ([]byte, error) {
	var b []byte
	for {
		line, err := r.ReadSlice('\n')
		if err != nil {
			return nil, err
		}
		if len(b)+len(line) > policy.HeaderLimit {
			return nil, errors.New("business WebSocket header too large")
		}
		b = append(b, line...)
		if bytes.Equal(line, []byte("\r\n")) {
			return b, nil
		}
	}
}
func headerToken(h http.Header, key, want string) bool {
	for _, value := range h.Values(key) {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), want) {
				return true
			}
		}
	}
	return false
}

type wsClientGate struct {
	net.Conn
	r                            *bufio.Reader
	state                        *wsInspectionState
	layers                       []contract.InboundPolicy
	plan                         *detect.Plan
	session                      *detect.Session
	pending, raw, payload        []byte
	started, decided, fragmented bool
	reject                       func()
	observers                    []func(detect.Detection)
	earlyData                    bool
	denyOnce                     sync.Once
	slotMu                       sync.Mutex
	slotHeld, slotClosed         bool
	payloadDeadline              time.Time
}

func (c *wsClientGate) releaseSlot() {
	c.slotMu.Lock()
	defer c.slotMu.Unlock()
	c.slotClosed = true
	if c.slotHeld {
		<-businessInspectionSlots
		c.slotHeld = false
	}
}

func (c *wsClientGate) acquireSlot() bool {
	c.slotMu.Lock()
	defer c.slotMu.Unlock()
	if c.slotClosed {
		return false
	}
	select {
	case businessInspectionSlots <- struct{}{}:
		c.slotHeld = true
		return true
	default:
		return false
	}
}
func (c *wsClientGate) Close() error {
	c.releaseSlot()
	c.state.fail(net.ErrClosed)
	return c.Conn.Close()
}
func (c *wsClientGate) CloseWrite() error {
	c.releaseSlot()
	c.state.fail(net.ErrClosed)
	if v, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return v.CloseWrite()
	}
	return c.Close()
}
func (c *wsClientGate) denied(err error) (int, error) {
	c.releaseSlot()
	if isTimeout(err) || strings.Contains(err.Error(), "timed out") {
		c.observed(detect.Detection{Status: detect.Unavailable, Reason: "inspection_timeout"})
	}
	if errors.Is(err, detect.ErrDenied) || errors.Is(err, policy.ErrDenied) {
		c.denyOnce.Do(func() {
			if c.reject != nil {
				c.reject()
			}
		})
	}
	c.state.fail(err)
	return 0, err
}
func (c *wsClientGate) observed(d detect.Detection) {
	for _, observe := range c.observers {
		observe(d)
	}
}
func (c *wsClientGate) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(c.pending) > 0 {
		n := copy(p, c.pending)
		c.pending = c.pending[n:]
		return n, nil
	}
	if !c.started {
		if !c.acquireSlot() {
			c.observed(detect.Detection{Status: detect.Unavailable, Reason: "inspection_capacity_exhausted"})
			return c.denied(errors.New("WebSocket inspection capacity exceeded"))
		}
		c.Conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		header, err := wsHeader(c.r)
		if err != nil {
			return c.denied(err)
		}
		req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(header)))
		if err != nil || req.Method != "GET" || req.Proto != "HTTP/1.1" || !headerToken(req.Header, "Connection", "upgrade") || !strings.EqualFold(req.Header.Get("Upgrade"), "websocket") || req.ContentLength != 0 || len(req.TransferEncoding) != 0 {
			return c.denied(errors.New("invalid business WebSocket request"))
		}
		key := req.Header.Get("Sec-WebSocket-Key")
		decoded, err := base64.StdEncoding.DecodeString(key)
		if err != nil || len(decoded) != 16 || req.Header.Get("Sec-WebSocket-Version") != "13" || req.Header.Get("Sec-WebSocket-Extensions") != "" {
			return c.denied(errors.New("unsupported business WebSocket handshake"))
		}
		if err := policy.CheckHTTP(c.layers, req.Host, req.URL.RequestURI(), false); err != nil {
			return c.denied(err)
		}
		// Early data is explicitly enabled. Ordinary subprotocol names must
		// never be guessed to be authentication material merely because they
		// happen to be valid base64 strings.
		if c.earlyData {
			values := req.Header.Values("Sec-WebSocket-Protocol")
			if len(values) > 1 {
				return c.denied(errors.New("ambiguous WebSocket early data"))
			}
			if len(values) == 1 {
				token := strings.TrimSpace(values[0])
				if strings.Contains(token, ",") || len(token) > 16384 {
					return c.denied(errors.New("unsupported WebSocket early data"))
				}
				early, err := base64.RawURLEncoding.DecodeString(token)
				if err != nil || len(early) == 0 {
					return c.denied(errors.New("invalid WebSocket early data"))
				}
				d := c.session.Feed(early, false, "tcp")
				c.observed(d)
				if d.Status == detect.NeedMore {
					return c.denied(errors.New("incomplete WebSocket early data before upgrade"))
				}
				if err := c.plan.Decision(d); err != nil {
					return c.denied(err)
				}
				c.payload = append(c.payload, early...)
			}
		}
		c.state.mu.Lock()
		c.state.key = key
		c.state.mu.Unlock()
		c.state.requestOnce.Do(func() { close(c.state.request) })
		c.started = true
		c.pending = header
		return c.Read(p)
	}
	if c.decided {
		return c.r.Read(p)
	}
	if err := c.state.wait(c.state.ready); err != nil {
		return c.denied(err)
	}
	if c.payloadDeadline.IsZero() {
		c.payloadDeadline = time.Now().Add(5 * time.Second)
	}
	c.Conn.SetReadDeadline(c.payloadDeadline)
	for {
		raw, payload, opcode, fin, err := readInspectionWSFrame(c.r)
		if err != nil {
			if len(c.raw)+len(raw) > maxFrame {
				return c.denied(errors.New("WebSocket inspection wire budget exceeded"))
			}
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || isTimeout(err) {
				c.raw = append(c.raw, raw...)
				if len(payload) > 0 && opcode < 8 {
					if opcode == 0 && !c.fragmented || opcode != 0 && c.fragmented || len(c.payload)+len(payload) > maxFrame {
						return c.denied(errors.New("invalid partial business WebSocket frame"))
					}
					c.payload = append(c.payload, payload...)
				}
				d := c.session.Feed(c.payload, true, "tcp")
				if isTimeout(err) && d.Status != detect.Match {
					d = detect.Detection{Status: detect.Unavailable, Reason: "inspection_timeout"}
				}
				c.observed(d)
				if denied := c.plan.Decision(d); denied != nil {
					return c.denied(denied)
				}
				c.decided = true
				c.releaseSlot()
				c.Conn.SetReadDeadline(time.Time{})
				c.pending = c.raw
				c.raw = nil
				c.payload = nil
				if len(c.pending) > 0 {
					return c.Read(p)
				}
			}
			return c.denied(err)
		}
		if len(c.raw)+len(raw) > maxFrame {
			return c.denied(errors.New("WebSocket inspection wire budget exceeded"))
		}
		if opcode >= 8 && len(c.raw) == 0 {
			c.pending = raw
			return c.Read(p)
		}
		c.raw = append(c.raw, raw...)
		if opcode < 8 {
			if opcode == 0 && !c.fragmented || opcode != 0 && c.fragmented {
				return c.denied(errors.New("invalid WebSocket continuation"))
			}
			c.fragmented = !fin
			if len(c.payload)+len(payload) > maxFrame {
				return c.denied(errors.New("WebSocket inspection payload budget exceeded"))
			}
			c.payload = append(c.payload, payload...)
		}
		d := c.session.Feed(c.payload, false, "tcp")
		if d.Status == detect.NeedMore {
			continue
		}
		c.observed(d)
		if err := c.plan.Decision(d); err != nil {
			return c.denied(err)
		}
		c.decided = true
		c.releaseSlot()
		c.Conn.SetReadDeadline(time.Time{})
		c.pending = c.raw
		c.raw = nil
		c.payload = nil
		return c.Read(p)
	}
}

func isTimeout(err error) bool { var e net.Error; return errors.As(err, &e) && e.Timeout() }

func readInspectionWSFrame(r *bufio.Reader) (raw, payload []byte, opcode byte, fin bool, err error) {
	read := func(b []byte) error {
		n, e := io.ReadFull(r, b)
		raw = append(raw, b[:n]...)
		return e
	}
	var first [2]byte
	if err = read(first[:]); err != nil {
		return
	}
	opcode = first[0] & 15
	fin = first[0]&128 != 0
	if first[0]&112 != 0 || first[1]&128 == 0 || opcode != 0 && opcode != 1 && opcode != 2 && opcode != 8 && opcode != 9 && opcode != 10 {
		err = errors.New("invalid business WebSocket frame")
		return
	}
	length := uint64(first[1] & 127)
	if length == 126 {
		var ext [2]byte
		if err = read(ext[:]); err != nil {
			return
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
		if length < 126 {
			err = errors.New("noncanonical WebSocket length")
			return
		}
	}
	if length == 127 {
		var ext [8]byte
		if err = read(ext[:]); err != nil {
			return
		}
		length = binary.BigEndian.Uint64(ext[:])
		if length < 65536 {
			err = errors.New("noncanonical WebSocket length")
			return
		}
	}
	if length > maxFrame || opcode >= 8 && (!fin || length > 125) {
		err = errors.New("WebSocket frame exceeds inspection budget")
		return
	}
	var mask [4]byte
	if err = read(mask[:]); err != nil {
		return
	}
	start := len(raw)
	err = read(make([]byte, int(length)))
	payload = make([]byte, len(raw)-start)
	for i := range payload {
		payload[i] = raw[start+i] ^ mask[i%4]
	}
	return
}

type wsResponseGate struct {
	net.Conn
	r       *bufio.Reader
	state   *wsInspectionState
	pending []byte
	started bool
}

func (c *wsResponseGate) Close() error { c.state.fail(net.ErrClosed); return c.Conn.Close() }
func (c *wsResponseGate) CloseWrite() error {
	if v, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return v.CloseWrite()
	}
	return c.Close()
}
func (c *wsResponseGate) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(c.pending) > 0 {
		n := copy(p, c.pending)
		c.pending = c.pending[n:]
		return n, nil
	}
	if c.started {
		return c.r.Read(p)
	}
	if err := c.state.wait(c.state.request); err != nil {
		return 0, err
	}
	c.Conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	header, err := wsHeader(c.r)
	if err != nil {
		c.state.fail(err)
		return 0, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(header)), nil)
	c.state.mu.Lock()
	key := c.state.key
	c.state.mu.Unlock()
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	if err != nil || resp.StatusCode != 101 || !headerToken(resp.Header, "Connection", "upgrade") || !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") || resp.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(sum[:]) || resp.Header.Get("Sec-WebSocket-Extensions") != "" {
		err = errors.New("business WebSocket upgrade rejected")
		c.state.fail(err)
		return 0, err
	}
	c.started = true
	c.pending = header
	c.state.readyOnce.Do(func() { close(c.state.ready) })
	return c.Read(p)
}
