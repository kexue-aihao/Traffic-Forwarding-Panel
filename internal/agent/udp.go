package agent

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/policy"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/policy/detect"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/tunnel"
)

const maxUDPQueuedBytes int64 = 64 << 20
const udpIdleTimeout = 30 * time.Second

var smallUDPPackets = sync.Pool{New: func() any { return new([2048]byte) }}

type udpPacket struct {
	payload []byte
	storage *[2048]byte
	peer    *net.UDPAddr
	ruleKey string
}
type udpCounters struct {
	received, forwarded, queueDrops, quotaDrops, rateDrops, policyDrops, capacityDrops, ioDrops, kernelDrops atomic.Uint64
	readBuffer, writeBuffer                                                                                  atomic.Int64
	overflowSupported                                                                                        atomic.Bool
}
type UDPStatistics struct {
	Received          uint64 `json:"received"`
	Forwarded         uint64 `json:"forwarded"`
	QueueDrops        uint64 `json:"queue_drops"`
	QuotaDrops        uint64 `json:"quota_drops"`
	RateDrops         uint64 `json:"rate_drops"`
	PolicyDrops       uint64 `json:"policy_drops"`
	CapacityDrops     uint64 `json:"capacity_drops"`
	IODrops           uint64 `json:"io_drops"`
	KernelDrops       uint64 `json:"kernel_drops"`
	OverflowSupported bool   `json:"overflow_supported"`
	ReadBufferBytes   int64  `json:"read_buffer_bytes"`
	WriteBufferBytes  int64  `json:"write_buffer_bytes"`
}

func (r *Runtime) UDPStats() map[string]UDPStatistics {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]UDPStatistics{}
	for _, b := range r.listeners {
		if b.udp == nil {
			continue
		}
		b.mu.Lock()
		id := b.rule.ID
		b.mu.Unlock()
		c := &b.udpStats
		out[id] = UDPStatistics{Received: c.received.Load(), Forwarded: c.forwarded.Load(), QueueDrops: c.queueDrops.Load(), QuotaDrops: c.quotaDrops.Load(), RateDrops: c.rateDrops.Load(), PolicyDrops: c.policyDrops.Load(), CapacityDrops: c.capacityDrops.Load(), IODrops: c.ioDrops.Load(), KernelDrops: c.kernelDrops.Load(), OverflowSupported: c.overflowSupported.Load(), ReadBufferBytes: c.readBuffer.Load(), WriteBufferBytes: c.writeBuffer.Load()}
	}
	return out
}

func (b *binding) packet(p []byte, peer *net.UDPAddr) *udpPacket {
	size := max(len(p), 2048)
	for {
		before := b.runtime.udpQueued.Load()
		if int64(size) > maxUDPQueuedBytes-before {
			return nil
		}
		if b.runtime.udpQueued.CompareAndSwap(before, before+int64(size)) {
			break
		}
	}
	packet := &udpPacket{peer: peer}
	if len(p) <= 2048 {
		packet.storage = smallUDPPackets.Get().(*[2048]byte)
		packet.payload = packet.storage[:len(p)]
	} else {
		packet.payload = make([]byte, len(p))
	}
	copy(packet.payload, p)
	return packet
}
func (b *binding) freePacket(p *udpPacket) {
	b.runtime.udpQueued.Add(-int64(max(len(p.payload), 2048)))
	if p.storage != nil {
		smallUDPPackets.Put(p.storage)
	}
}

func (s *udpSession) stop() {
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	if s.conn != nil {
		s.conn.Close()
	}
	s.mu.Unlock()
}

func (b *binding) finishUDP(k string, s *udpSession) {
	s.stop()
	s.finishOnce.Do(func() {
		s.release()
		b.mu.Lock()
		if b.sessions[k] == s {
			delete(b.sessions, k)
		}
		b.mu.Unlock()
		if s.out != nil {
			for {
				select {
				case p := <-s.out:
					b.freePacket(p)
				default:
					return
				}
			}
		}
	})
}

func (b *binding) serveUDP() {
	b.udp.SetReadBuffer(4 << 20)
	b.udp.SetWriteBuffer(4 << 20)
	workers := make([]chan *udpPacket, 4)
	for i := range workers {
		workers[i] = make(chan *udpPacket, 64)
		go b.udpWorker(workers[i])
	}
	defer func() {
		for _, q := range workers {
			close(q)
		}
	}()
	reader := newUDPBatchReader(b.udp, &b.udpStats)
	for {
		packets, e := reader.Read()
		if e != nil {
			return
		}
		for _, incoming := range packets {
			b.udpStats.received.Add(1)
			if incoming.truncated {
				b.udpStats.ioDrops.Add(1)
				continue
			}
			p := b.packet(incoming.payload, incoming.peer)
			if p == nil {
				b.udpStats.queueDrops.Add(1)
				continue
			}
			p.ruleKey = incoming.peer.String()
			h := uint32(2166136261)
			for _, ch := range p.ruleKey {
				h = (h ^ uint32(ch)) * 16777619
			}
			select {
			case workers[h%uint32(len(workers))] <- p:
			default:
				b.freePacket(p)
				b.udpStats.queueDrops.Add(1)
			}
		}
	}
}

func (b *binding) udpWorker(queue <-chan *udpPacket) {
	for p := range queue {
		v, _, ctx, pool := b.snapshot()
		if ctx.Err() != nil {
			b.freePacket(p)
			continue
		}
		b.mu.Lock()
		plan := b.plans[v.ID]
		b.mu.Unlock()
		var detection detect.Detection
		var blockedPacket bool
		if plan.HasAssociation() {
			proof := b.runtime.Associations.Admission(v.ID, plan.Generation(), p.peer.AddrPort(), v.Target, p.payload)
			detection, blockedPacket = associatedDatagramDecision(p.payload, ruleLayers(v), plan, proof)
		} else {
			detection, blockedPacket = policy.DatagramDecision(p.payload, ruleLayers(v), plan)
		}
		if plan != nil && !plan.Empty() {
			b.recordInspection(v.ID, detection, "raw")
		}
		if blockedPacket {
			b.policyRejected(v.ID)
			b.freePacket(p)
			b.udpStats.policyDrops.Add(1)
			continue
		}
		k := p.ruleKey
		b.mu.Lock()
		s := b.sessions[k]
		if s != nil && s.ctx.Err() != nil {
			s = nil
		}
		if s == nil {
			maximum := 1024
			if v.UDP != nil && v.UDP.MaxSessions > 0 {
				maximum = v.UDP.MaxSessions
			}
			if len(b.sessions) >= maximum {
				b.mu.Unlock()
				b.freePacket(p)
				b.udpStats.capacityDrops.Add(1)
				continue
			}
			select {
			case b.runtime.udpSessionSlots <- struct{}{}:
			default:
				b.mu.Unlock()
				b.freePacket(p)
				b.udpStats.capacityDrops.Add(1)
				continue
			}
			select {
			case b.runtime.udpDialSlots <- struct{}{}:
			default:
				<-b.runtime.udpSessionSlots
				b.mu.Unlock()
				b.freePacket(p)
				b.udpStats.capacityDrops.Add(1)
				continue
			}
			release, ok := pool.acquire(p.peer)
			if !ok {
				<-b.runtime.udpDialSlots
				<-b.runtime.udpSessionSlots
				b.mu.Unlock()
				b.freePacket(p)
				b.udpStats.capacityDrops.Add(1)
				continue
			}
			poolRelease := release
			release = func() { poolRelease(); <-b.runtime.udpSessionSlots }
			sctx, cancel := context.WithCancel(ctx)
			s = &udpSession{ctx: sctx, cancel: cancel, pool: pool, release: release, peer: p.peer, rule: v, plan: plan, out: make(chan *udpPacket, 64)}
			s.activity.Store(time.Now().UnixNano())
			b.sessions[k] = s
			go b.openUDP(k, s)
		}
		s.mu.Lock()
		pending := s.conn == nil
		s.mu.Unlock()
		if s.ctx.Err() != nil || pending && len(s.out) >= 8 {
			b.mu.Unlock()
			b.freePacket(p)
			b.udpStats.queueDrops.Add(1)
			continue
		}
		select {
		case s.out <- p:
		default:
			b.freePacket(p)
			b.udpStats.queueDrops.Add(1)
		}
		b.mu.Unlock()
	}
}

func (b *binding) openUDP(k string, s *udpSession) {
	conn, legacy, e := b.dial(s.ctx, s.rule)
	<-b.runtime.udpDialSlots
	if e != nil {
		b.udpStats.ioDrops.Add(1)
		b.finishUDP(k, s)
		return
	}
	s.mu.Lock()
	s.conn = conn
	s.tunnel = legacy
	cancelled := s.ctx.Err() != nil
	s.mu.Unlock()
	if cancelled {
		b.finishUDP(k, s)
		return
	}
	go b.sendUDP(k, s)
	go b.receiveUDP(k, s)
}

func (b *binding) chargeUDPPacket(s *udpSession, up bool, n int) bool {
	e := b.charge(s.ctx, s.pool, s.rule, up, n)
	if e == nil {
		return true
	}
	if errors.Is(e, errRateDrop) {
		b.udpStats.rateDrops.Add(1)
	} else if transientMeterError(e) {
		b.udpStats.quotaDrops.Add(1)
	} else {
		b.udpStats.ioDrops.Add(1)
		s.stop()
	}
	return false
}

func (b *binding) sendUDP(k string, s *udpSession) {
	defer b.finishUDP(k, s)
	for {
		var first *udpPacket
		select {
		case <-s.ctx.Done():
			return
		case first = <-s.out:
		}
		batch := []*udpPacket{first}
		if _, ok := s.conn.(*net.UDPConn); ok {
		gather:
			for len(batch) < 16 {
				select {
				case p := <-s.out:
					batch = append(batch, p)
				default:
					break gather
				}
			}
		}
		out := make([]udpIOPacket, 0, len(batch))
		for _, p := range batch {
			if d, ok := s.conn.(*tunnel.DatagramSession); ok {
				_, e := d.WriteAuthorized(p.payload, func() error { return b.charge(s.ctx, s.pool, s.rule, true, len(p.payload)) })
				if e != nil {
					if errors.Is(e, tunnel.ErrDatagramQueueFull) {
						b.udpStats.queueDrops.Add(1)
					} else if errors.Is(e, errRateDrop) {
						b.udpStats.rateDrops.Add(1)
					} else if transientMeterError(e) {
						b.udpStats.quotaDrops.Add(1)
					} else {
						s.stop()
						b.udpStats.ioDrops.Add(1)
					}
				} else {
					b.udpStats.forwarded.Add(1)
					s.activity.Store(time.Now().UnixNano())
				}
			} else if b.chargeUDPPacket(s, true, len(p.payload)) {
				out = append(out, udpIOPacket{payload: p.payload})
			}
		}
		if len(out) > 0 {
			s.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if raw, ok := s.conn.(*net.UDPConn); ok {
				n, e := writeUDPBatch(raw, out)
				b.udpStats.forwarded.Add(uint64(n))
				if e != nil || n < len(out) {
					b.udpStats.ioDrops.Add(uint64(len(out) - n))
				}
				if n > 0 {
					s.activity.Store(time.Now().UnixNano())
				}
			} else {
				for _, p := range out {
					var e error
					if s.tunnel != nil {
						e = s.tunnel.WritePacket(p.payload)
					} else {
						_, e = s.conn.Write(p.payload)
					}
					if e != nil {
						s.stop()
						b.udpStats.ioDrops.Add(1)
						break
					}
					b.udpStats.forwarded.Add(1)
					s.activity.Store(time.Now().UnixNano())
				}
			}
		}
		for _, p := range batch {
			b.freePacket(p)
		}
	}
}

func (b *binding) receiveUDP(k string, s *udpSession) {
	defer b.finishUDP(k, s)
	var reader *udpBatchReader
	if raw, ok := s.conn.(*net.UDPConn); ok {
		reader = newUDPBatchReader(raw, &b.udpStats)
	}
	var buf []byte
	if reader == nil {
		buf = make([]byte, 65535)
	}
	for {
		s.conn.SetReadDeadline(time.Unix(0, s.activity.Load()).Add(udpIdleTimeout))
		var packets []udpIOPacket
		var e error
		if reader != nil {
			packets, e = reader.Read()
		} else {
			var p []byte
			if s.tunnel != nil {
				p, e = s.tunnel.ReadPacket()
			} else {
				var n int
				n, e = s.conn.Read(buf)
				p = buf[:n]
			}
			packets = []udpIOPacket{{payload: p}}
		}
		if e != nil {
			if ne, ok := e.(net.Error); ok && ne.Timeout() && time.Since(time.Unix(0, s.activity.Load())) < udpIdleTimeout && s.ctx.Err() == nil {
				continue
			}
			return
		}
		out := make([]udpIOPacket, 0, len(packets))
		for _, p := range packets {
			if p.truncated {
				b.udpStats.ioDrops.Add(1)
				continue
			}
			if b.chargeUDPPacket(s, false, len(p.payload)) {
				s.activity.Store(time.Now().UnixNano())
				p.peer = s.peer
				out = append(out, p)
			}
		}
		if len(out) > 0 {
			n, e := writeUDPBatch(b.udp, out)
			if e != nil || n < len(out) {
				b.udpStats.ioDrops.Add(uint64(len(out) - n))
			}
			b.udpStats.forwarded.Add(uint64(n))
		}
	}
}
