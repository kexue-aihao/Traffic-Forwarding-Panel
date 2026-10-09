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

func parseObfuscation(strategy, paramsJSON string) (*contract.ObfuscationConfig, error) {
	if strings.TrimSpace(strategy) == "" || strategy == "none" {
		return nil, nil
	}
	params := map[string]any{}
	if strings.TrimSpace(paramsJSON) != "" {
		if err := json.Unmarshal([]byte(paramsJSON), &params); err != nil {
			return nil, fmt.Errorf("invalid obfuscation parameters: %w", err)
		}
	}
	cfg := &contract.ObfuscationConfig{Strategy: strategy, Params: params}
	if err := tunnel.ValidateObfuscation(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func main() {
	if e := run(); e != nil {
		if errors.Is(e, agent.ErrRestart) {
			os.Exit(agent.RestartExitCode)
		}
		log.Fatal(e)
	}
}
func run() error {
	mode := flag.String("mode", "agent", "agent, exit, secure-direct or reverse-exit")
	showVersion := flag.Bool("version", false, "print Agent release version")
	enableUninstall := flag.Bool("enable-uninstall", false, "enable remote uninstall of an official systemd installation")
	enableTerminal := flag.Bool("enable-terminal", false, "enable audited Linux remote commands as the Agent service account")
	releaseKey := flag.String("release-key", "", "base64 Ed25519 public key file; enables supervised Linux upgrades")
	panel := flag.String("panel", "https://localhost:8443", "panel base URL")
	name := flag.String("name", "node", "node display name")
	state := flag.String("state", "agent-state.json", "durable private state path")
	ca := flag.String("ca", "", "PEM root CA for panel and tunnel certificate validation")
	listen := flag.String("listen", "127.0.0.1:9443", "exit listening address")
	transport := flag.String("transport", "tls", "exit transport: tls/ws/wss/http/secure-direct")
	udpListen := flag.String("udp-listen", "", "optional single-exit QUIC DATAGRAM listening address (UDP)")
	cert := flag.String("cert", "", "exit PEM certificate")
	key := flag.String("key", "", "exit PEM private key")
	allow := flag.String("allow", "", "authorized reverse carrier identities e.g. reverse|exit-a; legacy tcp/udp destinations are ignored")
	nextHops := flag.String("next-hops", "", "private JSON file with operator-authorized next-hop entries")
	obfuscation := flag.String("obfuscation-strategy", "", "tunnel obfuscation strategy: random-padding/timing-perturb/tls-mimic")
	obfuscationParams := flag.String("obfuscation-params", "", "JSON object with obfuscation strategy parameters")
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
	if *mode == "exit" || *mode == "secure-direct" || *mode == "reverse-exit" {
		reverseAllowed := map[string]bool{}
		for _, v := range strings.Split(*allow, ",") {
			if identity, ok := strings.CutPrefix(v, "reverse|"); ok {
				reverseAllowed[identity] = true
			}
		}
		hops, e := loadNextHops(*nextHops)
		if e != nil {
			return e
		}
		obfs, e := parseObfuscation(*obfuscation, *obfuscationParams)
		if e != nil {
			return e
		}
		if *mode == "secure-direct" && obfs == nil {
			return errors.New("secure-direct requires -obfuscation-strategy")
		}
		s := &tunnel.Server{Token: os.Getenv("TFP_EXIT_TOKEN"), ReverseAllowed: reverseAllowed, NextHops: hops, NodeID: *nodeID, Obfuscation: obfs, Client: tunnel.Client{TLS: tc}}
		if *mode == "reverse-exit" {
			return s.RunReverse(ctx, tunnel.Client{TLS: tc}, *transport, *reverseEndpoint, *serverName, *nodeID)
		}
		s.TLS, e = certificateConfig(*cert, *key)
		if e != nil {
			return e
		}
		udpErrors := make(chan error, 1)
		if *udpListen != "" {
			if *mode != "exit" || len(hops) > 0 {
				return errors.New("-udp-listen requires an ordinary single exit")
			}
			ds := &tunnel.DatagramServer{TLS: s.TLS, Token: s.Token}
			defer ds.Close()
			if e := ds.Listen(*udpListen); e != nil {
				return e
			}
			go func() {
				if err := ds.ServeBound(); err != nil && ctx.Err() == nil {
					udpErrors <- err
					log.Printf("UDP exit: %v", err)
					stop()
					s.Close()
				}
			}()
			go func() { <-ctx.Done(); ds.Close() }()
		}
		l, e := net.Listen("tcp", *listen)
		if e != nil {
			return e
		}
		defer l.Close()
		go func() { <-ctx.Done(); s.Close() }()
		serveTransport := *transport
		if *mode == "secure-direct" {
			serveTransport = "secure-direct"
		}
		log.Printf("%s %s listening on %s", *mode, serveTransport, l.Addr())
		e = s.Serve(l, serveTransport)
		if ctx.Err() != nil {
			select {
			case udpErr := <-udpErrors:
				return udpErr
			default:
			}
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
