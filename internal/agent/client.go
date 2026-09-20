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
		reg := contract.Registration{Token: a.EnrollmentToken, Name: a.Name, Version: "0.1.0", OS: runtime.GOOS, Arch: runtime.GOARCH, Capabilities: []string{"tcp", "udp", "direct", "tls", "ws", "wss", "http", "block:http", "block:socks", "probe"}}
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
	interval := a.PollInterval
	if interval == 0 {
		interval = 5 * time.Second
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		if e = a.Step(ctx); e != nil {
			log.Printf("agent sync: %v", e)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}
func (a *Agent) Step(ctx context.Context) error {
	// Set a durable no-more-use marker before flushing and returning allocation.
	old := a.Store.Config()
	for _, r := range old.Rules {
		if r.Lease != nil && a.Store.Available(r, old.ValidUntil) != nil {
			if e := a.Store.Retire(r.Lease.ID); e != nil {
				return e
			}
			a.Runtime.StopLease(r.Lease.ID)
		}
	}
	if e := a.flush(ctx); e != nil {
		return e
	}
	if e := a.retire(ctx); e != nil {
		return e
	}
	var c contract.Config
	if e := a.request(ctx, "GET", "/agent/config", nil, &c); e != nil {
		return e
	}
	applyErr := a.Runtime.Apply(c, true)
	ack := contract.Ack{Version: c.Version, AppliedVersion: a.Runtime.Version()}
	if applyErr != nil {
		ack.Error = applyErr.Error()
	}
	if e := a.request(ctx, "POST", "/agent/ack", ack, nil); e != nil {
		return e
	}
	if applyErr != nil {
		return applyErr
	}
	if e := a.flush(ctx); e != nil {
		return e
	}
	if e := a.retire(ctx); e != nil {
		return e
	}
	p := a.Probe.Sample(ctx, a.Store.Identity().NodeID)
	return a.request(ctx, "POST", "/agent/probe", p, nil)
}
func (a *Agent) flush(ctx context.Context) error {
	records := a.Store.Pending()
	for len(records) > 0 {
		n := len(records)
		if n > 100 {
			n = 100
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
			return e
		}
		if e := a.Store.ConfirmRetired(id); e != nil {
			return e
		}
	}
	return nil
}
