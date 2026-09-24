package tunnel

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

// Measure the AEAD itself, independently of sockets, TLS records and framing.
// Seal uses a different nonce for every operation; Open verifies the tag.
// Keys and buffers are created outside the timed loop and are benchmark-only.
func BenchmarkAESGCM(b *testing.B) {
	for _, bits := range []int{128, 256} {
		for _, size := range []int{1024, 16 * 1024, 32 * 1024} {
			for _, operation := range []string{"encrypt", "decrypt"} {
				b.Run(fmt.Sprintf("AES%d/bytes=%d/%s", bits, size, operation), func(b *testing.B) {
					key := make([]byte, bits/8)
					if _, err := rand.Read(key); err != nil {
						b.Fatal(err)
					}
					block, err := aes.NewCipher(key)
					if err != nil {
						b.Fatal(err)
					}
					aead, err := cipher.NewGCM(block)
					if err != nil {
						b.Fatal(err)
					}
					plaintext := make([]byte, size)
					if _, err := rand.Read(plaintext); err != nil {
						b.Fatal(err)
					}
					nonce := make([]byte, aead.NonceSize())
					aad := []byte{23, 3, 3, 0, 0} // Five authenticated bytes, as in TLS.
					sealed := make([]byte, 0, size+aead.Overhead())
					opened := make([]byte, 0, size)
					if operation == "decrypt" {
						sealed = aead.Seal(sealed, nonce, plaintext, aad)
					}
					b.SetBytes(int64(size))
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						if operation == "encrypt" {
							binary.BigEndian.PutUint64(nonce[len(nonce)-8:], uint64(i))
							sealed = aead.Seal(sealed[:0], nonce, plaintext, aad)
						} else {
							opened, err = aead.Open(opened[:0], nonce, sealed, aad)
							if err != nil {
								b.Fatal(err)
							}
						}
					}
					b.StopTimer()
					if operation == "encrypt" {
						opened, err = aead.Open(opened[:0], nonce, sealed, aad)
					}
					if err != nil || !bytes.Equal(opened, plaintext) {
						b.Fatalf("AES-GCM round trip failed: %v", err)
					}
				})
			}
		}
	}
}

// Measure sustained one-way payload throughput over real TLS-protected
// carriers, with an acknowledgement after all bytes reach the destination.
// Both endpoints run locally: this includes encryption AND decryption plus
// framing/socket/mux costs, not an isolated encrypt/decrypt measurement.
func BenchmarkAESCarrierBulk(b *testing.B) {
	pair, roots := testCertificate(b)
	for _, transport := range []string{"tls", "ws", "wss", "http"} {
		for _, mux := range []bool{false, true} {
			for _, direction := range []string{"upload", "download"} {
				b.Run(fmt.Sprintf("%s/mux=%t/%s", transport, mux, direction), func(b *testing.B) {
					targetListener, err := net.Listen("tcp", "127.0.0.1:0")
					if err != nil {
						b.Fatal(err)
					}
					defer targetListener.Close()
					accepted := make(chan net.Conn, 1)
					go func() {
						conn, _ := targetListener.Accept()
						accepted <- conn
					}()
					// Reclaim the target even if carrier setup fails after dialing it.
					defer func() {
						targetListener.Close()
						for conn := range accepted {
							if conn != nil {
								conn.Close()
							}
							break
						}
					}()
					listener, err := net.Listen("tcp", "127.0.0.1:0")
					if err != nil {
						b.Fatal(err)
					}
					target := targetListener.Addr().String()
					s := &Server{TLS: &tls.Config{Certificates: []tls.Certificate{pair}}, Token: "aes-benchmark-test-token", Allowed: map[string]bool{"tcp|" + target: true}}
					done := make(chan struct{})
					go func() { defer close(done); _ = s.Serve(listener, transport) }()
					defer func() { s.Close(); <-done }()
					endpoint := listener.Addr().String()
					if transport == "ws" || transport == "wss" {
						endpoint = transport + "://" + endpoint + "/tunnel"
					}
					states := make(chan tls.ConnectionState, 1)
					client := Client{Pool: &MuxPool{}, TLS: &tls.Config{RootCAs: roots, VerifyConnection: func(state tls.ConnectionState) error {
						states <- state
						return nil
					}}}
					defer client.Pool.Close()
					c, err := client.DialRoute(context.Background(), transport, "tcp", target, contract.Tunnel{Endpoint: endpoint, ServerName: "localhost", Token: s.Token, Mux: mux})
					if err != nil {
						b.Fatal(err)
					}
					defer c.Close()
					peer := <-accepted
					close(accepted)
					if peer == nil {
						b.Fatal("target accept failed")
					}
					defer peer.Close()
					state := <-states
					if state.Version != tls.VersionTLS13 || len(state.VerifiedChains) == 0 {
						b.Fatal("TLS 1.3 with a verified certificate is required")
					}
					bits := 0
					switch state.CipherSuite {
					case tls.TLS_AES_128_GCM_SHA256:
						bits = 128
					case tls.TLS_AES_256_GCM_SHA384:
						bits = 256
					default:
						b.Skipf("negotiated %s; this benchmark requires AES-GCM", tls.CipherSuiteName(state.CipherSuite))
					}
					payload := make([]byte, 32*1024)
					if _, err := rand.Read(payload); err != nil {
						b.Fatal(err)
					}
					received := make([]byte, len(payload))
					run := func(count int) error {
						deadline := time.Now().Add(2 * time.Minute)
						c.SetDeadline(deadline)
						peer.SetDeadline(deadline)
						var sender, receiver net.Conn = c, peer
						if direction == "download" {
							sender, receiver = peer, c
						}
						drained := make(chan error, 1)
						go func() {
							err := bulkReceive(receiver, count, received, payload)
							if err != nil {
								receiver.Close()
							}
							drained <- err
						}()
						if err := bulkSend(sender, count, payload); err != nil {
							c.Close()
							peer.Close()
							<-drained
							return err
						}
						return <-drained
					}
					// Grow TLS record sizes and warm reusable buffers before timing.
					if err := run(32); err != nil {
						b.Fatal(err)
					}
					b.SetBytes(int64(len(payload)))
					b.ReportAllocs()
					b.ResetTimer()
					if err := run(b.N); err != nil {
						b.Fatal(err)
					}
					b.StopTimer()
					b.ReportMetric(float64(bits), "AES-bits")
				})
			}
		}
	}
}

func bulkSend(conn net.Conn, count int, payload []byte) error {
	for range count {
		if err := writeAll(conn, payload); err != nil {
			return err
		}
	}
	var ack [1]byte
	if _, err := io.ReadFull(conn, ack[:]); err != nil {
		return err
	}
	if ack[0] != 1 {
		return errors.New("bulk acknowledgement mismatch")
	}
	return nil
}

func bulkReceive(conn net.Conn, count int, buf, expected []byte) error {
	for range count {
		if _, err := io.ReadFull(conn, buf); err != nil {
			return err
		}
	}
	if !bytes.Equal(buf, expected) {
		return errors.New("bulk payload mismatch")
	}
	return writeAll(conn, []byte{1})
}
