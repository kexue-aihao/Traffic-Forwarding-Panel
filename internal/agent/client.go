package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/probe"
)

type Agent struct {
	URL             string
	EnrollmentToken string
	Name            string
	HTTP            *http.Client
	Store           *Store
	Runtime         *Runtime
	Probe           *probe.Collector
	PollInterval    time.Duration
	EnableTerminal  bool
	Upgrader        *Upgrader
	usageMu         sync.Mutex
	configMu        sync.Mutex
}

func (a *Agent) request(ctx context.Context, method, path string, body, out any) error {
	var b bytes.Buffer
	if body != nil {
		if e := json.NewEncoder(&b).Encode(body); e != nil {
			return e
		}
	}
	req, e := http.NewRequestWithContext(ctx, method, strings.TrimRight(a.URL, "/")+"/api/v1"+path, &b)
	if e != nil {
		return e
	}
	req.Header.Set("Content-Type", "application/json")
	if token := a.Store.Identity().Token; token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, e := a.HTTP.Do(req)
	if e != nil {
		return e
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("%s %s: HTTP %d", method, path, res.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(out)
	}
	return nil
}
func (a *Agent) Run(ctx context.Context) error {
	u, e := url.Parse(a.URL)
	if e != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1"))) {
		return errors.New("panel requires HTTPS (HTTP permitted only on loopback)")
	}
	if a.HTTP == nil {
		a.HTTP = &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	if a.Probe == nil {
		a.Probe = &probe.Collector{}
	}
	if a.Store.Identity().NodeID == "" {
		if a.EnrollmentToken == "" {
			return errors.New("enrollment token required for first start")
		}
		var registered contract.Registered
		reg := contract.Registration{Token: a.EnrollmentToken, Name: a.Name, Version: Version, OS: runtime.GOOS, Arch: runtime.GOARCH, Capabilities: a.capabilities()}
		if e = a.request(ctx, "POST", "/agent/register", reg, &registered); e != nil {
			return e
		}
		if registered.NodeID == "" || registered.Token == "" {
			return errors.New("invalid registration response")
		}
		if e = a.Store.SetIdentity(registered); e != nil {
			return e
		}
	}
	if cached := a.Store.Config(); cached.NodeID != "" {
		if e = a.Runtime.Apply(cached, false); e != nil {
			log.Printf("cached configuration unavailable: %v", e)
		}
	}
	defer a.Runtime.Close()
	if a.EnableTerminal || a.Upgrader != nil {
		controlCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { defer close(done); a.runControl(controlCtx) }()

		defer func() { cancel(); <-done }()
	}
	diagnosticCtx, diagnosticCancel := context.WithCancel(ctx)
	diagnosticDone := make(chan struct{})
	go func() { defer close(diagnosticDone); a.runDiagnostics(diagnosticCtx) }()
	defer func() { diagnosticCancel(); <-diagnosticDone }()
	// 网络诊断（LookingGlass）。与终端不同，这条通道只做 ping / tcping / mtr，
	// 参数结构化、不经过 shell，所以不需要 -enable-terminal。
	glassCtx, glassCancel := context.WithCancel(ctx)
	glassDone := make(chan struct{})
	go func() { defer close(glassDone); a.runLookingGlass(glassCtx) }()
	defer func() { glassCancel(); <-glassDone }()
	interval := a.PollInterval
	if interval == 0 {
		interval = 5 * time.Second
	}
	// Registration's first configuration attempt precedes probe publication.
	if e = a.syncConfig(ctx); e != nil {
		log.Printf("agent config: %v", e)
	}
	workersCtx, stopWorkers := context.WithCancel(ctx)
	var workers sync.WaitGroup
	var usageHealthy, probeHealthy atomic.Bool
	workers.Go(func() {
		a.runSync(workersCtx, "usage", time.Second, a.Store.usageWake, func(ctx context.Context) error {
			err := a.syncUsage(ctx)
			usageHealthy.Store(err == nil)
			return err
		})
	})
	workers.Go(func() {
		a.runSync(workersCtx, "probe", interval, nil, func(ctx context.Context) error {
			err := a.syncProbe(ctx)
			probeHealthy.Store(err == nil)
			return err
		})
	})
	defer func() { stopWorkers(); workers.Wait() }()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		if e == nil && usageHealthy.Load() && probeHealthy.Load() && a.Upgrader != nil {
			if e = a.Upgrader.Healthy(); e != nil {
				return e
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		case <-a.Store.configWake:
		}
		if e = a.syncConfig(ctx); e != nil {
			log.Printf("agent config: %v", e)
			// Do not let a failing endpoint spin on repeated wakeups.
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(time.Second):
			}
		}
	}
}
func (a *Agent) Step(ctx context.Context) error {
	// Deterministic single-cycle entry point used by tests and embedders.
	// Production runs the three channels independently below.
	usageErr := a.syncUsage(ctx)
	configErr := a.syncConfig(ctx)
	var afterErr error
	if usageErr == nil {
		afterErr = a.syncUsage(ctx)
	}
	probeErr := a.syncProbe(ctx)
	return errors.Join(usageErr, configErr, afterErr, probeErr)
}

func (a *Agent) syncConfig(ctx context.Context) error {
	a.configMu.Lock()
	defer a.configMu.Unlock()
	var c contract.Config
	if e := a.request(ctx, "GET", "/agent/config", nil, &c); e != nil {
		return e
	}
	applyErr := a.Runtime.Apply(c, true)
	ack := contract.Ack{Capabilities: a.capabilities(), AgentVersion: Version, Version: c.Version, AppliedVersion: a.Runtime.Version()}
	if applyErr != nil {
		ack.Error = applyErr.Error()
	}
	ackErr := a.request(ctx, "POST", "/agent/ack", ack, nil)
	if applyErr == nil {
		a.Store.requestUsage()
	}
	return errors.Join(applyErr, ackErr)
}

func (a *Agent) syncUsage(ctx context.Context) error {
	a.usageMu.Lock()
	defer a.usageMu.Unlock()
	var errs []error
	for _, id := range a.Store.renewals() {
		if err := a.Store.Retire(id); err != nil {
			errs = append(errs, err)
		}
	}
	errs = append(errs, a.flush(ctx))
	errs = append(errs, a.retire(ctx))
	return errors.Join(errs...)
}

func (a *Agent) syncProbe(ctx context.Context) error {
	p := a.Probe.Sample(ctx, a.Store.Identity().NodeID)
	return a.request(ctx, "POST", "/agent/probe", p, nil)
}

func (a *Agent) runSync(ctx context.Context, name string, interval time.Duration, wake <-chan struct{}, syncOnce func(context.Context) error) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for ctx.Err() == nil {
		if err := syncOnce(ctx); err != nil {
			log.Printf("agent %s: %v", name, err)
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-wake:
		}
	}
}

func (a *Agent) flush(ctx context.Context) error {
	records := a.Store.Pending()
	for len(records) > 0 {
		n := len(records)
		if n > 500 {
			n = 500
		}
		var result struct {
			Accepted []string `json:"accepted"`
		}
		if e := a.request(ctx, "POST", "/agent/usage", contract.UsageBatch{Records: records[:n]}, &result); e != nil {
			return e
		}
		if len(result.Accepted) == 0 {
			return errors.New("usage not acknowledged")
		}
		sent := map[string]bool{}
		for _, record := range records[:n] {
			sent[record.ID] = true
		}
		for _, id := range result.Accepted {
			if !sent[id] {
				return errors.New("server acknowledged an unsent usage record")
			}
		}
		if e := a.Store.Confirm(result.Accepted); e != nil {
			return e
		}
		records = records[n:]
	}
	return nil
}
func (a *Agent) retire(ctx context.Context) error {
	var errs []error
	pending := map[string]bool{}
	for _, record := range a.Store.Pending() {
		pending[record.LeaseID] = true
	}
	for id, used := range a.Store.Retirements() {
		if pending[id] {
			continue
		}
		req := struct {
			LeaseID string `json:"lease_id"`
			Used    int64  `json:"used_bytes,string"`
		}{id, used}
		if e := a.request(ctx, "POST", "/agent/leases/retire", req, nil); e != nil {
			errs = append(errs, e)
			continue
		}
		if e := a.Store.ConfirmRetired(id); e != nil {
			errs = append(errs, e)
			continue
		}
		wake(a.Store.configWake)
	}
	return errors.Join(errs...)
}

func capabilities() []string {
	return []string{"tcp", "udp", "direct", "direct-tls", "tls", "ws", "wss", "http", "chain:3", "resource-limits-v1", "advanced-routing-v1", "proxy-protocol-v1", "diagnostics-v1", "block:http", "block:socks", "looking-glass-v1", "probe"}
}
