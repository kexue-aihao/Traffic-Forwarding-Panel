package tunnel

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"testing"
	"time"
)

func testCertificate(t testing.TB) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	der, e := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if e != nil {
		t.Fatal(e)
	}
	kb, e := x509.MarshalPKCS8PrivateKey(key)
	if e != nil {
		t.Fatal(e)
	}
	cp := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	pair, e := tls.X509KeyPair(cp, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}))
	if e != nil {
		t.Fatal(e)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(cp)
	return pair, roots
}
func echoServers(t testing.TB) (string, string) {
	t.Helper()
	tcp, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { tcp.Close() })
	go func() {
		for {
			c, e := tcp.Accept()
			if e != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	udp, e := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { udp.Close() })
	go func() {
		p := make([]byte, 65535)
		for {
			n, a, e := udp.ReadFromUDP(p)
			if e != nil {
				return
			}
			udp.WriteToUDP(p[:n], a)
		}
	}()
	return tcp.Addr().String(), udp.LocalAddr().String()
}
func TestAllCarriersRealTCPUDPAndAuthorization(t *testing.T) {
	pair, roots := testCertificate(t)
	tcpTarget, udpTarget := echoServers(t)
	for _, transport := range []string{"tls", "ws", "wss", "http"} {
		t.Run(transport, func(t *testing.T) {
			l, e := net.Listen("tcp", "127.0.0.1:0")
			if e != nil {
				t.Fatal(e)
			}
			s := &Server{TLS: &tls.Config{Certificates: []tls.Certificate{pair}}, Token: "test-secret-token-123", Allowed: map[string]bool{"tcp|" + tcpTarget: true, "udp|" + udpTarget: true}}
			done := make(chan struct{})
			go func() { defer close(done); s.Serve(l, transport) }()
			t.Cleanup(func() { s.Close(); <-done })
			endpoint := l.Addr().String()
			if transport == "ws" || transport == "wss" {
				endpoint = transport + "://" + endpoint + "/tunnel"
			}
			client := Client{TLS: &tls.Config{RootCAs: roots}, Timeout: 2 * time.Second}
			c, e := client.Dial(context.Background(), transport, endpoint, "localhost", s.Token, "tcp", tcpTarget)
			if e != nil {
				t.Fatal(e)
			}
			c.SetDeadline(time.Now().Add(5 * time.Second))
			payload := bytes.Repeat([]byte("boundary-data"), 10000)
			writeDone := make(chan error, 1)
			go func() {
				_, e := c.Write(payload)
				if e == nil {
					e = c.CloseWrite()
				}
				writeDone <- e
			}()
			got, e := io.ReadAll(c)
			c.Close()
			if e != nil {
				t.Fatal(e)
			}
			if e = <-writeDone; e != nil {
				t.Fatal(e)
			}
			if !bytes.Equal(payload, got) {
				t.Fatalf("TCP mismatch %d vs %d", len(payload), len(got))
			}
			u, e := client.Dial(context.Background(), transport, endpoint, "localhost", s.Token, "udp", udpTarget)
			if e != nil {
				t.Fatal(e)
			}
			defer u.Close()
			u.SetDeadline(time.Now().Add(5 * time.Second))
			for _, packet := range [][]byte{[]byte("one"), {}, bytes.Repeat([]byte("z"), 50000), []byte("last")} {
				if e = u.WritePacket(packet); e != nil {
					t.Fatal(e)
				}
				got, e := u.ReadPacket()
				if e != nil {
					t.Fatal(e)
				}
				if !bytes.Equal(got, packet) {
					t.Fatalf("UDP boundary mismatch")
				}
			}
			if c, e := client.Dial(context.Background(), transport, endpoint, "localhost", "wrong-token", "tcp", tcpTarget); e == nil {
				c.Close()
				t.Fatal("invalid token accepted")
			}
			if c, e := client.Dial(context.Background(), transport, endpoint, "localhost", s.Token, "tcp", "127.0.0.1:1"); e == nil {
				c.Close()
				t.Fatal("non-allowlisted target accepted")
			}
			if c, e := client.Dial(context.Background(), transport, endpoint, "wrong.example", s.Token, "tcp", tcpTarget); e == nil {
				c.Close()
				t.Fatal("wrong certificate name accepted")
			}
			untrusted := Client{Timeout: 2 * time.Second}
			if c, e := untrusted.Dial(context.Background(), transport, endpoint, "localhost", s.Token, "tcp", tcpTarget); e == nil {
				c.Close()
				t.Fatal("untrusted certificate accepted")
			}
		})
	}
}
func TestOversizedFrameRejected(t *testing.T) {
	if _, _, e := readFrame(bytes.NewReader([]byte{dataFrame, 0, 1, 0, 0})); e == nil {
		t.Fatal("oversize accepted")
	}
}
