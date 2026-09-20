package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/agent"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/probe"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/tunnel"
)

func main() {
	if e := run(); e != nil {
		log.Fatal(e)
	}
}
func run() error {
	mode := flag.String("mode", "agent", "agent or exit")
	panel := flag.String("panel", "https://localhost:8443", "panel base URL")
	name := flag.String("name", "node", "node display name")
	state := flag.String("state", "agent-state.json", "durable private state path")
	ca := flag.String("ca", "", "PEM root CA for panel and tunnel certificate validation")
	listen := flag.String("listen", "127.0.0.1:9443", "exit listening address")
	transport := flag.String("transport", "tls", "exit transport: tls/ws/wss/http")
	cert := flag.String("cert", "", "exit PEM certificate")
	key := flag.String("key", "", "exit PEM private key")
	allow := flag.String("allow", "", "comma-separated exact exit destinations e.g. tcp|127.0.0.1:8080,udp|127.0.0.1:5353")
	echo := flag.String("ip-echo", "", "optional comma-separated controlled HTTPS plain-IP services")
	disk := flag.String("disk", ".", "probe filesystem path")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *mode == "exit" {
		pair, e := tls.LoadX509KeyPair(*cert, *key)
		if e != nil {
			return e
		}
		allowed := map[string]bool{}
		for _, v := range strings.Split(*allow, ",") {
			if v != "" {
				parts := strings.SplitN(v, "|", 2)
				if len(parts) != 2 || (parts[0] != "tcp" && parts[0] != "udp") {
					return fmt.Errorf("invalid allowed target")
				}
				if _, _, e := net.SplitHostPort(parts[1]); e != nil {
					return e
				}
				allowed[v] = true
			}
		}
		s := &tunnel.Server{TLS: &tls.Config{Certificates: []tls.Certificate{pair}}, Token: os.Getenv("TFP_EXIT_TOKEN"), Allowed: allowed}
		l, e := net.Listen("tcp", *listen)
		if e != nil {
			return e
		}
		defer l.Close()
		go func() { <-ctx.Done(); s.Close() }()
		log.Printf("exit %s listening on %s", *transport, l.Addr())
		e = s.Serve(l, *transport)
		if ctx.Err() != nil {
			return nil
		}
		return e
	}
	if *mode != "agent" {
		return fmt.Errorf("invalid mode")
	}
	tc := &tls.Config{MinVersion: tls.VersionTLS13}
	if *ca != "" {
		pem, e := os.ReadFile(*ca)
		if e != nil {
			return e
		}
		roots, e := x509.SystemCertPool()
		if e != nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(pem) {
			return fmt.Errorf("CA file contains no certificates")
		}
		tc.RootCAs = roots
	}
	store, e := agent.OpenStore(*state)
	if e != nil {
		return e
	}
	defer store.Close()
	runtime := agent.NewRuntime(store, tunnel.Client{TLS: tc})
	collector := &probe.Collector{DiskPath: *disk}
	if *echo != "" {
		collector.EchoURLs = strings.Split(*echo, ",")
	}
	httpClient := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{TLSClientConfig: tc.Clone()}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	a := agent.Agent{URL: *panel, EnrollmentToken: os.Getenv("TFP_ENROLLMENT_TOKEN"), Name: *name, Store: store, Runtime: runtime, Probe: collector, HTTP: httpClient}
	return a.Run(ctx)
}
