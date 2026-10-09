//go:build !linux

package agent

import "net"

type udpBatchReader struct {
	conn   *net.UDPConn
	buffer []byte
}

func newUDPBatchReader(c *net.UDPConn, _ *udpCounters) *udpBatchReader {
	return &udpBatchReader{conn: c, buffer: make([]byte, 65535)}
}
func (r *udpBatchReader) Read() ([]udpIOPacket, error) {
	n, a, e := r.conn.ReadFromUDP(r.buffer)
	if e != nil {
		return nil, e
	}
	return []udpIOPacket{{payload: r.buffer[:n], peer: a}}, nil
}
func writeUDPBatch(c *net.UDPConn, packets []udpIOPacket) (int, error) {
	for i, p := range packets {
		var e error
		if p.peer == nil {
			_, e = c.Write(p.payload)
		} else {
			_, e = c.WriteToUDP(p.payload, p.peer)
		}
		if e != nil {
			return i, e
		}
	}
	return len(packets), nil
}
