package agent

import "net"

type udpIOPacket struct {
	payload   []byte
	peer      *net.UDPAddr
	truncated bool
}
