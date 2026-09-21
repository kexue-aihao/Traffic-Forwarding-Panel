package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

func healthcheck(addr string) error {
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
	client := &http.Client{
		Timeout:       5 * time.Second,
		Transport:     &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()
	response, err := client.Get("http://" + net.JoinHostPort(host, port) + "/api/v1/health")
	if err != nil {
		return errors.New("panel healthcheck connection failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("panel healthcheck returned HTTP %d", response.StatusCode)
	}
	return nil
}
