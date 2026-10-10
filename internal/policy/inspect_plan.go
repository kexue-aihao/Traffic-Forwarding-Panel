package policy

import (
	"bytes"
	"errors"
	"io"
	"net"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/policy/detect"
)

const InspectionLimit = 65535

var ErrInspectionCapacity = errors.New("inspection capacity exhausted")

// A shared process budget includes local listeners and managed exits. UDP uses
// its existing bounded workers and does not take a per-packet global lock.
var inspectionSlots = make(chan struct{}, 128)

type readCapture struct {
	net.Conn
	b bytes.Buffer
}

func (c *readCapture) Read(p []byte) (int, error) {
	if c.b.Len() >= InspectionLimit {
		return 0, io.ErrShortBuffer
	}
	n, err := c.Conn.Read(p[:min(len(p), InspectionLimit-c.b.Len())])
	c.b.Write(p[:n])
	return n, err
}

func Inspect(conn net.Conn) (Inspection, error) { return InspectWithPlan(conn, nil) }

// InspectWithPlan shares a bounded prefix across candidates. Authentication is
// evaluated before plaintext metadata; a random encrypted first byte must not
// immediately classify a stream as SOCKS or a malformed TLS ClientHello.
func InspectWithPlan(conn net.Conn, plan *detect.Plan) (Inspection, error) {
	select {
	case inspectionSlots <- struct{}{}:
		defer func() { <-inspectionSlots }()
	default:
		return Inspection{Kind: "unknown", Conn: conn, Detection: detect.Detection{Status: detect.Unavailable, Reason: "inspection_capacity_exhausted"}}, ErrInspectionCapacity
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer conn.SetReadDeadline(time.Time{})
	prefix := make([]byte, 0, 256)
	var detection detect.Detection
	var session *detect.Session
	if plan != nil {
		session = plan.NewSession()
	}
	end := false
	for {
		if plan == nil {
			detection = detect.Structural(prefix, end, "tcp")
		} else {
			detection = session.Feed(prefix, end, "tcp")
		}
		if detection.Status != detect.NeedMore || end {
			break
		}
		need := max(len(prefix)+1, detection.Need)
		if need > InspectionLimit {
			end = true
			continue
		}
		// A socket Read returns available bytes; it does not wait to fill this
		// chunk. Capture bounded lookahead instead of doing one syscall per HTTP
		// method/header byte. The entire consumed prefix is verified and replayed.
		chunk := make([]byte, min(4096, InspectionLimit-len(prefix)))
		n, err := conn.Read(chunk)
		prefix = append(prefix, chunk[:n]...)
		if err != nil || n == 0 {
			if len(prefix) == 0 {
				if err == nil {
					err = io.ErrNoProgress
				}
				return Inspection{}, err
			}
			end = true
		}
	}
	if len(prefix) == 0 {
		var first [1]byte
		n, err := conn.Read(first[:])
		if n == 0 {
			return Inspection{}, err
		}
		prefix = append(prefix, first[:n]...)
	}
	input := &replay{Conn: conn, r: io.MultiReader(bytes.NewReader(prefix), conn)}
	if detection.Status == detect.Match && detection.Protocol != "http" {
		return Inspection{Kind: detection.Protocol, Detection: detection, Prefix: prefix, Conn: input, plan: plan}, nil
	}
	// Metadata parsing can consume additional bytes. Track all reads so a failed
	// plaintext guess can replay the unknown stream in full exactly once.
	tracked := &readCapture{Conn: input}
	v, err := inspectMetadata(tracked)
	if err != nil {
		v = Inspection{Kind: "unknown"}
	}
	// Include any buffered lookahead in the tunnel's verifiable wire prefix.
	// The underlying socket has consumed prefix already even if the metadata
	// parser only read part of input's replay buffer.
	v.Prefix = bytes.Clone(prefix)
	if tracked.b.Len() > len(prefix) {
		v.Prefix = bytes.Clone(tracked.b.Bytes())
	}
	v.Conn = &replay{Conn: conn, r: io.MultiReader(bytes.NewReader(v.Prefix), conn)}
	// inspectMetadata historically guessed SOCKS on one byte. Structural is the
	// only source of SOCKS classification; failed greetings remain unknown.
	if v.Kind == "socks" {
		v.Kind = "unknown"
	}
	v.Detection, v.plan = detection, plan
	return v, nil
}
