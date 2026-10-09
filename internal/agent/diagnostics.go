package agent

import (
	"context"
	"encoding/json"
	"net"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

func (a *Agent) diagnosticChecks(ctx context.Context, d contract.Diagnostic) []contract.DiagnosticCheck {
	checks := []contract.DiagnosticCheck{}
	var rule *contract.Rule
	cfg := a.Store.Config()
	for _, r := range cfg.Rules {
		if r.ID == d.RuleID && r.Version == d.RuleVersion && r.Enabled && a.Store.Available(r, cfg.ValidUntil) == nil {
			v := r
			rule = &v
			break
		}
	}
	checks = append(checks, contract.DiagnosticCheck{Stage: "entry_config", OK: rule != nil})
	if stats, ok := a.Runtime.UDPStats()[d.RuleID]; ok {
		detail, _ := json.Marshal(stats)
		checks = append(checks, contract.DiagnosticCheck{Stage: "udp_counters", OK: true, Detail: string(detail)})
	}
	if rule == nil {
		return checks
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if rule.Network == "udp" {
		if rule.Transport == "quic" {
			stats, _ := json.Marshal(a.Runtime.Datagrams.Stats())
			checks = append(checks, contract.DiagnosticCheck{Stage: "udp_carrier_counters", OK: true, Detail: string(stats)})
			start := time.Now()
			conn, e := a.Runtime.Datagrams.Dial(ctx, a.Runtime.Client.TLS, rule.Tunnel.Endpoint, rule.Tunnel.ServerName, rule.Tunnel.Token, rule.Target)
			if conn != nil {
				conn.Close()
			}
			checks = append(checks, contract.DiagnosticCheck{Stage: "udp_datagram", OK: e == nil, Milliseconds: time.Since(start).Milliseconds(), Detail: "QUIC DATAGRAM session; target UDP delivery requires an application response"})
		} else {
			detail := "UDP over TCP"
			if rule.Transport == "direct" {
				detail = "direct UDP"
			}
			checks = append(checks, contract.DiagnosticCheck{Stage: "udp_transport", OK: true, Detail: detail + "; target delivery requires an application response"})
		}
		return checks
	}
	if rule.Transport == "direct" {
		host, port, _ := net.SplitHostPort(rule.Target)
		start := time.Now()
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		checks = append(checks, contract.DiagnosticCheck{Stage: "entry_dns", OK: err == nil && len(ips) > 0, Milliseconds: time.Since(start).Milliseconds()})
		if err != nil || len(ips) == 0 {
			return checks
		}
		start = time.Now()
		conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(ips[0].IP.String(), port))
		if conn != nil {
			conn.Close()
		}
		checks = append(checks, contract.DiagnosticCheck{Stage: "entry_target", OK: err == nil, Milliseconds: time.Since(start).Milliseconds()})
	} else {
		start := time.Now()
		conn, err := a.Runtime.Client.DialRoute(ctx, rule.Transport, "tcp", rule.Target, *rule.Tunnel)
		if conn != nil {
			conn.Close()
		}
		checks = append(checks, contract.DiagnosticCheck{Stage: "exit_path", OK: err == nil, Milliseconds: time.Since(start).Milliseconds()})
	}
	return checks
}
func (a *Agent) runDiagnostics(ctx context.Context) {
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	var result *contract.Diagnostic
	for {
		if result != nil && time.Since(result.CreatedAt) > 2*time.Minute {
			result = nil
		}
		if result != nil {
			if a.request(ctx, "POST", "/agent/diagnostics/result", result, nil) == nil {
				result = nil
			}
		}
		if result == nil {
			var out struct {
				Diagnostic *contract.Diagnostic `json:"diagnostic"`
			}
			if a.request(ctx, "POST", "/agent/diagnostics", nil, &out) == nil && out.Diagnostic != nil {
				d := out.Diagnostic
				d.Checks = a.diagnosticChecks(ctx, *d)
				result = d
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
