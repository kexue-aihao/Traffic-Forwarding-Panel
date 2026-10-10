package policy

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

type inspectionMemoryConn struct {
	r *bytes.Reader
	w bytes.Buffer
}

func (c *inspectionMemoryConn) Read(p []byte) (int, error)     { return c.r.Read(p) }
func (c *inspectionMemoryConn) Write(p []byte) (int, error)    { return c.w.Write(p) }
func (*inspectionMemoryConn) Close() error                     { return nil }
func (*inspectionMemoryConn) LocalAddr() net.Addr              { return nil }
func (*inspectionMemoryConn) RemoteAddr() net.Addr             { return nil }
func (*inspectionMemoryConn) SetDeadline(time.Time) error      { return nil }
func (*inspectionMemoryConn) SetReadDeadline(time.Time) error  { return nil }
func (*inspectionMemoryConn) SetWriteDeadline(time.Time) error { return nil }

func TestUnknownInspectionReplaysAndWritesExactlyOnce(t *testing.T) {
	for _, prefix := range [][]byte{
		{5, 0, 1, 2, 3}, {4, 0, 0, 1, 0, 0, 0, 0}, {22, 3, 3, 0, 4, 0, 0, 0, 0}, {0, 4, 5, 22},
		[]byte("GET /unterminated HTTP/1.1\r\nHost: x"),
	} {
		conn := &inspectionMemoryConn{r: bytes.NewReader(prefix)}
		v, err := Inspect(conn)
		if err != nil {
			t.Fatalf("unknown inspection errored %x: %v", prefix, err)
		}
		got, err := io.ReadAll(v.Conn)
		if err != nil || !bytes.Equal(got, prefix) {
			t.Fatalf("replay %x want %x: %v", got, prefix, err)
		}
		if _, err = v.Conn.Write([]byte("reply")); err != nil || conn.w.String() != "reply" {
			t.Fatal("response swallowed", err)
		}
		if v.Kind == "socks4" || v.Kind == "socks5" {
			t.Fatalf("malformed header matched: %x", prefix)
		}
	}
}

func TestSOCKS5DatagramUsesRFC1928(t *testing.T) {
	for _, packet := range [][]byte{
		{0, 0, 0, 1, 127, 0, 0, 1, 0, 53, 'x'},
		{0, 0, 0, 3, 1, 'a', 0, 53, 'x'},
		append([]byte{0, 0, 0, 4}, append(make([]byte, 16), 0, 53, 'x')...),
	} {
		if !BlockedDatagram(packet, []string{"socks5"}) || !BlockedDatagram(packet, []string{"socks"}) {
			t.Fatalf("UDP bypass %x", packet)
		}
	}
	for _, packet := range [][]byte{{5, 1, 0}, {4, 1, 2}, {0, 0, 0, 1, 127}, {0, 0, 0, 3, 0, 0, 53}} {
		if BlockedDatagram(packet, []string{"socks"}) {
			t.Fatalf("unrelated/truncated UDP blocked %x", packet)
		}
	}
}

func TestInspectionProcessBudgetIsNotProtocolEvidence(t *testing.T) {
	for range cap(inspectionSlots) {
		inspectionSlots <- struct{}{}
	}
	defer func() {
		for len(inspectionSlots) > 0 {
			<-inspectionSlots
		}
	}()
	conn := &inspectionMemoryConn{r: bytes.NewReader([]byte{5, 1, 0})}
	v, err := Inspect(conn)
	if !errors.Is(err, ErrInspectionCapacity) || v.Detection.Reason != "inspection_capacity_exhausted" || v.Kind != "unknown" || conn.r.Len() != 3 {
		t.Fatalf("capacity treated as detection: %+v %v", v, err)
	}
}
