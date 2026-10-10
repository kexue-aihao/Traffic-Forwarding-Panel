package policy

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"golang.org/x/net/http2/hpack"
)

type h2Reader struct {
	r          io.Reader
	layers     []contract.InboundPolicy
	decoder    *hpack.Decoder
	pending    []byte
	preface    bool
	err        error
	streams    map[uint32]bool
	lastStream uint32
}

func newH2Reader(r io.Reader, p []contract.InboundPolicy) *h2Reader {
	d := hpack.NewDecoder(4096, nil)
	d.SetMaxStringLength(HeaderLimit)
	return &h2Reader{r: r, layers: p, decoder: d, streams: map[uint32]bool{}}
}
func readH2Frame(r io.Reader) ([]byte, error) {
	var h [9]byte
	if _, e := io.ReadFull(r, h[:]); e != nil {
		return nil, e
	}
	n := int(h[0])<<16 | int(h[1])<<8 | int(h[2])
	if n > 16384 {
		return nil, errors.New("HTTP/2 frame exceeds initial max frame size")
	}
	b := make([]byte, 9+n)
	copy(b, h[:])
	_, e := io.ReadFull(r, b[9:])
	return b, e
}
func (r *h2Reader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(r.pending) == 0 && r.err == nil {
		r.err = r.next()
	}
	if len(r.pending) > 0 {
		n := copy(p, r.pending)
		r.pending = r.pending[n:]
		return n, nil
	}
	return 0, r.err
}
func (r *h2Reader) next() error {
	if !r.preface {
		b := make([]byte, len(H2Preface))
		if _, e := io.ReadFull(r.r, b); e != nil {
			return e
		}
		if string(b) != H2Preface {
			return errors.New("invalid h2 preface")
		}
		r.pending = b
		r.preface = true
		return nil
	}
	b, e := readH2Frame(r.r)
	if e != nil {
		return e
	}
	if b[3] == 9 {
		return errors.New("unexpected HTTP/2 CONTINUATION")
	}
	if b[3] != 1 {
		if b[3] == 5 {
			return errors.New("client PUSH_PROMISE denied")
		}
		stream := binary.BigEndian.Uint32(b[5:9]) & 0x7fffffff
		if b[3] == 0 {
			if !r.streams[stream] {
				return errors.New("h2 DATA requires validated request headers")
			}
			if b[4]&1 != 0 {
				delete(r.streams, stream)
			}
		}
		if b[3] == 3 {
			delete(r.streams, stream)
		}
		r.pending = b
		return nil
	}
	stream := binary.BigEndian.Uint32(b[5:9]) & 0x7fffffff
	if stream == 0 || stream%2 == 0 {
		return errors.New("invalid client headers stream")
	}
	trailers := r.streams[stream]
	endStream := b[4]&1 != 0
	data := b[9:]
	pad := 0
	if b[4]&8 != 0 {
		if len(data) == 0 {
			return errors.New("invalid padding")
		}
		pad = int(data[0])
		data = data[1:]
	}
	if b[4]&32 != 0 {
		if len(data) < 5 {
			return errors.New("invalid priority")
		}
		data = data[5:]
	}
	if pad > len(data) {
		return errors.New("invalid padding")
	}
	block := append([]byte{}, data[:len(data)-pad]...)
	frames := b
	for b[4]&4 == 0 {
		b, e = readH2Frame(r.r)
		if e != nil {
			return e
		}
		if b[3] != 9 || binary.BigEndian.Uint32(b[5:9])&0x7fffffff != stream {
			return errors.New("invalid continuation sequence")
		}
		if len(block)+len(b)-9 > HeaderLimit || len(frames)+len(b) > 65535 {
			return errors.New("h2 header block too large")
		}
		block = append(block, b[9:]...)
		frames = append(frames, b...)
	}
	fields := []hpack.HeaderField{}
	decodedBytes := 0
	tooLarge := false
	r.decoder.SetEmitEnabled(true)
	r.decoder.SetEmitFunc(func(f hpack.HeaderField) {
		decodedBytes += len(f.Name) + len(f.Value) + 32
		if decodedBytes > HeaderLimit {
			tooLarge = true
			r.decoder.SetEmitEnabled(false)
			return
		}
		fields = append(fields, f)
	})
	if _, e = r.decoder.Write(block); e != nil {
		return e
	}
	if e = r.decoder.Close(); e != nil {
		return e
	}
	if tooLarge {
		return errors.New("decoded h2 headers too large")
	}
	host, path, method, scheme := "", "", "", ""
	seen := map[string]bool{}
	size := 0
	regular := false
	for _, f := range fields {
		size += len(f.Name) + len(f.Value) + 32
		if size > HeaderLimit {
			return errors.New("decoded h2 headers too large")
		}
		if f.Name != strings.ToLower(f.Name) || strings.ContainsAny(f.Value, "\x00\r\n") {
			return errors.New("invalid h2 header")
		}
		if strings.HasPrefix(f.Name, ":") {
			if regular || seen[f.Name] {
				return errors.New("invalid pseudo header")
			}
			seen[f.Name] = true
			if f.Name != ":authority" && f.Name != ":path" && f.Name != ":method" && f.Name != ":scheme" {
				return errors.New("unsupported request pseudo header")
			}
		} else {
			regular = true
		}
		switch f.Name {
		case ":authority", "host":
			n, e := Host(f.Value)
			if e != nil || host != "" && host != n {
				return errors.New("h2 authority mismatch")
			}
			host = n
		case ":path":
			path = f.Value
		case ":method":
			method = f.Value
		case ":scheme":
			scheme = f.Value
		case "connection", "upgrade", "transfer-encoding":
			return errors.New("invalid h2 connection header")
		}
	}
	// Request trailers cannot introduce routing metadata.
	if trailers {
		if len(seen) != 0 || host != "" || !endStream {
			return errors.New("routing metadata in h2 trailer")
		}
	} else {
		if method == "" || host == "" || stream <= r.lastStream || len(r.streams) >= 4096 {
			return errors.New("invalid initial h2 request or stream capacity")
		}
		if method != "CONNECT" && (path == "" || scheme != "http" && scheme != "https") {
			return errors.New("h2 path and scheme required")
		}
		raw, _, _ := strings.Cut(path, "?")
		if method == "CONNECT" {
			raw = "/"
		}
		if e := CheckHTTP(r.layers, host, raw, method == "CONNECT"); e != nil {
			return e
		}
		r.lastStream = stream
	}
	if endStream {
		delete(r.streams, stream)
	} else {
		r.streams[stream] = true
	}
	r.pending = bytes.Clone(frames)
	return nil
}
