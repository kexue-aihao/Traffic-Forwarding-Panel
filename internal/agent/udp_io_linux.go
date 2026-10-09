//go:build linux

package agent

import (
	"encoding/binary"
	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"
	"net"
	"syscall"
)

type udpBatchReader struct {
	conn         *ipv4.PacketConn
	messages     []ipv4.Message
	packets      []udpIOPacket
	stats        *udpCounters
	lastOverflow uint32
}

func newUDPBatchReader(c *net.UDPConn, stats *udpCounters) *udpBatchReader {
	batch := 16
	if c.RemoteAddr() != nil {
		batch = 4
	}
	if raw, e := c.SyscallConn(); e == nil {
		raw.Control(func(fd uintptr) {
			if unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RXQ_OVFL, 1) == nil {
				stats.overflowSupported.Store(true)
			}
			if c.RemoteAddr() == nil {
				if n, e := unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF); e == nil {
					stats.readBuffer.Store(int64(n))
				}
				if n, e := unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_SNDBUF); e == nil {
					stats.writeBuffer.Store(int64(n))
				}
			}
		})
	}
	r := &udpBatchReader{conn: ipv4.NewPacketConn(c), messages: make([]ipv4.Message, batch), packets: make([]udpIOPacket, batch), stats: stats}
	for i := range r.messages {
		r.messages[i].Buffers = [][]byte{make([]byte, 65535)}
		r.messages[i].OOB = make([]byte, unix.CmsgSpace(4))
	}
	return r
}
func (r *udpBatchReader) Read() ([]udpIOPacket, error) {
	n, e := r.conn.ReadBatch(r.messages, 0)
	if e != nil {
		return nil, e
	}
	for i := 0; i < n; i++ {
		m := r.messages[i]
		if controls, e := unix.ParseSocketControlMessage(m.OOB[:m.NN]); e == nil {
			for _, control := range controls {
				if control.Header.Level == unix.SOL_SOCKET && control.Header.Type == unix.SO_RXQ_OVFL && len(control.Data) >= 4 {
					count := binary.NativeEndian.Uint32(control.Data[:4])
					r.stats.kernelDrops.Add(uint64(count - r.lastOverflow))
					r.lastOverflow = count
				}
			}
		}
		a, _ := m.Addr.(*net.UDPAddr)
		r.packets[i] = udpIOPacket{payload: m.Buffers[0][:min(m.N, 65535)], peer: a, truncated: m.Flags&syscall.MSG_TRUNC != 0}
	}
	return r.packets[:n], nil
}
func writeUDPBatch(c *net.UDPConn, packets []udpIOPacket) (int, error) {
	messages := make([]ipv4.Message, len(packets))
	for i, p := range packets {
		messages[i] = ipv4.Message{Buffers: [][]byte{p.payload}}
		if p.peer != nil {
			messages[i].Addr = p.peer
		}
	}
	return ipv4.NewPacketConn(c).WriteBatch(messages, 0)
}
