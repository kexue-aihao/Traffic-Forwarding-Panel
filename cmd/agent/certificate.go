package main

import (
	"crypto/tls"
	"log"
	"os"
	"sync"
)

// Renewed certificates apply to new handshakes without interrupting open tunnels.
func certificateConfig(certPath, keyPath string) (*tls.Config, error) {
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, err
	}
	current := &pair
	var mu sync.Mutex
	var certInfo, keyInfo os.FileInfo
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			mu.Lock()
			defer mu.Unlock()
			certNow, certErr := os.Stat(certPath)
			keyNow, keyErr := os.Stat(keyPath)
			if certErr != nil || keyErr != nil {
				return current, nil
			}
			if sameCertificateFile(certInfo, certNow) && sameCertificateFile(keyInfo, keyNow) {
				return current, nil
			}
			updated, err := tls.LoadX509KeyPair(certPath, keyPath)
			if err != nil {
				// Certbot may replace the certificate and key in separate steps.
				log.Printf("certificate reload deferred: %v", err)
				return current, nil
			}
			current = &updated
			certInfo, keyInfo = certNow, keyNow
			return current, nil
		},
	}, nil
}

func sameCertificateFile(a, b os.FileInfo) bool {
	return a != nil && os.SameFile(a, b) && a.ModTime().Equal(b.ModTime()) && a.Size() == b.Size()
}
