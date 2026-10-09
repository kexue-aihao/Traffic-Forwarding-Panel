package tunnel

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
)

const datagramALPN = "tfp-udp-datagram-v1"
const datagramHeader = 25
const maxDatagramFlows = 1024
const maxDatagramMemory int64 = 8 << 20
const fragmentTTL = time.Second

var ErrDatagramQueueFull = errors.New("UDP datagram queue full")

func datagramConfig() *quic.Config {
	return &quic.Config{EnableDatagrams: true, Allow0RTT: false, HandshakeIdleTimeout: 5 * time.Second, MaxIdleTimeout: 2 * time.Minute, MaxIncomingStreams: 1024, MaxIncomingUniStreams: -1, InitialStreamReceiveWindow: 8192, MaxStreamReceiveWindow: 8192, MaxConnectionReceiveWindow: 1 << 20}
}

type datagramOpen struct {
	Version   int       `json:"version"`
	Token     string    `json:"token"`
	Flow      uint64    `json:"flow"`
	Target    string    `json:"target"`
	ExpiresAt time.Time `json:"expires_at"`
}
type datagramJob struct {
	session *DatagramSession
	payload []byte
}
type fragments struct {
	total        int
	parts        [][]byte
	count, bytes int
	expires      time.Time
}

type datagramLink struct {
	conn       *quic.Conn
	mu         sync.Mutex
	flows      map[uint64]*DatagramSession
	send       chan datagramJob
	admission  sync.Mutex
	queued     atomic.Int64
	reassembly atomic.Int64
	received   atomic.Int64
	packetSize atomic.Int64
	drops      atomic.Uint64
	sentFrames atomic.Uint64
}

func newDatagramLink(q *quic.Conn) *datagramLink {
	l := &datagramLink{conn: q, flows: map[uint64]*DatagramSession{}, send: make(chan datagramJob, 256)}
	// quic-go starts at a 1280-byte QUIC packet and conservatively allows
	// 1243 bytes of DATAGRAM data. Keep 1200-byte business packets in one
	// frame (25-byte header); a smaller peer/path limit is handled below.
	l.packetSize.Store(1240)
	go l.receive()
	go l.sender()
	go l.collect()
	return l
}

func (l *datagramLink) add(ctx context.Context, flow uint64, control *quic.Stream) (*DatagramSession, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if flow == 0 || l.flows[flow] != nil || len(l.flows) >= maxDatagramFlows || l.conn.Context().Err() != nil {
		return nil, errors.New("datagram flow capacity or identity invalid")
	}
	sctx, cancel := context.WithCancel(ctx)
	s := &DatagramSession{link: l, flow: flow, control: control, ctx: sctx, cancel: cancel, in: make(chan []byte, 32), wake: make(chan struct{}), parts: map[uint64]*fragments{}, seen: map[uint64]time.Time{}}
	l.flows[flow] = s
	return s, nil
}

func takeMemory(counter *atomic.Int64, n int) bool {
	for {
		old := counter.Load()
		if int64(n) > maxDatagramMemory-old {
			return false
		}
		if counter.CompareAndSwap(old, old+int64(n)) {
			return true
		}
	}
}

func (l *datagramLink) receive() {
	defer func() {
		l.mu.Lock()
		sessions := make([]*DatagramSession, 0, len(l.flows))
		for _, s := range l.flows {
			sessions = append(sessions, s)
		}
		l.mu.Unlock()
		for _, s := range sessions {
			s.Close()
		}
	}()
	for {
		p, e := l.conn.ReceiveDatagram(l.conn.Context())
		if e != nil {
			return
		}
		if len(p) < datagramHeader || p[0] != 1 {
			l.drops.Add(1)
			continue
		}
		flow := binary.BigEndian.Uint64(p[1:9])
		l.mu.Lock()
		s := l.flows[flow]
		l.mu.Unlock()
		if s == nil {
			l.drops.Add(1)
			continue
		}
		s.accept(p)
	}
}

func (l *datagramLink) sender() {
	for {
		select {
		case <-l.conn.Context().Done():
			// Serialize shutdown with a writer already between authorization
			// and enqueue, so its buffers cannot outlive the sender.
			l.admission.Lock()
			defer l.admission.Unlock()
			for {
				select {
				case j := <-l.send:
					l.queued.Add(-int64(len(j.payload)))
				default:
					return
				}
			}
		case j := <-l.send:
			if j.session.ctx.Err() == nil {
				if e := l.sendPacket(j.session, j.payload); e != nil {
					l.drops.Add(1)
				}
			}
			l.queued.Add(-int64(len(j.payload)))
		}
	}
}

func (l *datagramLink) sendPacket(s *DatagramSession, p []byte) error {
	// Retry a size rejection only, with a fresh packet ID. Lost business data is
	// never retransmitted. A partial old assembly expires independently.
	for attempt := 0; attempt < 2; attempt++ {
		chunk := int(l.packetSize.Load()) - datagramHeader
		if chunk < 512 {
			return errors.New("QUIC datagram MTU too small")
		}
		count := max(1, (len(p)+chunk-1)/chunk)
		id := s.sequence.Add(1)
		var sizeErr *quic.DatagramTooLargeError
		for i := 0; i < count; i++ {
			start, end := i*chunk, min(len(p), (i+1)*chunk)
			frame := make([]byte, datagramHeader+end-start)
			frame[0] = 1
			binary.BigEndian.PutUint64(frame[1:9], s.flow)
			binary.BigEndian.PutUint64(frame[9:17], id)
			binary.BigEndian.PutUint32(frame[17:21], uint32(len(p)))
			binary.BigEndian.PutUint16(frame[21:23], uint16(i))
			binary.BigEndian.PutUint16(frame[23:25], uint16(count))
			copy(frame[25:], p[start:end])
			if e := l.conn.SendDatagram(frame); e != nil {
				if errors.As(e, &sizeErr) && sizeErr.MaxDatagramPayloadSize > datagramHeader+512 {
					l.packetSize.Store(sizeErr.MaxDatagramPayloadSize)
					break
				}
				return e
			}
			l.sentFrames.Add(1)
		}
		if sizeErr == nil {
			return nil
		}
	}
	return errors.New("QUIC datagram MTU changed repeatedly")
}

func (l *datagramLink) collect() {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-l.conn.Context().Done():
			return
		case now := <-tick.C:
			l.mu.Lock()
			sessions := make([]*DatagramSession, 0, len(l.flows))
			for _, s := range l.flows {
				sessions = append(sessions, s)
			}
			l.mu.Unlock()
			for _, s := range sessions {
				s.mu.Lock()
				s.expireLocked(now)
				s.mu.Unlock()
			}
		}
	}
}

// DatagramSession is a packet-preserving net.Conn. Business bytes are carried
// exclusively by RFC 9221 DATAGRAM; the stream contains control messages only.
type DatagramSession struct {
	link                        *datagramLink
	flow                        uint64
	control                     *quic.Stream
	ctx                         context.Context
	cancel                      context.CancelFunc
	sequence                    atomic.Uint64
	in                          chan []byte
	mu                          sync.Mutex
	readMu                      sync.Mutex
	wake                        chan struct{}
	readDeadline, writeDeadline time.Time
	parts                       map[uint64]*fragments
	seen                        map[uint64]time.Time
	once                        sync.Once
}

func (s *DatagramSession) expireLocked(now time.Time) {
	for id, a := range s.parts {
		if !now.Before(a.expires) {
			delete(s.parts, id)
			s.link.reassembly.Add(-int64(a.total))
			s.link.drops.Add(1)
		}
	}
	for id, end := range s.seen {
		if !now.Before(end) {
			delete(s.seen, id)
		}
	}
}

func (s *DatagramSession) accept(frame []byte) {
	id := binary.BigEndian.Uint64(frame[9:17])
	total := int(binary.BigEndian.Uint32(frame[17:21]))
	index := int(binary.BigEndian.Uint16(frame[21:23]))
	count := int(binary.BigEndian.Uint16(frame[23:25]))
	payload := frame[25:]
	if total > 65535 || count < 1 || count > 128 || index >= count || len(payload) > total || count > 1 && len(payload) == 0 {
		s.link.drops.Add(1)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		return
	}
	now := time.Now()
	s.expireLocked(now)
	if _, ok := s.seen[id]; ok {
		return
	}
	var packet []byte
	if count == 1 {
		if len(payload) != total {
			s.link.drops.Add(1)
			return
		}
		packet = payload
	} else {
		a := s.parts[id]
		if a == nil {
			if len(s.parts) >= 4 || !takeMemory(&s.link.reassembly, total) {
				s.link.drops.Add(1)
				return
			}
			a = &fragments{total: total, parts: make([][]byte, count), expires: now.Add(fragmentTTL)}
			s.parts[id] = a
		}
		if a.total != total || len(a.parts) != count {
			s.link.drops.Add(1)
			return
		}
		if a.parts[index] != nil {
			if !bytes.Equal(a.parts[index], payload) {
				s.link.drops.Add(1)
			}
			return
		}
		if len(payload) > a.total-a.bytes {
			s.link.drops.Add(1)
			return
		}
		a.parts[index] = append([]byte(nil), payload...)
		a.count++
		a.bytes += len(payload)
		if a.count != count {
			return
		}
		delete(s.parts, id)
		s.link.reassembly.Add(-int64(a.total))
		if a.bytes != total {
			s.link.drops.Add(1)
			return
		}
		packet = make([]byte, 0, total)
		for _, part := range a.parts {
			packet = append(packet, part...)
		}
	}
	if len(s.seen) >= 256 {
		for old := range s.seen {
			delete(s.seen, old)
			break
		}
	}
	s.seen[id] = now.Add(2 * fragmentTTL)
	if !takeMemory(&s.link.received, len(packet)) {
		s.link.drops.Add(1)
		return
	}
	select {
	case s.in <- append([]byte{}, packet...):
	default:
		s.link.received.Add(-int64(len(packet)))
		s.link.drops.Add(1)
	}
}

func (s *DatagramSession) Read(p []byte) (int, error) {
	s.readMu.Lock()
	defer s.readMu.Unlock()
	for {
		s.mu.Lock()
		deadline, wake := s.readDeadline, s.wake
		s.mu.Unlock()
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return 0, os.ErrDeadlineExceeded
		}
		var timer *time.Timer
		var timeout <-chan time.Time
		if !deadline.IsZero() {
			timer = time.NewTimer(time.Until(deadline))
			timeout = timer.C
		}
		select {
		case <-s.ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return 0, net.ErrClosed
		case <-timeout:
			return 0, os.ErrDeadlineExceeded
		case <-wake:
			if timer != nil {
				timer.Stop()
			}
			continue
		case packet := <-s.in:
			if timer != nil {
				timer.Stop()
			}
			s.link.received.Add(-int64(len(packet)))
			return copy(p, packet), nil
		}
	}
}

func (s *DatagramSession) Write(p []byte) (int, error) { return s.WriteAuthorized(p, nil) }

// Admission is bounded and precedes quota debit. Blocking in quic-go's send
// queue is isolated in the connection sender, never in a UDP receive loop.
func (s *DatagramSession) WriteAuthorized(p []byte, authorize func() error) (int, error) {
	if len(p) > 65535 {
		return 0, errors.New("UDP payload exceeds platform packet buffer")
	}
	s.link.admission.Lock()
	defer s.link.admission.Unlock()
	if s.ctx.Err() != nil || s.link.conn.Context().Err() != nil {
		return 0, net.ErrClosed
	}
	s.mu.Lock()
	deadline := s.writeDeadline
	s.mu.Unlock()
	if !deadline.IsZero() && !time.Now().Before(deadline) {
		return 0, os.ErrDeadlineExceeded
	}
	if len(s.link.send) == cap(s.link.send) || !takeMemory(&s.link.queued, len(p)) {
		return 0, ErrDatagramQueueFull
	}
	if authorize != nil {
		if e := authorize(); e != nil {
			s.link.queued.Add(-int64(len(p)))
			return 0, e
		}
	}
	s.link.send <- datagramJob{session: s, payload: append([]byte{}, p...)}
	return len(p), nil
}

func (s *DatagramSession) Close() error {
	s.once.Do(func() {
		s.cancel()
		s.control.CancelRead(0)
		s.control.CancelWrite(0)
		s.link.mu.Lock()
		delete(s.link.flows, s.flow)
		s.link.mu.Unlock()
		s.mu.Lock()
		for _, a := range s.parts {
			s.link.reassembly.Add(-int64(a.total))
		}
		s.parts = map[uint64]*fragments{}
		s.mu.Unlock()
		for {
			select {
			case p := <-s.in:
				s.link.received.Add(-int64(len(p)))
			default:
				return
			}
		}
	})
	return nil
}
func (s *DatagramSession) LocalAddr() net.Addr  { return s.link.conn.LocalAddr() }
func (s *DatagramSession) RemoteAddr() net.Addr { return s.link.conn.RemoteAddr() }
func (s *DatagramSession) SetReadDeadline(t time.Time) error {
	s.mu.Lock()
	s.readDeadline = t
	close(s.wake)
	s.wake = make(chan struct{})
	s.mu.Unlock()
	return nil
}
func (s *DatagramSession) SetWriteDeadline(t time.Time) error {
	s.mu.Lock()
	s.writeDeadline = t
	s.mu.Unlock()
	return nil
}
func (s *DatagramSession) SetDeadline(t time.Time) error {
	s.SetReadDeadline(t)
	return s.SetWriteDeadline(t)
}

type datagramPoolEntry struct {
	ready chan struct{}
	link  *datagramLink
	err   error
}
type DatagramPool struct {
	mu      sync.Mutex
	entries map[string]*datagramPoolEntry
	closed  bool
}

type DatagramStatistics struct {
	Connections        int    `json:"connections"`
	Flows              int    `json:"flows"`
	SubmittedFrames    uint64 `json:"submitted_frames"`
	ApplicationDrops   uint64 `json:"application_drops"`
	QueuedBytes        int64  `json:"queued_bytes"`
	ReassemblyBytes    int64  `json:"reassembly_bytes"`
	ReceivedQueueBytes int64  `json:"received_queue_bytes"`
	MinimumFrameSize   int64  `json:"minimum_frame_size"`
}

// Stats exposes bounded carrier counters without endpoints or credentials.
// Submitted frames are accepted by quic-go, not confirmed wire delivery.
func (p *DatagramPool) Stats() DatagramStatistics {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := DatagramStatistics{}
	for _, entry := range p.entries {
		l := entry.link
		if l == nil || l.conn.Context().Err() != nil {
			continue
		}
		out.Connections++
		l.mu.Lock()
		out.Flows += len(l.flows)
		l.mu.Unlock()
		out.SubmittedFrames += l.sentFrames.Load()
		out.ApplicationDrops += l.drops.Load()
		out.QueuedBytes += l.queued.Load()
		out.ReassemblyBytes += l.reassembly.Load()
		out.ReceivedQueueBytes += l.received.Load()
		if n := l.packetSize.Load(); out.MinimumFrameSize == 0 || n < out.MinimumFrameSize {
			out.MinimumFrameSize = n
		}
	}
	return out
}

func (p *DatagramPool) Dial(ctx context.Context, base *tls.Config, endpoint, serverName, token, target string) (*DatagramSession, error) {
	if len(token) < 16 || len(token) > 128 {
		return nil, errors.New("datagram token must contain 16-128 characters")
	}
	host, _, e := net.SplitHostPort(endpoint)
	if e != nil {
		return nil, e
	}
	tc := &tls.Config{MinVersion: tls.VersionTLS13}
	if base != nil {
		tc = base.Clone()
	}
	if tc.InsecureSkipVerify {
		return nil, errors.New("certificate verification cannot be disabled")
	}
	tc.MinVersion = tls.VersionTLS13
	tc.NextProtos = []string{datagramALPN}
	tc.ServerName = serverName
	if tc.ServerName == "" {
		tc.ServerName = host
	}
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	k := endpoint + "\x00" + tc.ServerName + "\x00" + token
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, net.ErrClosed
	}
	if p.entries == nil {
		p.entries = map[string]*datagramPoolEntry{}
	}
	for key, cached := range p.entries {
		if cached.link != nil && cached.link.conn.Context().Err() != nil {
			delete(p.entries, key)
		}
	}
	entry := p.entries[k]
	if entry != nil && entry.link != nil && entry.link.conn.Context().Err() != nil {
		delete(p.entries, k)
		entry = nil
	}
	owner := entry == nil
	if owner {
		if len(p.entries) >= 64 {
			p.mu.Unlock()
			return nil, errors.New("datagram endpoint pool full")
		}
		entry = &datagramPoolEntry{ready: make(chan struct{})}
		p.entries[k] = entry
	}
	p.mu.Unlock()
	if owner {
		q, e := quic.DialAddr(dialCtx, endpoint, tc, datagramConfig())
		if e == nil && !q.ConnectionState().SupportsDatagrams.Remote {
			q.CloseWithError(1, "DATAGRAM required")
			e = errors.New("exit did not negotiate DATAGRAM")
		}
		p.mu.Lock()
		entry.err = e
		if e == nil {
			entry.link = newDatagramLink(q)
			if p.closed {
				q.CloseWithError(0, "closed")
				entry.err = net.ErrClosed
			}
		} else {
			delete(p.entries, k)
		}
		close(entry.ready)
		p.mu.Unlock()
	} else {
		select {
		case <-entry.ready:
		case <-dialCtx.Done():
			return nil, dialCtx.Err()
		}
	}
	if entry.err != nil {
		return nil, entry.err
	}
	stream, e := entry.link.conn.OpenStreamSync(dialCtx)
	if e != nil {
		return nil, e
	}
	deadline, _ := dialCtx.Deadline()
	stream.SetDeadline(deadline)
	var random [8]byte
	if _, e = rand.Read(random[:]); e != nil {
		stream.CancelWrite(1)
		return nil, e
	}
	flow := binary.BigEndian.Uint64(random[:])
	if flow == 0 {
		flow = 1
	}
	s, e := entry.link.add(ctx, flow, stream)
	if e != nil {
		stream.CancelRead(1)
		stream.CancelWrite(1)
		return nil, e
	}
	ok := false
	defer func() {
		if !ok {
			s.Close()
		}
	}()
	request := datagramOpen{Version: 1, Token: token, Flow: flow, Target: target, ExpiresAt: time.Now().UTC().Add(time.Hour)}
	payload, _ := json.Marshal(request)
	if e = writeFrame(stream, openFrame, payload); e != nil {
		return nil, e
	}
	kind, response, e := readFrame(stream)
	if e != nil {
		return nil, e
	}
	if kind != readyFrame || string(response) != "ok" {
		return nil, errors.New("datagram exit rejected session")
	}
	stream.SetDeadline(time.Time{})
	ok = true
	go func() { var b [1]byte; stream.Read(b[:]); s.Close() }()
	go func() {
		select {
		case <-ctx.Done():
			s.Close()
		case <-s.ctx.Done():
		}
	}()
	return s, nil
}

func (p *DatagramPool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	for _, entry := range p.entries {
		if entry.link != nil {
			entry.link.conn.CloseWithError(0, "closed")
		}
	}
}

type DatagramServer struct {
	TLS         *tls.Config
	Token       string
	IdleTimeout time.Duration
	mu          sync.Mutex
	listener    *quic.Listener
	conns       map[*quic.Conn]struct{}
	closed      bool
	slots       chan struct{}
	targets     chan struct{}
	flows       chan struct{}
}

// Listen binds synchronously so a combined TCP/UDP exit can fail startup before
// advertising either listener as ready.
func (s *DatagramServer) Listen(address string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.listener != nil {
		return errors.New("datagram server already bound or closed")
	}
	if s.TLS == nil || (len(s.TLS.Certificates) == 0 && s.TLS.GetCertificate == nil) || len(s.Token) < 16 || len(s.Token) > 128 {
		return errors.New("datagram certificate and token (16-128 chars) required")
	}
	tc := s.TLS.Clone()
	tc.MinVersion = tls.VersionTLS13
	tc.NextProtos = []string{datagramALPN}
	l, e := quic.ListenAddr(address, tc, datagramConfig())
	if e != nil {
		return e
	}
	s.listener = l
	s.conns = map[*quic.Conn]struct{}{}
	s.slots = make(chan struct{}, 64)
	s.targets = make(chan struct{}, 32)
	s.flows = make(chan struct{}, 1024)
	return nil
}

func (s *DatagramServer) Serve(address string) error {
	if e := s.Listen(address); e != nil {
		return e
	}
	return s.ServeBound()
}

// ServeBound accepts sessions on a successfully bound listener. Call once.
func (s *DatagramServer) ServeBound() error {
	s.mu.Lock()
	l := s.listener
	s.mu.Unlock()
	if l == nil {
		return errors.New("datagram listener is not bound")
	}
	defer s.Close()
	for {
		q, e := l.Accept(context.Background())
		if e != nil {
			return e
		}
		select {
		case s.slots <- struct{}{}:
			go s.handle(q)
		default:
			q.CloseWithError(1, "capacity")
		}
	}
}
func (s *DatagramServer) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}
func (s *DatagramServer) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for q := range s.conns {
		q.CloseWithError(0, "closed")
	}
	if s.listener != nil {
		return s.listener.Close()
	}
	return nil
}
func (s *DatagramServer) handle(q *quic.Conn) {
	defer func() { <-s.slots; q.CloseWithError(0, "closed"); s.mu.Lock(); delete(s.conns, q); s.mu.Unlock() }()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.conns[q] = struct{}{}
	s.mu.Unlock()
	if !q.ConnectionState().SupportsDatagrams.Remote {
		return
	}
	l := newDatagramLink(q)
	for {
		st, e := q.AcceptStream(q.Context())
		if e != nil {
			return
		}
		select {
		case s.targets <- struct{}{}:
			go s.open(l, st)
		default:
			st.CancelRead(1)
			st.CancelWrite(1)
		}
	}
}
func (s *DatagramServer) open(l *datagramLink, st *quic.Stream) {
	released := false
	release := func() {
		if !released {
			<-s.targets
			released = true
		}
	}
	defer release()
	st.SetDeadline(time.Now().Add(10 * time.Second))
	ok := false
	defer func() {
		if !ok {
			st.CancelRead(1)
			st.CancelWrite(1)
		}
	}()
	kind, p, e := readFrame(st)
	if e != nil || kind != openFrame || len(p) > 4096 {
		return
	}
	var req datagramOpen
	dec := json.NewDecoder(bytes.NewReader(p))
	dec.DisallowUnknownFields()
	if dec.Decode(&req) != nil || dec.Decode(new(any)) != io.EOF || req.Version != 1 || req.Flow == 0 || subtle.ConstantTimeCompare([]byte(req.Token), []byte(s.Token)) != 1 || !time.Now().Before(req.ExpiresAt) || req.ExpiresAt.After(time.Now().Add(time.Hour+time.Second)) {
		return
	}
	host, port, e := net.SplitHostPort(req.Target)
	pn, _ := strconv.Atoi(port)
	if e != nil || host == "" || pn < 1 || pn > 65535 {
		return
	}
	ctx, cancel := context.WithDeadline(l.conn.Context(), req.ExpiresAt)
	defer cancel()
	select {
	case s.flows <- struct{}{}:
		defer func() { <-s.flows }()
	default:
		return
	}
	target, e := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "udp", req.Target)
	if e != nil {
		return
	}
	defer target.Close()
	flow, e := l.add(ctx, req.Flow, st)
	if e != nil {
		return
	}
	defer flow.Close()
	if writeFrame(st, readyFrame, []byte("ok")) != nil {
		return
	}
	st.SetDeadline(time.Time{})
	release()
	ok = true
	go func() { var b [1]byte; st.Read(b[:]); flow.Close(); target.Close() }()
	idle := s.IdleTimeout
	if idle == 0 {
		idle = 2 * time.Minute
	}
	var activity atomic.Int64
	activity.Store(time.Now().UnixNano())
	go func() {
		defer target.Close()
		buf := make([]byte, 65535)
		for {
			flow.SetReadDeadline(time.Now().Add(idle))
			n, e := flow.Read(buf)
			if e != nil {
				if ne, ok := e.(net.Error); ok && ne.Timeout() && time.Since(time.Unix(0, activity.Load())) < idle && ctx.Err() == nil {
					continue
				}
				return
			}
			target.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if _, e = target.Write(buf[:n]); e != nil {
				return
			}
			activity.Store(time.Now().UnixNano())
		}
	}()
	buf := make([]byte, 65535)
	for {
		target.SetReadDeadline(time.Now().Add(idle))
		n, e := target.Read(buf)
		if e != nil {
			if ne, ok := e.(net.Error); ok && ne.Timeout() && time.Since(time.Unix(0, activity.Load())) < idle && ctx.Err() == nil {
				continue
			}
			return
		}
		activity.Store(time.Now().UnixNano())
		if _, e = flow.Write(buf[:n]); e != nil && !errors.Is(e, ErrDatagramQueueFull) {
			return
		}
	}
}
