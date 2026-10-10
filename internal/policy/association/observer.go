package association

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"sync"
)

const maxHandshake = 1024

type observer struct {
	mu             sync.Mutex
	registry       *Registry
	session        *session
	stage          int
	client, server []byte
	methods        []byte
	failed         bool
	closed         bool
}
type observedConn struct {
	net.Conn
	observe    *observer
	clientSide bool
}

// Observe wraps a live control TCP pair. Bytes are observed only after a
// successful Write to the opposite endpoint, preserving original wire bytes
// and proving that the handshake was actually transmitted.
func (r *Registry) Observe(client, target net.Conn, bindings ...Binding) (net.Conn, net.Conn, func(), error) {
	if client == nil || target == nil || len(bindings) == 0 || len(bindings) > MaxBindings {
		return nil, nil, nil, errors.New("invalid SOCKS association observation")
	}
	peer, e := peerIP(client)
	if e != nil {
		return nil, nil, nil, e
	}
	r.mu.Lock()
	r.pruneLocked()
	if len(r.sessions) >= r.limit {
		r.mu.Unlock()
		return nil, nil, nil, errors.New("SOCKS association session capacity exceeded")
	}
	accepted := make([]Binding, 0, len(bindings))
	for _, b := range bindings {
		b = canonicalBinding(b)
		if !validBinding(b) || !r.allowed[b] {
			r.mu.Unlock()
			return nil, nil, nil, errors.New("SOCKS association relationship not configured")
		}
		accepted = append(accepted, b)
	}
	r.next++
	s := &session{id: r.next, bindings: accepted, peer: peer, expires: r.now().Add(r.ttl)}
	r.sessions[s.id] = s
	r.mu.Unlock()
	o := &observer{registry: r, session: s}
	closeObserver := func() { o.close() }
	return &observedConn{Conn: client, observe: o, clientSide: true}, &observedConn{Conn: target, observe: o, clientSide: false}, closeObserver, nil
}

func (c *observedConn) Read(b []byte) (int, error) {
	n, e := c.Conn.Read(b)
	if e != nil {
		c.observe.close()
	}
	return n, e
}
func (c *observedConn) Write(b []byte) (int, error) {
	n, e := c.Conn.Write(b)
	if n > 0 {
		c.observe.feed(!c.clientSide, b[:n])
	}
	if e != nil {
		c.observe.close()
	}
	return n, e
}
func (c *observedConn) Close() error { c.observe.close(); return c.Conn.Close() }
func (c *observedConn) CloseWrite() error {
	c.observe.close()
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Conn.Close()
}

func (o *observer) close() { o.mu.Lock(); defer o.mu.Unlock(); o.closeLocked() }
func (o *observer) closeLocked() {
	if o.closed {
		return
	}
	o.closed = true
	o.failed = true
	o.registry.mu.Lock()
	o.registry.removeSessionLocked(o.session.id, o.session)
	o.registry.mu.Unlock()
	o.client = nil
	o.server = nil
}
func (o *observer) feed(client bool, b []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.failed || o.closed || o.stage == 6 {
		return
	}
	if client {
		if len(b) > maxHandshake-len(o.client) {
			o.closeLocked()
			return
		}
		o.client = append(o.client, b...)
	} else {
		if len(b) > maxHandshake-len(o.server) {
			o.closeLocked()
			return
		}
		o.server = append(o.server, b...)
	}
	if len(o.client) > maxHandshake || len(o.server) > maxHandshake {
		o.closeLocked()
		return
	}
	for {
		switch o.stage {
		case 0:
			if len(o.client) < 2 {
				return
			}
			if o.client[0] != 5 || o.client[1] == 0 {
				o.closeLocked()
				return
			}
			n := 2 + int(o.client[1])
			if len(o.client) < n {
				return
			}
			o.methods = append([]byte(nil), o.client[2:n]...)
			for _, m := range o.methods {
				if m == 255 {
					o.closeLocked()
					return
				}
			}
			o.client = o.client[n:]
			o.stage = 1
		case 1:
			if len(o.server) < 2 {
				return
			}
			method := o.server[1]
			if o.server[0] != 5 || !bytes.Contains(o.methods, []byte{method}) || method != 0 && method != 2 {
				o.closeLocked()
				return
			}
			o.server = o.server[2:]
			if method == 2 {
				o.stage = 2
			} else {
				o.stage = 4
			}
		case 2:
			if len(o.client) < 2 {
				return
			}
			if o.client[0] != 1 || o.client[1] == 0 {
				o.closeLocked()
				return
			}
			n := 2 + int(o.client[1])
			if len(o.client) < n+1 {
				return
			}
			if o.client[n] == 0 {
				o.closeLocked()
				return
			}
			n += 1 + int(o.client[n])
			if len(o.client) < n {
				return
			}
			o.client = o.client[n:]
			o.stage = 3
		case 3:
			if len(o.server) < 2 {
				return
			}
			if o.server[0] != 1 || o.server[1] != 0 {
				o.closeLocked()
				return
			}
			o.server = o.server[2:]
			o.stage = 4
		case 4:
			if len(o.client) < 4 {
				return
			}
			if o.client[0] != 5 || o.client[1] != 3 || o.client[2] != 0 {
				o.closeLocked()
				return
			}
			endpoint, n, ok := parseAddress(o.client, 3)
			if !ok {
				if n > 0 && n <= maxHandshake {
					return
				}
				o.closeLocked()
				return
			}
			if !endpoint.Addr().IsUnspecified() && endpoint.Addr().Unmap() != o.session.peer {
				o.closeLocked()
				return
			}
			o.registry.mu.Lock()
			o.session.port = endpoint.Port()
			o.registry.mu.Unlock()
			o.client = o.client[n:]
			if len(o.client) > 0 {
				o.closeLocked()
				return
			}
			o.stage = 5
		case 5:
			if len(o.server) < 4 {
				return
			}
			if o.server[0] != 5 || o.server[1] != 0 || o.server[2] != 0 {
				o.closeLocked()
				return
			}
			endpoint, n, ok := parseAddress(o.server, 3)
			if !ok {
				if n > 0 && n <= maxHandshake {
					return
				}
				o.closeLocked()
				return
			}
			if endpoint.Addr().IsUnspecified() || endpoint.Port() == 0 {
				o.closeLocked()
				return
			}
			r := o.registry
			r.mu.Lock()
			valid := false
			if current := r.sessions[o.session.id]; current == o.session && !current.closed && r.now().Before(current.expires) {
				filtered := current.bindings[:0]
				for _, binding := range current.bindings {
					if binding.RelayEndpoint == endpoint && r.allowed[binding] {
						filtered = append(filtered, binding)
						valid = true
					}
				}
				current.bindings = filtered
				if valid {
					valid = r.indexSessionLocked(current)
					current.ready = valid
					current.expires = r.now().Add(r.ttl)
				}
			}
			r.mu.Unlock()
			if !valid {
				o.closeLocked()
				return
			}
			o.server = o.server[n:]
			if len(o.server) > 0 {
				o.closeLocked()
				return
			}
			o.stage = 6
			o.methods = nil
			o.client = nil
			o.server = nil
			return
		default:
			return
		}
	}
}

// parseAddress parses only numeric SOCKS endpoints. A domain BND cannot prove
// an exact advertised socket without DNS assumptions, so it is unsupported.
func parseAddress(b []byte, start int) (netip.AddrPort, int, bool) {
	if len(b) <= start {
		return netip.AddrPort{}, start + 1, false
	}
	n := start + 1
	var addr netip.Addr
	switch b[start] {
	case 1:
		if len(b) < n+4+2 {
			return netip.AddrPort{}, n + 4 + 2, false
		}
		var a [4]byte
		copy(a[:], b[n:n+4])
		addr = netip.AddrFrom4(a)
		n += 4
	case 4:
		if len(b) < n+16+2 {
			return netip.AddrPort{}, n + 16 + 2, false
		}
		var a [16]byte
		copy(a[:], b[n:n+16])
		addr = netip.AddrFrom16(a).Unmap()
		n += 16
	case 3:
		if len(b) <= n {
			return netip.AddrPort{}, n + 1, false
		}
		length := int(b[n])
		if length == 0 {
			return netip.AddrPort{}, -1, false
		}
		n += 1 + length + 2
		if len(b) < n {
			return netip.AddrPort{}, n, false
		}
		return netip.AddrPort{}, -1, false
	default:
		return netip.AddrPort{}, -1, false
	}
	port := binary.BigEndian.Uint16(b[n : n+2])
	return netip.AddrPortFrom(addr, port), n + 2, true
}
