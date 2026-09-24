package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"io"
	"net"
	"strings"
	"time"
)

type route struct {
	rule  contract.Rule
	until time.Time
	pool  *resourcePool
}

func sameRoutes(a, b map[string]route) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		other, ok := b[k]
		if !ok || !sameForwardingRule(v.rule, other.rule) {
			return false
		}
	}
	return true
}
func validateSharedRules(rules []contract.Rule) error {
	byID := map[string]contract.Rule{}
	names := map[string]bool{}
	for _, r := range rules {
		if r.Enabled {
			byID[r.ID] = r
		}
	}
	for _, r := range rules {
		if !r.Enabled || r.SharedTLS == nil {
			continue
		}
		t := r.SharedTLS
		k := key(r) + "|" + t.ServerName
		if names[k] {
			return errors.New("duplicate shared TLS SNI")
		}
		names[k] = true
		if t.ParentID == "" {
			continue
		}
		p, ok := byID[t.ParentID]
		if !ok || p.SharedTLS == nil || p.SharedTLS.ParentID != "" || p.UserID != r.UserID || p.GroupID != r.GroupID || p.NodeID != r.NodeID || key(p) != key(r) {
			return errors.New("shared TLS parent unavailable or unauthorized")
		}
	}
	return nil
}

type helloConn struct {
	net.Conn
	captured bytes.Buffer
}

func (c *helloConn) Read(p []byte) (int, error) {
	remain := 65536 - c.captured.Len()
	if remain <= 0 {
		return 0, errors.New("ClientHello too large")
	}
	if len(p) > remain {
		p = p[:remain]
	}
	n, e := c.Conn.Read(p)
	c.captured.Write(p[:n])
	return n, e
}

// The standard TLS parser handles fragmented ClientHello records. No handshake
// response is written: bytes are replayed unchanged to the selected origin.
func (c *helloConn) Write(p []byte) (int, error) { return len(p), nil }

type replayConn struct {
	net.Conn
	reader io.Reader
}

func (c *replayConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
func (c *replayConn) CloseWrite() error {
	if v, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return v.CloseWrite()
	}
	return c.Close()
}
func readServerName(conn net.Conn) (string, net.Conn, error) {
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	recorder := &helloConn{Conn: conn}
	name := ""
	sentinel := errors.New("hello captured")
	parser := tls.Server(recorder, &tls.Config{GetConfigForClient: func(info *tls.ClientHelloInfo) (*tls.Config, error) {
		name = strings.ToLower(info.ServerName)
		return nil, sentinel
	}})
	_ = parser.HandshakeContext(context.Background())
	if !contract.ValidServerName(name) {
		return "", nil, errors.New("exact DNS SNI required")
	}
	conn.SetReadDeadline(time.Time{})
	return name, &replayConn{Conn: conn, reader: io.MultiReader(bytes.NewReader(recorder.captured.Bytes()), conn)}, nil
}
