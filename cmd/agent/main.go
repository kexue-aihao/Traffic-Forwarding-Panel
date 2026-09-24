package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/agent"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/probe"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/tunnel"
)

func main() {
	if e := run(); e != nil {
		if errors.Is(e, agent.ErrRestart) {
			os.Exit(agent.RestartExitCode)
		}
		log.Fatal(e)
	}
}
func run() error {
	mode := flag.String("mode", "agent", "agent, exit or reverse-exit")
	showVersion := flag.Bool("version", false, "print Agent release version")
	enableUninstall := flag.Bool("enable-uninstall", false, "enable remote uninstall of an official systemd installation")
	enableTerminal := flag.Bool("enable-terminal", false, "enable audited Linux remote commands as the Agent service account")
	releaseKey := flag.String("release-key", "", "base64 Ed25519 public key file; enables supervised Linux upgrades")
	panel := flag.String("panel", "https://localhost:8443", "panel base URL")
	name := flag.String("name", "node", "node display name")
	state := flag.String("state", "agent-state.json", "durable private state path")
	ca := flag.String("ca", "", "PEM root CA for panel and tunnel certificate validation")
	listen := flag.String("listen", "127.0.0.1:9443", "exit listening address")
	transport := flag.String("transport", "tls", "exit transport: tls/ws/wss/http")
	cert := flag.String("cert", "", "exit PEM certificate")
	key := flag.String("key", "", "exit PEM private key")
	allow := flag.String("allow", "", "comma-separated exact exit destinations e.g. tcp|127.0.0.1:8080,udp|127.0.0.1:5353")
	nextHops := flag.String("next-hops", "", "private JSON file with operator-authorized next-hop entries")
	nodeID := flag.String("exit-id", "", "stable unique exit identity for chain cycle detection")
	reverseEndpoint := flag.String("reverse-endpoint", "", "outbound reverse carrier endpoint")
	serverName := flag.String("server-name", "", "reverse carrier certificate DNS name")
	echo := flag.String("ip-echo", "", "optional comma-separated controlled HTTPS plain-IP services")
	disk := flag.String("disk", ".", "probe filesystem path")
	flag.Parse()
	if *showVersion {
		fmt.Println(agent.Version)
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *mode == "uninstall-worker" {
		return agent.RunUninstallWorker(ctx)
	}
	tc, e := trustConfig(*ca)
	if e != nil {
		return e
	}
	if *mode == "exit" || *mode == "reverse-exit" {
		allowed := map[string]bool{}
		for _, v := range strings.Split(*allow, ",") {
			if v != "" {
				parts := strings.SplitN(v, "|", 2)
				if len(parts) != 2 || (parts[0] != "tcp" && parts[0] != "udp" && parts[0] != "reverse") {
					return fmt.Errorf("invalid allowed target")
				}
				if _, _, e := net.SplitHostPort(parts[1]); e != nil && parts[0] != "reverse" {
					return e
				}
				allowed[v] = true
			}
		}
		hops, e := loadNextHops(*nextHops)
		if e != nil {
			return e
		}
		s := &tunnel.Server{Token: os.Getenv("TFP_EXIT_TOKEN"), Allowed: allowed, NextHops: hops, NodeID: *nodeID, Client: tunnel.Client{TLS: tc}}
		if *mode == "reverse-exit" {
			return s.RunReverse(ctx, tunnel.Client{TLS: tc}, *transport, *reverseEndpoint, *serverName, *nodeID)
		}
		pair, e := tls.LoadX509KeyPair(*cert, *key)
		if e != nil {
			return e
		}
		s.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
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
	var upgrader *agent.Upgrader
	if *releaseKey != "" {
		if runtime.GOOS != "linux" {
			return fmt.Errorf("managed upgrades require Linux")
		}
		publicKey, e := agent.LoadReleaseKey(*releaseKey)
		if e != nil {
			return e
		}
		binary, e := os.Executable()
		if e != nil {
			return e
		}
		binary, e = filepath.EvalSymlinks(binary)
		if e != nil {
			return e
		}
		journal, e := filepath.Abs(*state + ".upgrade.json")
		if e != nil {
			return e
		}
		if e = os.MkdirAll(filepath.Dir(journal), 0700); e != nil {
			return e
		}
		upgrader = &agent.Upgrader{Binary: binary, Journal: journal, PublicKey: publicKey}
		if os.Getenv("TFP_AGENT_WORKER") != "1" {
			return upgrader.Supervise(ctx, os.Args[1:])
		}
	}
	if *enableTerminal && runtime.GOOS != "linux" {
		return fmt.Errorf("remote commands require Linux")
	}
	ctx, cancelRun := context.WithCancelCause(ctx)
	defer cancelRun(nil)
	if upgrader != nil {
		upgrader.Restart = func() { cancelRun(agent.ErrRestart) }
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
	if upgrader != nil {
		upgrader.HTTP = httpClient
	}
	a := agent.Agent{URL: *panel, EnrollmentToken: os.Getenv("TFP_ENROLLMENT_TOKEN"), Name: *name, Store: store, Runtime: runtime, Probe: collector, HTTP: httpClient, EnableTerminal: *enableTerminal, EnableUninstall: *enableUninstall, Upgrader: upgrader}
	if *ca != "" {
		a.PanelCA, e = os.ReadFile(*ca)
		if e != nil {
			return e
		}
	}
	e = a.Run(ctx)
	if errors.Is(context.Cause(ctx), agent.ErrRestart) {
		return agent.ErrRestart
	}
	return e
}
func trustConfig(ca string) (*tls.Config, error) {
	tc := &tls.Config{MinVersion: tls.VersionTLS13, ClientSessionCache: tls.NewLRUClientSessionCache(256)}
	if ca != "" {
		pem, e := os.ReadFile(ca)
		if e != nil {
			return nil, e
		}
		roots, e := x509.SystemCertPool()
		if e != nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("CA file contains no certificates")
		}
		tc.RootCAs = roots
	}
	return tc, nil
}
func loadNextHops(path string) ([]contract.TunnelHop, error) {
	if path == "" {
		return nil, nil
	}
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	stat, e := f.Stat()
	if e != nil {
		return nil, e
	}
	if stat.Size() > 65536 || !stat.Mode().IsRegular() {
		return nil, fmt.Errorf("next-hop config must be a regular file <=64KiB")
	}
	if runtime.GOOS != "windows" && stat.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("next-hop config contains secrets: chmod 600 required")
	}
	var hops []contract.TunnelHop
	d := json.NewDecoder(io.LimitReader(f, 65537))
	d.DisallowUnknownFields()
	if e = d.Decode(&hops); e != nil {
		return nil, e
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, fmt.Errorf("one next-hop JSON array required")
	}
	if len(hops) > 64 {
		return nil, fmt.Errorf("at most 64 next-hop entries")
	}
	for _, hop := range hops {
		if e = tunnel.ValidateChain(hop, nil); e != nil {
			return nil, e
		}
	}
	return hops, nil
}
