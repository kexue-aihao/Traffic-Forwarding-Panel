package agent

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

var proxySignature = []byte("\r\n\r\n\x00\r\nQUIT\n")

type proxyConn struct {
	net.Conn
	reader              *bufio.Reader
	source, destination net.Addr
}

func (c *proxyConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
func (c *proxyConn) RemoteAddr() net.Addr       { return c.source }
func (c *proxyConn) LocalAddr() net.Addr        { return c.destination }
func (c *proxyConn) CloseWrite() error {
	if v, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return v.CloseWrite()
	}
	return c.Close()
}

func receiveProxy(c net.Conn, p *contract.ProxyProtocol) (net.Conn, error) {
	if p == nil || p.Accept == "" || p.Accept == "off" {
		return c, nil
	}
	peer, err := netip.ParseAddrPort(c.RemoteAddr().String())
	if err != nil {
		return nil, err
	}
	trusted := false
	for _, v := range p.TrustedCIDRs {
		prefix, e := netip.ParsePrefix(v)
		if e == nil && prefix.Contains(peer.Addr().Unmap()) {
			trusted = true
		}
	}
	if !trusted {
		return nil, errors.New("untrusted proxy")
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer c.SetReadDeadline(time.Time{})
	reader := bufio.NewReaderSize(c, 256)
	var source, dest *net.TCPAddr
	if p.Accept == "v1" {
		line := make([]byte, 0, 108)
		for len(line) < 108 {
			v, e := reader.ReadByte()
			if e != nil {
				return nil, e
			}
			line = append(line, v)
			if v == '\n' {
				break
			}
		}
		if !bytes.HasSuffix(line, []byte("\r\n")) {
			return nil, errors.New("invalid PROXY line")
		}
		fields := strings.Split(strings.TrimSuffix(string(line), "\r\n"), " ")
		if len(fields) != 6 || fields[0] != "PROXY" || (fields[1] != "TCP4" && fields[1] != "TCP6") {
			return nil, errors.New("PROXY TCP address required")
		}
		a, e := netip.ParseAddr(fields[2])
		b, e2 := netip.ParseAddr(fields[3])
		ap, e3 := strconv.Atoi(fields[4])
		bp, e4 := strconv.Atoi(fields[5])
		if e != nil || e2 != nil || e3 != nil || e4 != nil || ap < 1 || ap > 65535 || bp < 1 || bp > 65535 || a.Is4() != b.Is4() || (fields[1] == "TCP4") != a.Is4() {
			return nil, errors.New("invalid PROXY address")
		}
		source = &net.TCPAddr{IP: net.IP(a.AsSlice()), Port: ap}
		dest = &net.TCPAddr{IP: net.IP(b.AsSlice()), Port: bp}
	} else {
		h := make([]byte, 16)
		if _, err = io.ReadFull(reader, h); err != nil {
			return nil, err
		}
		size := int(binary.BigEndian.Uint16(h[14:]))
		n := 12
		if h[13] == 0x21 {
			n = 36
		} else if h[13] != 0x11 {
			return nil, errors.New("PROXY v2 TCP family required")
		}
		if !bytes.Equal(h[:12], proxySignature) || h[12] != 0x21 || size < n || size > 512 {
			return nil, errors.New("invalid PROXY v2 header")
		}
		data := make([]byte, size)
		if _, err = io.ReadFull(reader, data); err != nil {
			return nil, err
		}
		width := 4
		if n == 36 {
			width = 16
		}
		source = &net.TCPAddr{IP: net.IP(data[:width]), Port: int(binary.BigEndian.Uint16(data[2*width:]))}
		dest = &net.TCPAddr{IP: net.IP(data[width : 2*width]), Port: int(binary.BigEndian.Uint16(data[2*width+2:]))}
		if source.Port == 0 || dest.Port == 0 {
			return nil, errors.New("invalid PROXY port")
		}
	}
	return &proxyConn{Conn: c, reader: reader, source: source, destination: dest}, nil
}

func sendProxy(dst net.Conn, client net.Conn, p *contract.ProxyProtocol) error {
	if p == nil || p.Send == "" || p.Send == "off" {
		return nil
	}
	src, e := netip.ParseAddrPort(client.RemoteAddr().String())
	if e != nil {
		return e
	}
	dest, e := netip.ParseAddrPort(client.LocalAddr().String())
	if e != nil {
		return e
	}
	src = netip.AddrPortFrom(src.Addr().Unmap(), src.Port())
	dest = netip.AddrPortFrom(dest.Addr().Unmap(), dest.Port())
	if src.Addr().Is4() != dest.Addr().Is4() {
		return errors.New("PROXY family mismatch")
	}
	var data []byte
	if p.Send == "v1" {
		family := "TCP6"
		if src.Addr().Is4() {
			family = "TCP4"
		}
		data = []byte(fmt.Sprintf("PROXY %s %s %s %d %d\r\n", family, src.Addr(), dest.Addr(), src.Port(), dest.Port()))
	} else {
		family := byte(0x21)
		if src.Addr().Is4() {
			family = 0x11
		}
		address := append(src.Addr().AsSlice(), dest.Addr().AsSlice()...)
		address = binary.BigEndian.AppendUint16(address, src.Port())
		address = binary.BigEndian.AppendUint16(address, dest.Port())
		data = append(append([]byte{}, proxySignature...), 0x21, family)
		data = binary.BigEndian.AppendUint16(data, uint16(len(address)))
		data = append(data, address...)
	}
	dst.SetWriteDeadline(time.Now().Add(5 * time.Second))
	defer dst.SetWriteDeadline(time.Time{})
	for len(data) > 0 {
		n, err := dst.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}
