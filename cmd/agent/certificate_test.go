package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestIPCertificateRenewalPreservesOpenConnections(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, public, private)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
	certPath, keyPath := filepath.Join(t.TempDir(), "cert.pem"), filepath.Join(t.TempDir(), "key.pem")
	issue := func(serial int64) ([]byte, []byte) {
		t.Helper()
		pub, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		template := &x509.Certificate{SerialNumber: big.NewInt(serial), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
		der, err := x509.CreateCertificate(rand.Reader, template, ca, pub, private)
		if err != nil {
			t.Fatal(err)
		}
		keyDER, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	}
	write := func(path string, body []byte, stamp time.Time) {
		t.Helper()
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	cert, key := issue(2)
	write(certPath, cert, now)
	write(keyPath, key, now)
	config, err := certificateConfig(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := tls.Listen("tcp", "127.0.0.1:0", config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() { defer conn.Close(); _, _ = io.Copy(conn, conn) }()
		}
	}()
	dial := func(name string) (*tls.Conn, error) {
		return tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", listener.Addr().String(), &tls.Config{RootCAs: roots, ServerName: name, MinVersion: tls.VersionTLS13})
	}
	open, err := dial("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	defer open.Close()
	checkSerial := func(want int64) {
		t.Helper()
		conn, err := dial("127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if got := conn.ConnectionState().PeerCertificates[0].SerialNumber.Int64(); got != want {
			t.Fatalf("certificate serial = %d, want %d", got, want)
		}
	}
	if conn, err := dial("127.0.0.2"); err == nil {
		conn.Close()
		t.Fatal("IP certificate accepted for another IP")
	}
	cert, key = issue(3)
	write(certPath, cert, now.Add(time.Second))
	checkSerial(2) // Incomplete rotation retains the last matching certificate and key.
	write(keyPath, key, now.Add(time.Second))
	checkSerial(3)
	if err := open.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := open.Write([]byte("alive")); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, 5)
	if _, err := io.ReadFull(open, response); err != nil || string(response) != "alive" {
		t.Fatalf("established connection interrupted by renewal: %q, %v", response, err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 10 {
				if _, err := config.GetCertificate(nil); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
}

func TestCertificateConfigRejectsMissingFiles(t *testing.T) {
	if _, err := certificateConfig("missing-cert.pem", "missing-key.pem"); err == nil {
		t.Fatal("missing certificate accepted at startup")
	}
}
