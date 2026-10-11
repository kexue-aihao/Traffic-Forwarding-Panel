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
	"os"
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
	EnableUninstall bool
	PanelCA         []byte
	Upgrader        *Upgrader
	usageMu         sync.Mutex
	configMu        sync.Mutex
	probeMu         sync.Mutex
	prefetchMu      sync.Mutex
	probeBootRetry  time.Time
}

type httpStatusError struct {
	method, path string
	status       int
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("%s %s: HTTP %d", e.method, e.path, e.status)
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
		return &httpStatusError{method: method, path: path, status: res.StatusCode}
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(out)
	}
	return nil
}
func (a *Agent) Run(ctx context.Context) error {
	// A failed candidate must remain readable by the previous supervisor's
	// rollback binary. Grow windows only after the supervisor commits health
	// and removes the activation journal, including after an interrupted start.
	if a.Upgrader != nil {
		a.Store.pauseTCPGrowth.Store(true)
	}
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
		reg := contract.Registration{Token: a.EnrollmentToken, Name: a.Name, Version: Version, OS: runtime.GOOS, Arch: runtime.GOARCH, Capabilities: a.capabilities(), UpgradeKeySHA256: a.upgradeKeySHA256()}
		if a.Runtime != nil {
			reg.InspectionProfiles = a.Runtime.InspectionProfileStatuses()
		}
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
	if a.EnableTerminal || a.EnableUninstall || a.Upgrader != nil {
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
		a.runSync(workersCtx, "usage", time.Second, a.Store.usageWake, 25*time.Millisecond, func(ctx context.Context) error {
			err := a.syncUsage(ctx)
			usageHealthy.Store(err == nil)
			return err
		})
	})
	workers.Go(func() {
		a.runSync(workersCtx, "lease prefetch", 100*time.Millisecond, a.Store.leaseWake, 10*time.Millisecond, func(ctx context.Context) error {
			if a.Upgrader != nil && a.Store.pauseTCPGrowth.Load() {
				if _, err := os.Stat(a.Upgrader.Journal); errors.Is(err, os.ErrNotExist) {
					a.Store.pauseTCPGrowth.Store(false)
				}
			}
			return a.syncPrefetch(ctx)
		})
	})
	workers.Go(func() {
		a.runSync(workersCtx, "probe", interval, nil, 0, func(ctx context.Context) error {
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
			var status *httpStatusError
			if errors.As(e, &status) && status.method == "POST" && status.path == "/agent/ack" && status.status == http.StatusConflict {
				// Prefetch/retirement can advance desired_version between GET
				// and ACK. Refresh promptly instead of delaying the pipeline a
				// whole second; retain a small backoff for repeated conflicts.
				wake(a.Store.configWake)
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(10 * time.Millisecond):
				}
				continue
			}
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
	prefetchErr := a.syncPrefetch(ctx)
	return errors.Join(usageErr, configErr, afterErr, probeErr, prefetchErr)
}

func (a *Agent) syncConfig(ctx context.Context) error {
	a.configMu.Lock()
	defer a.configMu.Unlock()
	var c contract.Config
	if e := a.request(ctx, "GET", "/agent/config", nil, &c); e != nil {
		return e
	}
	applyErr := a.Runtime.Apply(c, true)
	ack := contract.Ack{TLSIngressStatuses: a.Runtime.TLSIngressStatuses(), RuleStatuses: a.Runtime.PolicyStatuses(), Services: a.Runtime.Services.Statuses(), Capabilities: a.capabilities(), AgentVersion: Version, Version: c.Version, AppliedVersion: a.Runtime.Version(), UpgradeKeySHA256: a.upgradeKeySHA256()}
	ack.InspectionProfiles = a.Runtime.InspectionProfileStatuses()
	if applyErr != nil {
		ack.Error = applyErr.Error()
	}
	ackErr := a.request(ctx, "POST", "/agent/ack", ack, nil)
	var status *httpStatusError
	if errors.As(ackErr, &status) && status.status == http.StatusConflict {
		wake(a.Store.configWake)
	}
	if applyErr == nil {
		a.Store.requestUsage()
	}
	return errors.Join(applyErr, ackErr)
}

func (a *Agent) syncUsage(ctx context.Context) error {
	a.usageMu.Lock()
	defer a.usageMu.Unlock()
	var errs []error
	errs = append(errs, a.Store.FlushCredits())
	for _, id := range a.Store.renewals() {
		if err := a.Store.Retire(id); err != nil {
			errs = append(errs, err)
		}
	}
	errs = append(errs, a.flush(ctx))
	errs = append(errs, a.retire(ctx))
	return errors.Join(errs...)
}

// Lease prefetch must not queue behind a slow usage POST or retirement. Its
// own worker can provision the next allocation while the current one is spent.
func (a *Agent) syncPrefetch(ctx context.Context) error {
	a.prefetchMu.Lock()
	defer a.prefetchMu.Unlock()
	var errs []error
	for _, request := range a.Store.prefetches() {
		var result struct {
			LeaseID string `json:"lease_id"`
		}
		if e := a.request(ctx, "POST", "/agent/leases/prefetch", request, &result); e != nil {
			var status *httpStatusError
			if errors.As(e, &status) && status.status == http.StatusConflict {
				// A purchase, retirement or concurrent config refresh can replace
				// this allocation. Prefetch is optional; fetch the current policy
				// without turning a successful usage settlement into a failure.
				wake(a.Store.configWake)
			} else {
				errs = append(errs, e)
			}
		} else if result.LeaseID != "" {
			wake(a.Store.configWake)
		}
	}
	return errors.Join(errs...)
}

func (a *Agent) syncProbe(ctx context.Context) error {
	p := a.Probe.Sample(ctx, a.Store.Identity().NodeID)
	return a.sendProbe(ctx, p)
}

func (a *Agent) sendProbe(ctx context.Context, p contract.Probe) error {
	a.probeMu.Lock()
	defer a.probeMu.Unlock()
	if time.Now().Before(a.probeBootRetry) {
		p.BootedAt = nil
	}
	err := a.request(ctx, "POST", "/agent/probe", p, nil)
	var status *httpStatusError
	if p.BootedAt != nil && errors.As(err, &status) && status.status == http.StatusBadRequest {
		// Older panels reject unknown JSON fields. Retry this same sample
		// without the optional boot timestamp; cache only a successful fallback.
		p.BootedAt = nil
		if fallback := a.request(ctx, "POST", "/agent/probe", p, nil); fallback == nil {
			a.probeBootRetry = time.Now().Add(5 * time.Minute)
			return nil
		}
	}
	return err
}

func (a *Agent) runSync(ctx context.Context, name string, interval time.Duration, wake <-chan struct{}, spacing time.Duration, syncOnce func(context.Context) error) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for ctx.Err() == nil {
		started := time.Now()
		if err := syncOnce(ctx); err != nil {
			log.Printf("agent %s: %v", name, err)
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			continue
		}
		// Coalesce a busy stream's notifications into bounded batches instead
		// of issuing a database transaction for every completed credit window.
		if delay := spacing - time.Since(started); delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
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
	return []string{"tcp", "udp", "direct", "direct-tls", "secure-direct", "secure-direct-v1", "tls", "ws", "wss", "http", "chain:3", "resource-limits-v1", "advanced-routing-v1", "shared-tls-ingress-v1", "proxy-protocol-v1", "diagnostics-v1", "block:http", "block:socks", "looking-glass-v1", "probe", "obfuscation-v1", "obfuscation:random-padding", "obfuscation:timing-perturb", "obfuscation:tls-mimic", "udp-credit-v1", "udp-datagram-v1", "lease-set-v1", "group-policy-v2", "inbound-inspection-v1", "http-stream-filter-v1", "peer-address-policy-v1", "route-failover-v1", "managed-services-v1", "reverse-group-v1", "reverse:tls_simple", "tls_simple", "application-inspection-v1", "inspect:socks4-tcp-v1", "inspect:socks5-tcp-v1", "inspect:socks5-udp-structural-v1", "inspect:socks5-udp-associated-v1", "inspect:ss-aead2017-v1", "inspect:ss-aead2022-v1", "inspect:ss-sip023-v1", "inspect:vmess-aead-v1", "inspect:trojan-v1", "business-tls-termination-v1", "staged-inspection-v1"}
}
