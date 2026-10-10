package tunnel

import (
	"bytes"
	"crypto/subtle"
	"errors"
	"io"
	"net"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/policy"
)

const acceptedFrame byte = 5
const inspectionFrame byte = 6

func (s *Server) authorized(req openRequest) bool {
	if len(s.Grants) == 0 {
		return !s.Managed && subtle.ConstantTimeCompare([]byte(req.Token), []byte(s.Token)) == 1
	}
	for _, g := range s.Grants {
		if req.Network == "reverse" {
			if req.Target == g.Identity && subtle.ConstantTimeCompare([]byte(req.Token), []byte(g.Token)) == 1 {
				return true
			}
			continue
		}
		if subtle.ConstantTimeCompare([]byte(req.Token), []byte(g.StreamToken)) != 1 {
			continue
		}
		if req.Reverse != "" && req.Reverse != g.Identity {
			continue
		}
		if req.Network == "mux" {
			return req.Target == ""
		}
		for _, t := range g.Targets {
			if t.RuleID == req.RuleID && t.Network == req.Network && t.Target == req.Target {
				return true
			}
		}
	}
	return false
}

type prefixConn struct {
	net.Conn
	r io.Reader
}

func (c *prefixConn) Read(p []byte) (int, error)    { return c.r.Read(p) }
func (*prefixConn) SetReadDeadline(time.Time) error { return nil }

func inspectPrefix(p []byte, layers []contract.InboundPolicy) (policy.Inspection, error) {
	if len(p) == 0 {
		return policy.Inspection{}, errors.New("inspection prefix required")
	}
	v, e := policy.Inspect(&prefixConn{r: bytes.NewReader(p)})
	if e != nil {
		return v, e
	}
	return v, v.Check(layers)
}

type verifiedConn struct {
	net.Conn
	prefix []byte
	reader io.Reader
}

func (c *verifiedConn) Read(p []byte) (int, error) {
	if c.reader == nil {
		b := make([]byte, len(c.prefix))
		if _, e := io.ReadFull(c.Conn, b); e != nil {
			return 0, e
		}
		if !bytes.Equal(b, c.prefix) {
			return 0, errors.New("inspection replay mismatch")
		}
		c.reader = io.MultiReader(bytes.NewReader(b), c.Conn)
		c.prefix = nil
	}
	return c.reader.Read(p)
}
func (c *verifiedConn) CloseWrite() error {
	if v, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return v.CloseWrite()
	}
	return c.Close()
}

func (s *Server) ReverseReady(identity string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.reverse[identity]
	return v != nil && !v.IsClosed()
}

type targetState struct {
	failures int
	until    time.Time
	probing  bool
}

func (s *Server) targetAttempt(req openRequest) (func(error), error) {
	if s.Policy == nil || s.Policy.Failover == nil {
		return func(error) {}, nil
	}
	f := *s.Policy.Failover
	key := req.RuleID + "|" + req.Target + "|" + req.Reverse
	if len(req.Chain) > 0 {
		key += "|" + req.Chain[0].Endpoint
	}
	s.mu.Lock()
	if s.targets == nil {
		s.targets = map[string]*targetState{}
	}
	state := s.targets[key]
	if state == nil {
		if len(s.targets) >= 4096 {
			s.mu.Unlock()
			return nil, errors.New("target circuit capacity reached")
		}
		state = &targetState{}
		s.targets[key] = state
	}
	if time.Now().Before(state.until) || state.probing {
		s.mu.Unlock()
		return nil, errors.New("target cooling")
	}
	state.probing = !state.until.IsZero()
	s.mu.Unlock()
	return func(err error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		state.probing = false
		if err == nil {
			state.failures = 0
			state.until = time.Time{}
			return
		}
		state.failures++
		if state.failures >= max(1, f.MaxFail) {
			state.until = time.Now().Add(time.Duration(max(1, f.CooldownSec)) * time.Second)
		}
	}, nil
}
