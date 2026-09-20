package agent

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/tunnel"
)

func chainCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, roots
}

func TestChainChargesPayloadOnlyOnceAtEntry(t *testing.T) {
	pair, roots := chainCertificate(t)
	for _, count := range []int{2, 3} {
		for _, network := range []string{"tcp", "udp"} {
			t.Run(fmt.Sprintf("%d/%s", count, network), func(t *testing.T) {
				store, runtime, config := setup(t)
				runtime.Client = tunnel.Client{TLS: &tls.Config{RootCAs: roots}, Timeout: 2 * time.Second}
				var target string
				if network == "tcp" {
					l, err := net.Listen("tcp", "127.0.0.1:0")
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { l.Close() })
					target = l.Addr().String()
					go func() {
						c, err := l.Accept()
						if err == nil {
							defer c.Close()
							io.Copy(c, c)
						}
					}()
				} else {
					c, err := net.ListenPacket("udp", "127.0.0.1:0")
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { c.Close() })
					target = c.LocalAddr().String()
					go func() {
						buf := make([]byte, 65535)
						for {
							n, peer, err := c.ReadFrom(buf)
							if err != nil {
								return
							}
							c.WriteTo(buf[:n], peer)
						}
					}()
				}
				var hops []contract.TunnelHop
				var listeners []net.Listener
				for i, transport := range []string{"tls", "ws", "http"}[:count] {
					l, err := net.Listen("tcp", "127.0.0.1:0")
					if err != nil {
						t.Fatal(err)
					}
					listeners = append(listeners, l)
					endpoint := l.Addr().String()
					if transport == "ws" {
						endpoint = "ws://" + endpoint + "/tunnel"
					}
					hops = append(hops, contract.TunnelHop{Transport: transport, Endpoint: endpoint, ServerName: "localhost", Token: fmt.Sprintf("exit-token-number-%d", i)})
				}
				for i, hop := range hops {
					s := &tunnel.Server{TLS: &tls.Config{Certificates: []tls.Certificate{pair}}, Token: hop.Token, Allowed: map[string]bool{network + "|" + target: true}, NodeID: fmt.Sprintf("exit-%d", i), Client: runtime.Client}
					if i+1 < len(hops) {
						s.NextHops = []contract.TunnelHop{hops[i+1]}
					}
					done := make(chan struct{})
					go func() { defer close(done); s.Serve(listeners[i], hop.Transport) }()
					t.Cleanup(func() { s.Close(); <-done })
				}
				rule := testRule(target)
				rule.Network = network
				rule.Transport = hops[0].Transport
				rule.Tunnel = &contract.Tunnel{Endpoint: hops[0].Endpoint, ServerName: hops[0].ServerName, Token: hops[0].Token, Chain: hops[1:]}
				config.Rules = []contract.Rule{rule}
				if err := runtime.Apply(config, true); err != nil {
					t.Fatal(err)
				}
				binding := runtime.listeners[key(rule)]
				var address string
				if network == "tcp" {
					address = binding.tcp.Addr().String()
				} else {
					address = binding.udp.LocalAddr().String()
				}
				c, err := net.DialTimeout(network, address, time.Second)
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				c.SetDeadline(time.Now().Add(5 * time.Second))
				payload := bytes.Repeat([]byte("entry-meter"), 4000)
				if _, err = c.Write(payload); err != nil {
					t.Fatal(err)
				}
				var got []byte
				if network == "tcp" {
					c.(*net.TCPConn).CloseWrite()
					got, err = io.ReadAll(c)
				} else {
					got = make([]byte, 65535)
					var n int
					n, err = c.Read(got)
					got = got[:n]
				}
				if err != nil || !bytes.Equal(got, payload) {
					t.Fatalf("forwarded payload: %d bytes, %v", len(got), err)
				}
				var up, down int64
				for _, usage := range store.Pending() {
					if usage.RuleID != rule.ID || usage.LeaseID != rule.Lease.ID {
						t.Fatalf("usage belongs to another rule or lease: %+v", usage)
					}
					up += usage.UploadBytes
					down += usage.DownloadBytes
				}
				if up != int64(len(payload)) || down != int64(len(payload)) {
					t.Fatalf("payload charged more than once: %d up / %d down, want %d each", up, down, len(payload))
				}
				bad := config
				bad.Version++
				invalid := rule
				invalid.Tunnel = &contract.Tunnel{Endpoint: hops[0].Endpoint, ServerName: hops[0].ServerName, Token: hops[0].Token, Chain: []contract.TunnelHop{hops[0]}}
				bad.Rules = []contract.Rule{invalid}
				if err = runtime.Apply(bad, true); err == nil {
					t.Fatal("cyclic chain applied")
				}
				if runtime.Version() != config.Version || store.Config().Version != config.Version || runtime.listeners[key(rule)] != binding {
					t.Fatal("invalid chain replaced last valid configuration")
				}
			})
		}
	}
}
