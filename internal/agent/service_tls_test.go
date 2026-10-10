package agent

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"io"
	"math/big"
	"net"
	"os"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/tunnel"
	"github.com/quic-go/quic-go"
)

func applyServices(t *testing.T, m *ServiceManager, configs []contract.ServiceConfig) {
	t.Helper()
	commit, _, err := m.Prepare(configs, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	commit()
}

func managedEcho(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	return l.Addr().String()
}

func signedClient(t *testing.T, ca tls.Certificate) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := x509.ParseCertificate(ca.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, issuer, &key.PublicKey, ca.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func TestManagedTLSHandshakePolicy(t *testing.T) {
	pair, roots := chainCertificate(t)
	// This test authority authorizes both TLS usages; an authority restricted
	// to serverAuth must not be used to issue an mTLS client certificate.
	ca, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	ca.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, pair.PrivateKey.(*ecdsa.PrivateKey).Public(), pair.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	pair.Certificate = [][]byte{der}
	ca, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots = x509.NewCertPool()
	roots.AddCert(ca)
	listen, target := freeTCP(t), managedEcho(t)
	profile := localProfile(t, pair, []string{listen})
	m := &ServiceManager{Profiles: map[string]ServiceProfile{"local": profile}}
	t.Cleanup(m.Close)
	grant := contract.ServiceGrant{Identity: "rule-a", StreamToken: "business-token-long-123456", Targets: []contract.ServiceTarget{{RuleID: "rule-a", Network: "tcp", Target: target}}}
	v := contract.ServiceConfig{ID: "exit", Kind: "exit", Listen: listen, Transport: "tls", Token: "base-token-long-123456", Profile: "local", Grants: []contract.ServiceGrant{grant}, TLS: contract.ReverseTLS{ALPN: []string{"tfp-reverse-v1"}, CAProfile: "local"}}
	applyServices(t, m, []contract.ServiceConfig{v})
	_, wrongRoots := chainCertificate(t)
	dial := func(tc *tls.Config, good bool) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		client := tunnel.Client{TLS: tc, RuleID: "rule-a"}
		c, err := client.DialRoute(ctx, "tls", "tcp", target, contract.Tunnel{Endpoint: listen, ServerName: tc.ServerName, Token: grant.StreamToken})
		if !good {
			if err == nil {
				c.Close()
				t.Fatal("forbidden TLS handshake opened business stream")
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(time.Second))
		c.Write([]byte("ok"))
		var got [2]byte
		if _, err = io.ReadFull(c, got[:]); err != nil || string(got[:]) != "ok" {
			t.Fatalf("business echo %q %v", got, err)
		}
	}
	valid := &tls.Config{RootCAs: roots, ServerName: "localhost", NextProtos: []string{"tfp-reverse-v1"}}
	dial(valid, true)
	for _, bad := range []*tls.Config{
		{RootCAs: wrongRoots, ServerName: "localhost", NextProtos: valid.NextProtos},
		{RootCAs: roots, ServerName: "wrong.example", NextProtos: valid.NextProtos},
		{RootCAs: roots, ServerName: "localhost"},
		{RootCAs: roots, ServerName: "localhost", NextProtos: []string{"h2"}},
		{RootCAs: roots, ServerName: "localhost", MaxVersion: tls.VersionTLS12, NextProtos: valid.NextProtos},
	} {
		dial(bad, false)
	}
	profile.RequireClientCertificate = true
	m.Profiles["local"] = profile
	applyServices(t, m, []contract.ServiceConfig{v})
	dial(valid, false)
	mtls := valid.Clone()
	mtls.Certificates = []tls.Certificate{signedClient(t, pair)}
	dial(mtls, true)
	wrongPair, _ := chainCertificate(t)
	mtls.Certificates = []tls.Certificate{signedClient(t, wrongPair)}
	dial(mtls, false)
}

func TestManagedCertificateRotationOnSameTCPAndQUICPorts(t *testing.T) {
	pair, roots := chainCertificate(t)
	listen := freeTCP(t)
	udpListen, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	udpAddress := udpListen.LocalAddr().String()
	udpListen.Close()
	profile := localProfile(t, pair, []string{listen, udpAddress})
	m := &ServiceManager{Profiles: map[string]ServiceProfile{"local": profile}}
	t.Cleanup(m.Close)
	v := contract.ServiceConfig{ID: "exit", Kind: "exit", Listen: listen, Transport: "tls", Token: "base-token-long-123456", Profile: "local", UDP: &contract.UDPExit{Endpoint: udpAddress, Token: "datagram-base-token-123456"}}
	applyServices(t, m, []contract.ServiceConfig{v})
	check := func(pool *x509.CertPool) {
		t.Helper()
		c, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", listen, &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS13})
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		q, err := quic.DialAddr(ctx, udpAddress, &tls.Config{RootCAs: pool, ServerName: "localhost", NextProtos: []string{"tfp-udp-datagram-v1"}}, &quic.Config{EnableDatagrams: true})
		if err != nil {
			t.Fatal(err)
		}
		q.CloseWithError(0, "test complete")
	}
	check(roots)
	oldServer, oldUDP := m.entries[v.ID].server, m.entries[v.ID].udp
	newPair, newRoots := chainCertificate(t)
	newProfile := localProfile(t, newPair, nil)
	for _, paths := range [][2]string{{newProfile.Certificate, profile.Certificate}, {newProfile.PrivateKey, profile.PrivateKey}} {
		data, err := os.ReadFile(paths[0])
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(paths[1], data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	applyServices(t, m, []contract.ServiceConfig{v})
	if m.entries[v.ID].server == oldServer || m.entries[v.ID].udp != oldUDP {
		t.Fatal("certificate generation or UDP socket reuse incorrect")
	}
	check(newRoots)
	if bytes.Equal(oldServer.TLS.Certificates[0].Certificate[0], m.entries[v.ID].server.TLS.Certificates[0].Certificate[0]) {
		t.Fatal("certificate did not rotate")
	}
}

func TestManagedServiceRenewalIgnoresExpiredPreviousTimer(t *testing.T) {
	pair, roots := chainCertificate(t)
	listen := freeTCP(t)
	m := &ServiceManager{Profiles: map[string]ServiceProfile{"local": localProfile(t, pair, []string{listen})}}
	t.Cleanup(m.Close)
	v := contract.ServiceConfig{ID: "exit", Kind: "exit", Listen: listen, Transport: "tls", Token: "base-token-long-123456", Profile: "local"}
	commit, _, err := m.Prepare([]contract.ServiceConfig{v}, time.Now().Add(100*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	commit()
	// Prepare holds the manager lock across the previous expiry; the expired
	// callback is queued while a valid replacement is staged.
	commit, _, err = m.Prepare([]contract.ServiceConfig{v}, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	commit()
	time.Sleep(20 * time.Millisecond)
	if status := m.Statuses(); len(status) != 1 || !status[0].Ready {
		t.Fatalf("previous timer revoked renewed service: %+v", status)
	}
	c, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", listen, &tls.Config{RootCAs: roots, ServerName: "localhost"})
	if err != nil {
		t.Fatal("renewed listener was closed", err)
	}
	c.Close()
}
