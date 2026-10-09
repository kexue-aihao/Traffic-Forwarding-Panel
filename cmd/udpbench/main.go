// udpbench runs independent UDP references and a paced, verified echo load.
// It intentionally never calls Agent accounting when running baseline relays.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/crc32"
	"log"
	"math"
	"net"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/tunnel"
)

const payloadHeader = 32
const duplicateWindow = 65536

var crcTable = crc32.MakeTable(crc32.Castagnoli)

type options struct {
	mode, listen, target, cert, key, ca, name, output string
	direction                                         string
	size, sessions                                    int
	pps                                               int64
	warmup, duration, drain                           time.Duration
}

func main() {
	if e := run(); e != nil {
		log.Fatal(e)
	}
}
func run() error {
	o := options{}
	flag.StringVar(&o.mode, "mode", "load", "echo, relay (B1), quic-relay (B2 exit), quic-entry (B2 entry), load")
	flag.StringVar(&o.listen, "listen", "127.0.0.1:9000", "reference listening address")
	flag.StringVar(&o.target, "target", "127.0.0.1:9001", "UDP target, or QUIC exit for quic-entry")
	flag.StringVar(&o.cert, "cert", "", "QUIC exit certificate PEM")
	flag.StringVar(&o.key, "key", "", "QUIC exit key PEM")
	flag.StringVar(&o.ca, "ca", "", "optional CA PEM")
	flag.StringVar(&o.name, "server-name", "", "QUIC certificate verification name")
	flag.StringVar(&o.output, "output", "", "JSON report path; stdout when omitted")
	flag.StringVar(&o.direction, "direction", "echo", "echo (symmetric bidirectional), upload (32-byte verified ACK), download (32-byte request)")
	flag.IntVar(&o.size, "size", 1200, "complete UDP payload length, including 32-byte measurement header")
	flag.IntVar(&o.sessions, "sessions", 1, "simultaneous UDP flows (1..1024)")
	flag.Int64Var(&o.pps, "pps", 10000, "aggregate offered packets per second")
	flag.DurationVar(&o.warmup, "warmup", 30*time.Second, "warmup excluded from results")
	flag.DurationVar(&o.duration, "duration", 120*time.Second, "measurement duration")
	flag.DurationVar(&o.drain, "drain", 2*time.Second, "receive drain time after sampling")
	forward := flag.String("forward", "", "business UDP destination for quic-entry")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if o.mode == "load" {
		return load(ctx, o)
	}
	if o.mode == "quic-relay" {
		cert, e := tls.LoadX509KeyPair(o.cert, o.key)
		if e != nil {
			return e
		}
		s := &tunnel.DatagramServer{TLS: &tls.Config{Certificates: []tls.Certificate{cert}}, Token: os.Getenv("TFP_EXIT_TOKEN")}
		go func() { <-ctx.Done(); s.Close() }()
		e = s.Serve(o.listen)
		if ctx.Err() != nil {
			return nil
		}
		return e
	}
	var pool *tunnel.DatagramPool
	var tc *tls.Config
	if o.mode == "quic-entry" {
		if *forward == "" {
			return errors.New("quic-entry requires -forward UDP destination")
		}
		pool = &tunnel.DatagramPool{}
		defer pool.Close()
		tc = &tls.Config{MinVersion: tls.VersionTLS13}
		if o.ca != "" {
			pem, e := os.ReadFile(o.ca)
			if e != nil {
				return e
			}
			roots, e := x509.SystemCertPool()
			if e != nil {
				roots = x509.NewCertPool()
			}
			if !roots.AppendCertsFromPEM(pem) {
				return errors.New("invalid CA PEM")
			}
			tc.RootCAs = roots
		}
	} else if o.mode != "echo" && o.mode != "relay" {
		return errors.New("invalid mode")
	}
	a, e := net.ResolveUDPAddr("udp", o.listen)
	if e != nil {
		return e
	}
	listener, e := net.ListenUDP("udp", a)
	if e != nil {
		return e
	}
	defer listener.Close()
	listener.SetReadBuffer(4 << 20)
	listener.SetWriteBuffer(4 << 20)
	go func() { <-ctx.Done(); listener.Close() }()
	log.Printf("%s listening on %s", o.mode, listener.LocalAddr())
	var mu sync.Mutex
	flows := map[string]net.Conn{}
	defer func() {
		mu.Lock()
		for _, c := range flows {
			c.Close()
		}
		mu.Unlock()
	}()
	buf := make([]byte, 65535)
	for {
		n, peer, e := listener.ReadFromUDP(buf)
		if e != nil {
			if ctx.Err() != nil {
				return nil
			}
			return e
		}
		if o.mode == "echo" {
			if p := referenceReply(buf[:n]); p != nil {
				listener.WriteToUDP(p, peer)
			}
			continue
		}
		k := peer.String()
		mu.Lock()
		flow := flows[k]
		full := len(flows) >= 1024
		mu.Unlock()
		if flow == nil {
			if full {
				continue
			}
			if pool != nil {
				flow, e = pool.Dial(ctx, tc, o.target, o.name, os.Getenv("TFP_EXIT_TOKEN"), *forward)
			} else {
				flow, e = (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "udp", o.target)
			}
			if e != nil {
				log.Printf("reference dial failed: %v", e)
				continue
			}
			mu.Lock()
			flows[k] = flow
			mu.Unlock()
			go func(c net.Conn, peer *net.UDPAddr, key string) {
				defer c.Close()
				defer func() { mu.Lock(); delete(flows, key); mu.Unlock() }()
				p := make([]byte, 65535)
				for {
					c.SetReadDeadline(time.Now().Add(2 * time.Minute))
					n, e := c.Read(p)
					if e != nil {
						return
					}
					listener.WriteToUDP(p[:n], peer)
				}
			}(flow, peer, k)
		}
		flow.SetWriteDeadline(time.Now().Add(5 * time.Second))
		flow.Write(buf[:n])
	}
}

const measurementMarker uint32 = 0xa0000000

func descriptor(flow, size int, direction string) uint32 {
	mode := uint32(0)
	if direction == "upload" {
		mode = 1
	} else if direction == "download" {
		mode = 2
	}
	return measurementMarker | uint32(size)<<12 | mode<<10 | uint32(flow)
}

// The reference target validates the uploaded payload before acknowledging it.
// Download requests carry the same sequence/timestamp with a bounded requested
// response length; bulk bytes are generated in the existing receive buffer.
func referenceReply(p []byte) []byte {
	if len(p) < payloadHeader || binary.BigEndian.Uint32(p[8:12])&0xf0000000 != measurementMarker {
		return p
	}
	d := binary.BigEndian.Uint32(p[8:12])
	size, mode, flow := int((d>>12)&0xffff), (d>>10)&3, int(d&1023)
	if size < payloadHeader || size > 65507 || mode == 3 || binary.BigEndian.Uint32(p[28:32]) != payloadCRC(p) || (mode == 2 && len(p) != payloadHeader) || (mode != 2 && len(p) != size) {
		return nil
	}
	if mode == 1 {
		p = p[:payloadHeader]
	} else if mode == 2 {
		if cap(p) < size {
			return nil
		}
		p = p[:size]
		for j := payloadHeader; j < size; j++ {
			p[j] = byte(j + flow)
		}
	}
	binary.BigEndian.PutUint32(p[28:32], payloadCRC(p))
	return p
}

type report struct {
	OS              string  `json:"os"`
	Arch            string  `json:"arch"`
	Go              string  `json:"go"`
	CPUs            int     `json:"logical_cpus"`
	Target          string  `json:"target"`
	Direction       string  `json:"direction"`
	Payload         int     `json:"payload_bytes"`
	Sessions        int     `json:"sessions"`
	OfferedPPS      int64   `json:"offered_pps"`
	DurationSeconds float64 `json:"duration_seconds"`
	Sent            uint64  `json:"sent_packets"`
	Received        uint64  `json:"verified_received_packets"`
	Lost            uint64  `json:"lost_packets"`
	LossPercent     float64 `json:"loss_percent"`
	Duplicates      uint64  `json:"duplicates"`
	Reordered       uint64  `json:"reordered"`
	Late            uint64  `json:"too_late_packets"`
	Corrupt         uint64  `json:"corrupt_packets"`
	WriteErrors     uint64  `json:"write_errors"`
	ReceivePPS      float64 `json:"verified_pps"`
	Mbps            float64 `json:"verified_payload_mbps"`
	UploadMbps      float64 `json:"verified_upload_mbps"`
	DownloadMbps    float64 `json:"verified_download_mbps"`
	P50US           int64   `json:"rtt_p50_us"`
	P95US           int64   `json:"rtt_p95_us"`
	P99US           int64   `json:"rtt_p99_us"`
	RTTOverflow     uint64  `json:"rtt_over_1_second"`
	ElapsedSeconds  float64 `json:"elapsed_seconds"`
	HeapBytes       uint64  `json:"heap_bytes_at_end"`
	Goroutines      int     `json:"goroutines_at_end"`
	Notes           string  `json:"notes"`
}
type loadCounters struct {
	sent, received, duplicates, reordered, late, corrupt, writeErrors, overflow atomic.Uint64
	histogram                                                                   [100001]atomic.Uint64
}

func load(ctx context.Context, o options) error {
	if o.direction == "" {
		o.direction = "echo"
	}
	if o.direction != "echo" && o.direction != "upload" && o.direction != "download" {
		return errors.New("direction must be echo, upload or download")
	}
	if o.size < payloadHeader || o.size > 65507 || o.sessions < 1 || o.sessions > 1024 || o.pps < 1 || o.pps > 10000000 || o.duration < time.Millisecond || o.duration > 24*time.Hour || o.warmup < 0 || o.drain < 0 || o.drain > time.Minute {
		return errors.New("invalid load size, sessions, rate or duration")
	}
	var counters loadCounters
	sendSize, replySize := o.size, o.size
	if o.direction == "upload" {
		replySize = payloadHeader
	} else if o.direction == "download" {
		sendSize = payloadHeader
	}
	var wg sync.WaitGroup
	start := time.Now().Add(100 * time.Millisecond)
	measure := start.Add(o.warmup)
	end := measure.Add(o.duration)
	connections := make([]net.Conn, o.sessions)
	defer func() {
		for _, c := range connections {
			if c != nil {
				c.Close()
			}
		}
	}()
	for i := range connections {
		c, e := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "udp", o.target)
		if e != nil {
			return e
		}
		connections[i] = c
		if raw, ok := c.(*net.UDPConn); ok {
			raw.SetReadBuffer(4 << 20)
			raw.SetWriteBuffer(4 << 20)
		}
	}
	var runIDBytes [8]byte
	binary.BigEndian.PutUint64(runIDBytes[:], uint64(time.Now().UnixNano()))
	runID := binary.BigEndian.Uint64(runIDBytes[:])
	for i, c := range connections {
		flow := i
		wg.Go(func() {
			buf := make([]byte, 65535)
			seen := make([]uint64, duplicateWindow)
			for i := range seen {
				seen[i] = math.MaxUint64
			}
			var highest uint64
			for {
				c.SetReadDeadline(end.Add(o.drain))
				n, e := c.Read(buf)
				if e != nil {
					return
				}
				p := buf[:n]
				if n != replySize || n < payloadHeader || binary.BigEndian.Uint64(p[:8]) != runID || binary.BigEndian.Uint32(p[8:12]) != descriptor(flow, o.size, o.direction) || binary.BigEndian.Uint32(p[28:32]) != payloadCRC(p) {
					counters.corrupt.Add(1)
					continue
				}
				seq := binary.BigEndian.Uint64(p[12:20])
				sentAt := time.Unix(0, int64(binary.BigEndian.Uint64(p[20:28])))
				if sentAt.Before(measure) || !sentAt.Before(end) {
					continue
				}
				us := time.Since(sentAt).Microseconds()
				if us < 0 {
					counters.corrupt.Add(1)
					continue
				}
				if seq < highest && highest-seq >= duplicateWindow {
					counters.late.Add(1)
					continue
				}
				if seen[seq%duplicateWindow] == seq {
					counters.duplicates.Add(1)
					continue
				}
				seen[seq%duplicateWindow] = seq
				if seq < highest {
					counters.reordered.Add(1)
				} else {
					highest = seq
				}
				counters.received.Add(1)
				if us > 1000000 {
					counters.overflow.Add(1)
				}
				counters.histogram[min(us/10, 100000)].Add(1)
			}
		})
		wg.Go(func() {
			payload := make([]byte, sendSize)
			for j := payloadHeader; j < len(payload); j++ {
				payload[j] = byte(j + flow)
			}
			binary.BigEndian.PutUint64(payload[:8], runID)
			binary.BigEndian.PutUint32(payload[8:12], descriptor(flow, o.size, o.direction))
			sequence := uint64(0)
			for {
				index := uint64(flow) + sequence*uint64(o.sessions)
				due := start.Add(time.Duration(float64(index) * float64(time.Second) / float64(o.pps)))
				if !due.Before(end) {
					return
				}
				if wait := time.Until(due); wait > 0 {
					timer := time.NewTimer(wait)
					select {
					case <-ctx.Done():
						timer.Stop()
						return
					case <-timer.C:
					}
				}
				now := time.Now()
				if !now.Before(end) || ctx.Err() != nil {
					return
				}
				binary.BigEndian.PutUint64(payload[12:20], sequence)
				binary.BigEndian.PutUint64(payload[20:28], uint64(now.UnixNano()))
				binary.BigEndian.PutUint32(payload[28:32], payloadCRC(payload))
				sequence++
				c.SetWriteDeadline(end)
				if _, e := c.Write(payload); e != nil {
					if !now.Before(measure) {
						counters.writeErrors.Add(1)
					}
					continue
				}
				if !now.Before(measure) {
					counters.sent.Add(1)
				}
			}
		})
	}
	go func() {
		select {
		case <-ctx.Done():
			for _, c := range connections {
				c.Close()
			}
		case <-time.After(time.Until(end.Add(o.drain))):
		}
	}()
	wg.Wait()
	if e := ctx.Err(); e != nil {
		return e
	}
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	r := report{OS: runtime.GOOS, Arch: runtime.GOARCH, Go: runtime.Version(), CPUs: runtime.NumCPU(), Target: o.target, Payload: o.size, Sessions: o.sessions, OfferedPPS: o.pps, DurationSeconds: o.duration.Seconds(), Sent: counters.sent.Load(), Received: counters.received.Load(), Duplicates: counters.duplicates.Load(), Reordered: counters.reordered.Load(), Late: counters.late.Load(), Corrupt: counters.corrupt.Load(), WriteErrors: counters.writeErrors.Load(), RTTOverflow: counters.overflow.Load(), ElapsedSeconds: time.Since(measure).Seconds(), HeapBytes: mem.HeapAlloc, Goroutines: runtime.NumGoroutine(), Notes: "Echo RTT; verified payload excludes proxy/QUIC overhead. Quantiles have 10 us resolution, overflow saturates at 1 s. Load rate is an offer, verify achieved sent rate. Delays beyond a 65536-packet reordering window count as late/lost. Warmup is excluded. Cross-host results require a recorded topology and CPU/storage/socket-drop evidence."}
	r.Direction = o.direction
	r.Notes += " Upload uses a 32-byte ACK after target CRC validation; download uses a 32-byte request. Payload Mbps counts bulk only; echo is correlated symmetric traffic, RTT is request/response RTT in every mode."
	if r.Received > r.Sent {
		return errors.New("verified replies exceed successful sends")
	}
	r.Lost = r.Sent - r.Received
	if r.Sent > 0 {
		r.LossPercent = float64(r.Lost) * 100 / float64(r.Sent)
	}
	r.ReceivePPS = float64(r.Received) / o.duration.Seconds()
	r.Mbps = r.ReceivePPS * float64(o.size) * 8 / 1000000
	if o.direction != "download" {
		r.UploadMbps = r.Mbps
	}
	if o.direction != "upload" {
		r.DownloadMbps = r.Mbps
	}
	quantile := func(f float64) int64 {
		target := uint64(math.Ceil(float64(r.Received) * f))
		var total uint64
		for i := range counters.histogram {
			total += counters.histogram[i].Load()
			if target > 0 && total >= target {
				return int64(i * 10)
			}
		}
		return 0
	}
	r.P50US = quantile(.5)
	r.P95US = quantile(.95)
	r.P99US = quantile(.99)
	data, e := json.MarshalIndent(r, "", "  ")
	if e != nil {
		return e
	}
	data = append(data, '\n')
	if o.output != "" {
		return os.WriteFile(o.output, data, 0644)
	}
	fmt.Print(string(data))
	return nil
}
func payloadCRC(p []byte) uint32 {
	crc := crc32.Update(0, crcTable, p[:28])
	return crc32.Update(crc, crcTable, p[32:])
}
