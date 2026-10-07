package tunnel

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"math/big"
	"net"
	"sync"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

// Obfuscator defines pluggable obfuscation strategies
type Obfuscator interface {
	ObfuscateWrite(plaintext []byte) (obfuscated []byte, err error)
	DeobfuscateRead(obfuscated []byte) (plaintext []byte, err error)
	Overhead() int
	Close() error
}

// obfsConn wraps net.Conn with transparent obfuscation between TLS and Session layers
type obfsConn struct {
	net.Conn
	strategy Obfuscator
	readMu   sync.Mutex
	writeMu  sync.Mutex
	readBuf  []byte
}

const maxObfuscatedFrame = 1 << 20

func (c *obfsConn) Read(p []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	for len(c.readBuf) == 0 {
		var header [4]byte
		if _, err := io.ReadFull(c.Conn, header[:]); err != nil {
			return 0, err
		}
		n := binary.BigEndian.Uint32(header[:])
		if n == 0 || n > maxObfuscatedFrame {
			return 0, errors.New("invalid obfuscation frame length")
		}
		frame := make([]byte, n)
		if _, err := io.ReadFull(c.Conn, frame); err != nil {
			return 0, err
		}
		plain, err := c.strategy.DeobfuscateRead(frame)
		if err != nil {
			return 0, err
		}
		if len(plain) == 0 {
			return 0, errors.New("empty obfuscation frame")
		}
		c.readBuf = plain
	}
	n := copy(p, c.readBuf)
	c.readBuf = c.readBuf[n:]
	return n, nil
}

func (c *obfsConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	obfuscated, err := c.strategy.ObfuscateWrite(p)
	if err != nil {
		return 0, err
	}

	if len(obfuscated) == 0 || len(obfuscated) > maxObfuscatedFrame {
		return 0, errors.New("invalid obfuscation frame length")
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(obfuscated)))
	if err = writeAll(c.Conn, header[:]); err != nil {
		return 0, err
	}
	if err = writeAll(c.Conn, obfuscated); err != nil {
		return 0, err
	}

	return len(p), nil
}

func (c *obfsConn) Close() error {
	c.strategy.Close()
	return c.Conn.Close()
}

// RandomPaddingObfs implements random padding obfuscation
type RandomPaddingObfs struct {
	minPad int
	maxPad int
}

func (o *RandomPaddingObfs) ObfuscateWrite(p []byte) ([]byte, error) {
	padRange := o.maxPad - o.minPad + 1
	if padRange <= 0 {
		return nil, errors.New("invalid padding range")
	}

	padBig, err := rand.Int(rand.Reader, big.NewInt(int64(padRange)))
	if err != nil {
		return nil, err
	}
	padLen := o.minPad + int(padBig.Int64())

	total := 2 + len(p) + padLen
	buf := make([]byte, total)

	binary.BigEndian.PutUint16(buf[0:2], uint16(len(p)))
	copy(buf[2:], p)

	if _, err := io.ReadFull(rand.Reader, buf[2+len(p):]); err != nil {
		return nil, err
	}

	return buf, nil
}

func (o *RandomPaddingObfs) DeobfuscateRead(p []byte) ([]byte, error) {
	if len(p) < 2 {
		return nil, errors.New("truncated padding frame")
	}

	dataLen := binary.BigEndian.Uint16(p[0:2])
	if 2+int(dataLen) > len(p) {
		return nil, errors.New("invalid padding frame length")
	}

	return p[2 : 2+dataLen], nil
}

func (o *RandomPaddingObfs) Overhead() int {
	return 2 + o.maxPad
}

func (o *RandomPaddingObfs) Close() error {
	return nil
}

// TimingPerturbObfs implements timing perturbation obfuscation
type TimingPerturbObfs struct {
	minDelay time.Duration
	maxDelay time.Duration
}

func (o *TimingPerturbObfs) ObfuscateWrite(p []byte) ([]byte, error) {
	delayRange := o.maxDelay - o.minDelay
	if delayRange <= 0 {
		return nil, errors.New("invalid delay range")
	}

	delayNs, err := rand.Int(rand.Reader, big.NewInt(int64(delayRange)))
	if err != nil {
		return nil, err
	}
	delay := o.minDelay + time.Duration(delayNs.Int64())

	time.Sleep(delay)
	return p, nil
}

func (o *TimingPerturbObfs) DeobfuscateRead(p []byte) ([]byte, error) {
	return p, nil
}

func (o *TimingPerturbObfs) Overhead() int {
	return 0
}

func (o *TimingPerturbObfs) Close() error {
	return nil
}

// TLSTrafficMimicObfs implements TLS traffic mimicking obfuscation
type TLSTrafficMimicObfs struct{}

const (
	tlsRecordTypeAppData = 0x17
	tlsVersion1_2        = 0x0303
	maxTLSRecordPayload  = 16384
)

func (o *TLSTrafficMimicObfs) ObfuscateWrite(p []byte) ([]byte, error) {
	if len(p) == 0 {
		return nil, errors.New("empty payload")
	}

	totalLen := 5 + len(p)
	buf := make([]byte, totalLen)

	buf[0] = tlsRecordTypeAppData
	binary.BigEndian.PutUint16(buf[1:3], tlsVersion1_2)
	binary.BigEndian.PutUint16(buf[3:5], uint16(len(p)))
	copy(buf[5:], p)

	return buf, nil
}

func (o *TLSTrafficMimicObfs) DeobfuscateRead(p []byte) ([]byte, error) {
	if len(p) < 5 {
		return nil, errors.New("truncated TLS record")
	}

	if p[0] != tlsRecordTypeAppData {
		return nil, errors.New("invalid TLS record type")
	}

	payloadLen := binary.BigEndian.Uint16(p[3:5])
	if 5+int(payloadLen) > len(p) {
		return nil, errors.New("invalid TLS record length")
	}

	return p[5 : 5+payloadLen], nil
}

func (o *TLSTrafficMimicObfs) Overhead() int {
	return 5
}

func (o *TLSTrafficMimicObfs) Close() error {
	return nil
}

// newObfuscator creates an obfuscator from configuration
func newObfuscator(cfg *contract.ObfuscationConfig) (Obfuscator, error) {
	if cfg == nil {
		return nil, nil
	}

	switch cfg.Strategy {
	case "random-padding":
		minPad := 10
		maxPad := 255
		if v, ok := cfg.Params["min_pad"].(float64); ok {
			minPad = int(v)
		}
		if v, ok := cfg.Params["max_pad"].(float64); ok {
			maxPad = int(v)
		}
		if minPad < 0 || maxPad > 255 || minPad > maxPad {
			return nil, errors.New("invalid padding range")
		}
		return &RandomPaddingObfs{minPad: minPad, maxPad: maxPad}, nil

	case "timing-perturb":
		minDelay := 1 * time.Millisecond
		maxDelay := 50 * time.Millisecond
		if v, ok := cfg.Params["min_delay_ms"].(float64); ok {
			minDelay = time.Duration(v) * time.Millisecond
		}
		if v, ok := cfg.Params["max_delay_ms"].(float64); ok {
			maxDelay = time.Duration(v) * time.Millisecond
		}
		if minDelay < 0 || maxDelay > 1000*time.Millisecond || minDelay > maxDelay {
			return nil, errors.New("invalid delay range")
		}
		return &TimingPerturbObfs{minDelay: minDelay, maxDelay: maxDelay}, nil

	case "tls-mimic":
		return &TLSTrafficMimicObfs{}, nil

	case "none", "":
		return nil, nil

	default:
		return nil, errors.New("unsupported obfuscation strategy: " + cfg.Strategy)
	}
}

// ValidateObfuscation checks an operator-supplied configuration without
// exposing the concrete strategy implementations to command packages.
func ValidateObfuscation(cfg *contract.ObfuscationConfig) error {
	if cfg == nil {
		return nil
	}
	o, err := newObfuscator(cfg)
	if err != nil {
		return err
	}
	if o != nil {
		return o.Close()
	}
	return nil
}
