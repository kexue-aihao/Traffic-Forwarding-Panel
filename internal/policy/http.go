package policy

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"golang.org/x/net/http/httpguts"
)

type header struct {
	host, path      string
	length          int64
	chunked, opaque bool
}

func readLine(r *bufio.Reader, limit int) ([]byte, error) {
	var b []byte
	for {
		p, e := r.ReadSlice('\n')
		if len(b)+len(p) > limit {
			return nil, errors.New("HTTP line too large")
		}
		b = append(b, p...)
		if e == bufio.ErrBufferFull {
			continue
		}
		if e != nil {
			return b, e
		}
		if len(b) < 2 || b[len(b)-2] != '\r' {
			return nil, errors.New("HTTP requires CRLF")
		}
		return b, nil
	}
}
func readHeader(r *bufio.Reader) ([]byte, error) {
	return readHeaderLimit(r, HeaderLimit)
}
func readHeaderLimit(r *bufio.Reader, limit int) ([]byte, error) {
	var b []byte
	for {
		p, e := readLine(r, limit-len(b))
		if e != nil {
			return nil, e
		}
		b = append(b, p...)
		if bytes.Equal(p, []byte("\r\n")) {
			return b, nil
		}
	}
}
func parseHeader(b []byte) (header, error) {
	v := header{}
	lines := strings.Split(string(b), "\r\n")
	if len(lines) < 3 {
		return v, errors.New("invalid HTTP header")
	}
	start := strings.Split(lines[0], " ")
	if len(start) != 3 || start[0] == "" || (start[2] != "HTTP/1.1" && start[2] != "HTTP/1.0") {
		return v, errors.New("invalid request line")
	}
	for _, ch := range start[0] {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", ch)) {
			return v, errors.New("invalid method")
		}
	}
	h := http.Header{}
	for _, line := range lines[1:] {
		if line == "" {
			break
		}
		if line[0] == ' ' || line[0] == '\t' {
			return v, errors.New("folded header denied")
		}
		k, x, ok := strings.Cut(line, ":")
		if !ok || k == "" || strings.ContainsAny(k, " \t\x00") {
			return v, errors.New("invalid header")
		}
		for _, c := range k {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", c)) {
				return v, errors.New("invalid header name")
			}
		}
		if !httpguts.ValidHeaderFieldValue(x) {
			return v, errors.New("invalid header value")
		}
		h.Add(k, strings.TrimSpace(x))
	}
	if len(h.Values("Host")) != 1 {
		return v, errors.New("one HTTP Host required")
	}
	var e error
	v.host, e = Host(h.Get("Host"))
	if e != nil {
		return v, e
	}
	v.opaque = start[0] == "CONNECT" || h.Get("Upgrade") != ""
	if start[0] == "CONNECT" {
		authority, e := Host(start[1])
		if e != nil || authority != v.host {
			return v, errors.New("CONNECT authority mismatch")
		}
		v.path = "/"
	} else {
		u, e := url.ParseRequestURI(start[1])
		if e != nil {
			return v, e
		}
		if u.Fragment != "" || u.User != nil {
			return v, errors.New("invalid request URI")
		}
		if u.IsAbs() {
			authority, e := Host(u.Host)
			if e != nil || authority != v.host {
				return v, errors.New("absolute URI authority mismatch")
			}
		}
		v.path = u.EscapedPath()
		if v.path == "" {
			v.path = "/"
		}
	}
	cl, te := h.Values("Content-Length"), h.Values("Transfer-Encoding")
	if len(cl) > 1 || len(te) > 1 || len(cl) > 0 && len(te) > 0 {
		return v, errors.New("ambiguous HTTP framing")
	}
	if len(cl) == 1 {
		if cl[0] == "" {
			return v, errors.New("empty content length")
		}
		for _, c := range cl[0] {
			if c < '0' || c > '9' {
				return v, errors.New("invalid content length")
			}
		}
		v.length, e = strconv.ParseInt(cl[0], 10, 64)
		if e != nil {
			return v, e
		}
	}
	if len(te) == 1 {
		if !strings.EqualFold(te[0], "chunked") {
			return v, errors.New("unsupported transfer encoding")
		}
		v.chunked = true
	}
	return v, nil
}

// Headers are released only after validation. Bodies stream in caller-sized
// pieces; buffering is bounded independently of Content-Length/chunk length.
type httpReader struct {
	r                                    *bufio.Reader
	layers                               []contract.InboundPolicy
	pending                              []byte
	left                                 int64
	chunked, chunkCRLF, trailers, opaque bool
	err                                  error
}

func newHTTPReader(r io.Reader, p []contract.InboundPolicy) *httpReader {
	return &httpReader{r: bufio.NewReaderSize(r, HeaderLimit+1), layers: p}
}
func (r *httpReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(r.pending) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		if r.opaque {
			return r.r.Read(p)
		}
		if r.left > 0 {
			n, e := r.r.Read(p[:min(int64(len(p)), r.left)])
			r.left -= int64(n)
			if e != nil {
				r.err = e
			}
			return n, e
		}
		if r.chunked {
			if r.chunkCRLF {
				var crlf [2]byte
				_, e := io.ReadFull(r.r, crlf[:])
				if e != nil || string(crlf[:]) != "\r\n" {
					r.err = errors.New("invalid chunk terminator")
					continue
				}
				r.pending = crlf[:]
				r.chunkCRLF = false
				continue
			}
			if r.trailers {
				b, e := readHeader(r.r)
				if e != nil {
					r.err = e
					continue
				}
				for _, line := range strings.Split(string(b), "\r\n") {
					if line == "" {
						break
					}
					name, value, ok := strings.Cut(line, ":")
					if !ok || !httpguts.ValidHeaderFieldName(name) || !httpguts.ValidHeaderFieldValue(value) || strings.EqualFold(name, "Host") || strings.EqualFold(name, "Content-Length") || strings.EqualFold(name, "Transfer-Encoding") || strings.EqualFold(name, "Connection") || strings.EqualFold(name, "Upgrade") || strings.EqualFold(name, "Trailer") {
						r.err = errors.New("invalid trailer")
						break
					}
				}
				if r.err != nil {
					continue
				}
				r.pending = b
				r.chunked = false
				r.trailers = false
				continue
			}
			b, e := readLine(r.r, 4096)
			if e != nil {
				r.err = e
				continue
			}
			s := strings.TrimSuffix(string(b), "\r\n")
			size, extensions, extended := strings.Cut(s, ";")
			if size == "" || strings.ContainsAny(size, " +-\t") {
				r.err = errors.New("invalid chunk size")
				continue
			}
			if extended && !validChunkExtensions(";"+extensions) {
				r.err = errors.New("invalid chunk extension")
				continue
			}
			n, e := strconv.ParseUint(size, 16, 63)
			if e != nil {
				r.err = e
				continue
			}
			r.pending = b
			r.left = int64(n)
			r.chunkCRLF = n > 0
			r.trailers = n == 0
			continue
		}
		b, e := readHeader(r.r)
		if e != nil {
			r.err = e
			continue
		}
		h, e := parseHeader(b)
		if e == nil {
			e = CheckHTTP(r.layers, h.host, h.path, h.opaque)
		}
		if e != nil {
			r.err = fmt.Errorf("HTTP policy: %w", e)
			continue
		}
		r.pending = b
		r.left = h.length
		r.chunked = h.chunked
		r.opaque = h.opaque
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func validChunkExtensions(s string) bool {
	for {
		s = strings.TrimLeft(s, " \t")
		if s == "" {
			return true
		}
		if s[0] != ';' {
			return false
		}
		s = strings.TrimLeft(s[1:], " \t")
		n := strings.IndexAny(s, "=; \t")
		if n < 0 {
			return httpguts.ValidHeaderFieldName(s)
		}
		if !httpguts.ValidHeaderFieldName(s[:n]) {
			return false
		}
		s = strings.TrimLeft(s[n:], " \t")
		if s == "" || s[0] != '=' {
			continue
		}
		s = strings.TrimLeft(s[1:], " \t")
		if s == "" {
			return false
		}
		if s[0] != '"' {
			n = strings.IndexAny(s, "; \t")
			if n < 0 {
				return httpguts.ValidHeaderFieldName(s)
			}
			if !httpguts.ValidHeaderFieldName(s[:n]) {
				return false
			}
			s = s[n:]
			continue
		}
		closed := false
		for i := 1; i < len(s); i++ {
			if s[i] == '"' {
				if !httpguts.ValidHeaderFieldValue(s[1:i]) {
					return false
				}
				s = s[i+1:]
				closed = true
				break
			}
			if s[i] == '\\' {
				i++
				if i >= len(s) {
					return false
				}
			}
		}
		if !closed {
			return false
		}
	}
}
