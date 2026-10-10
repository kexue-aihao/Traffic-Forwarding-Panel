package policy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/policy/detect"
)

const H2Preface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"
const HeaderLimit = 32768

type Inspection struct {
	Kind, Host string
	ALPN       []string
	Prefix     []byte
	Conn       net.Conn
	Detection  detect.Detection
	plan       *detect.Plan
}

type replay struct {
	net.Conn
	r io.Reader
}

func (c *replay) Read(p []byte) (int, error) { return c.r.Read(p) }
func (c *replay) CloseWrite() error {
	if v, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return v.CloseWrite()
	}
	return c.Close()
}

type capture struct {
	net.Conn
	b bytes.Buffer
}

func (c *capture) Read(p []byte) (int, error) {
	if c.b.Len() >= 65535 {
		return 0, errors.New("inspection prefix too large")
	}
	n, e := c.Conn.Read(p[:min(len(p), 65535-c.b.Len())])
	c.b.Write(p[:n])
	return n, e
}

// The parser never sends handshake replies to the business client.
func (c *capture) Write(p []byte) (int, error) { return len(p), nil }

func inspectMetadata(conn net.Conn) (Inspection, error) {
	r := bufio.NewReaderSize(conn, HeaderLimit+1)
	first, e := r.Peek(1)
	if e != nil {
		return Inspection{}, e
	}
	v := Inspection{Kind: "unknown"}
	var prefix []byte
	if first[0] == 22 {
		c := &capture{Conn: &replay{Conn: conn, r: r}}
		seen := false
		parser := tls.Server(c, &tls.Config{GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
			seen = true
			v.Host = h.ServerName
			v.ALPN = append([]string{}, h.SupportedProtos...)
			return nil, errors.New("inspection complete")
		}})
		_ = parser.HandshakeContext(context.Background())
		if !seen {
			return v, errors.New("invalid ClientHello")
		}
		v.Kind = "tls"
		prefix = append([]byte{}, c.b.Bytes()...)
	} else if first[0] == 4 || first[0] == 5 {
		v.Kind = "socks"
		prefix = append(prefix, first...)
		_, _ = r.Discard(1)
	} else {
		// Classify request lines, including extension and lowercase methods. A
		// completed non-HTTP prefix is replayed; incomplete metadata times out.
		for n := 1; n <= 1024; n++ {
			p, err := r.Peek(n)
			if err != nil {
				if err == io.EOF && len(p) > 0 {
					prefix = append(prefix, p...)
					_, _ = r.Discard(len(p))
					break
				}
				return v, err
			}
			ch := p[n-1]
			if ch == ' ' {
				if string(p) == "PRI " {
					p, err = r.Peek(len(H2Preface))
					if err != nil || string(p) != H2Preface {
						return v, errors.New("invalid HTTP/2 preface")
					}
					v.Kind = "h2"
					prefix = append(prefix, p...)
					_, _ = r.Discard(len(p))
					break
				}
				line, e := readLine(r, 8192)
				if e != nil {
					if e == io.EOF {
						prefix = append(prefix, line...)
						break
					}
					return v, e
				}
				if !bytes.Contains(line, []byte(" HTTP/1.")) {
					prefix = append(prefix, line...)
					break
				}
				tail, e := readHeaderLimit(r, HeaderLimit-len(line))
				if e != nil {
					return v, e
				}
				prefix = append(line, tail...)
				v.Kind = "http"
				h, e := parseHeader(prefix)
				if e != nil {
					return v, e
				}
				v.Host = h.host
				break
			}
			token := ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(ch))
			if !token {
				prefix = append(prefix, p...)
				_, _ = r.Discard(len(p))
				break
			}
			if n == 1024 {
				return v, errors.New("method token too large")
			}
		}
	}
	v.Prefix = prefix
	v.Conn = &replay{Conn: conn, r: io.MultiReader(bytes.NewReader(prefix), r)}
	return v, nil
}

func (v Inspection) Check(layers []contract.InboundPolicy) error {
	if err := v.CheckApplications(layers); err != nil {
		return err
	}
	return v.CheckMetadata(layers)
}

func (v Inspection) CheckApplications(layers []contract.InboundPolicy) error {
	if v.plan != nil {
		if err := v.plan.Decision(v.Detection); err != nil {
			return fmt.Errorf("%w: inspection", ErrDenied)
		}
	}
	for _, p := range layers {
		if p.Inspection != nil && p.Inspection.Mode == "observe" {
			continue
		}
		for _, app := range p.BlockedApps {
			if (app == "socks" && (v.Kind == "socks4" || v.Kind == "socks5")) || app == v.Kind {
				return ErrDenied
			}
			if app == "http" {
				if v.Kind == "http" || v.Kind == "h2" {
					return ErrDenied
				}
				for _, a := range v.ALPN {
					if a == "h2" || strings.HasPrefix(a, "http/") {
						return ErrDenied
					}
				}
			}
		}
	}
	return nil
}

func (v Inspection) CheckMetadata(layers []contract.InboundPolicy) error {
	for _, p := range layers {
		if p.TLSRequired && v.Kind != "tls" {
			return ErrDenied
		}
		if p.RejectEmptySNI && v.Kind == "tls" && v.Host == "" {
			return ErrDenied
		}
	}
	if v.Kind == "h2" {
		return nil
	} // authority is checked on each HEADERS block.
	if e := CheckHost(layers, v.Host); e != nil {
		return e
	}
	if v.Kind == "http" {
		h, e := parseHeader(v.Prefix)
		if e != nil {
			return e
		}
		return CheckHTTP(layers, h.host, h.path, h.opaque)
	}
	return nil
}

func Filter(conn net.Conn, v Inspection, layers []contract.InboundPolicy) net.Conn {
	if !NeedsHTTP(layers) {
		return conn
	}
	if v.Kind == "http" {
		return &replay{Conn: conn, r: newHTTPReader(conn, layers)}
	}
	if v.Kind == "h2" {
		return &replay{Conn: conn, r: newH2Reader(conn, layers)}
	}
	return conn
}

type rejectionConn struct {
	net.Conn
	reject   func()
	rejected bool
}

func (c *rejectionConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if errors.Is(err, ErrDenied) && !c.rejected {
		c.rejected = true
		c.reject()
	}
	return n, err
}
func (c *rejectionConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Close()
}

func FilterWithRejection(conn net.Conn, v Inspection, layers []contract.InboundPolicy, reject func()) net.Conn {
	filtered := Filter(conn, v, layers)
	if filtered == conn || reject == nil {
		return filtered
	}
	return &rejectionConn{Conn: filtered, reject: reject}
}

// UDP checks the business datagram, never the outer QUIC/DTLS carrier.
func BlockedDatagram(p []byte, apps []string) bool {
	d := detect.Structural(p, true, "udp")
	for _, app := range apps {
		if (app == "socks" || app == "socks5") && d.Protocol == "socks5" && d.Status == detect.Match {
			return true
		}
		if app == "http" {
			for _, method := range []string{"GET ", "POST ", "HEAD ", "PUT ", "DELETE ", "OPTIONS ", "CONNECT ", "TRACE ", "PATCH ", "PRI "} {
				if bytes.HasPrefix(p, []byte(method)) {
					return true
				}
			}
		}
	}
	return false
}

func BlockedDatagramWithPlan(p []byte, layers []contract.InboundPolicy, plan *detect.Plan) bool {
	_, denied := DatagramDecision(p, layers, plan)
	return denied
}

// DatagramDecision authenticates each datagram once. A confirmed encrypted
// protocol takes precedence over coincidental plaintext header bytes.
func DatagramDecision(p []byte, layers []contract.InboundPolicy, plan *detect.Plan) (detect.Detection, bool) {
	d := detect.Structural(p, true, "udp")
	if plan != nil && !plan.Empty() {
		d = plan.Feed(p, true, "udp")
		if plan.Decision(d) != nil {
			return d, true
		}
		if d.Status == detect.Match && d.Evidence == detect.Authenticated {
			return d, false
		}
	}
	for _, layer := range layers {
		if layer.Inspection != nil && layer.Inspection.Mode == "observe" {
			continue
		}
		if BlockedDatagram(p, layer.BlockedApps) {
			return d, true
		}
	}
	return d, false
}
