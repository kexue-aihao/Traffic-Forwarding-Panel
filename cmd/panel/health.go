package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"
)

func healthcheck(addr string) error {
	return healthcheckTLS(addr, "")
}

func healthcheckTLS(addr, certificate string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid healthcheck listen address: %w", err)
	}
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}
	scheme := "http"
	transport := &http.Transport{Proxy: nil}
	if certificate != "" {
		body, err := os.ReadFile(certificate)
		if err != nil {
			return errors.New("cannot read healthcheck TLS certificate")
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(body) {
			return errors.New("invalid healthcheck TLS certificate")
		}
		block, _ := pem.Decode(body)
		if block == nil {
			return errors.New("invalid healthcheck TLS certificate")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return errors.New("invalid healthcheck TLS certificate")
		}
		serverName := host
		if len(cert.DNSNames) > 0 {
			serverName = cert.DNSNames[0]
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: roots, ServerName: serverName, MinVersion: tls.VersionTLS12}
		scheme = "https"
	}
	client := &http.Client{
		Timeout:       5 * time.Second,
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()
	response, err := client.Get(scheme + "://" + net.JoinHostPort(host, port) + "/api/v1/health")
	if err != nil {
		return errors.New("panel healthcheck connection failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("panel healthcheck returned HTTP %d", response.StatusCode)
	}
	return nil
}
