// Package probe samples actual operating-system counters. Unsupported values
// remain nil, never synthetic zeroes. Public IP observations are refreshed
// periodically from configured HTTPS echo services or the built-in curl probe.
package probe

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	gnet "github.com/shirou/gopsutil/v4/net"
)

type Collector struct {
	mu       sync.Mutex
	DiskPath string
	EchoURLs []string
	HTTP     *http.Client
	last     time.Time
	previous map[string]gnet.IOCountersStat
	ips      []contract.IPObservation
	ipSample time.Time
	// CPU 型号不会变，采一次就缓存 —— 每 5 秒读一遍 /proc/cpuinfo 没有意义。
	cpuModel *string
}

const (
	publicIPEchoURL = "https://api.ipify.org"
	publicIPTimeout = 6 * time.Second
	publicIPMaxTime = "5"
)

// runPublicIPCommand is a variable so probe tests can exercise command output
// without requiring network access or a local curl installation.
var runPublicIPCommand = func(ctx context.Context, family string) ([]byte, error) {
	return exec.CommandContext(ctx, "curl", "-"+family+"fsS", "--max-time", publicIPMaxTime, publicIPEchoURL).Output()
}

func (c *Collector) Sample(ctx context.Context, nodeID string) contract.Probe {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now().UTC()
	p := contract.Probe{NodeID: nodeID, SampledAt: now}
	if v, e := cpu.PercentWithContext(ctx, 0, false); e == nil && len(v) > 0 && !c.last.IsZero() {
		p.CPUPercent = &v[0]
	}
	if v, e := mem.VirtualMemoryWithContext(ctx); e == nil {
		p.MemoryUsed = &v.Used
		p.MemoryTotal = &v.Total
	}
	path := c.DiskPath
	if path == "" {
		path = "."
	}
	if v, e := disk.UsageWithContext(ctx, path); e == nil {
		p.DiskUsed = &v.Used
		p.DiskTotal = &v.Total
	}
	if v, e := host.UptimeWithContext(ctx); e == nil {
		p.UptimeSeconds = &v
	}
	if v, e := load.AvgWithContext(ctx); e == nil {
		p.Load1 = &v.Load1
	}
	if c.cpuModel == nil {
		if v, e := cpu.InfoWithContext(ctx); e == nil && len(v) > 0 && v[0].ModelName != "" {
			model := v[0].ModelName
			c.cpuModel = &model
		}
	}
	p.CPUModel = c.cpuModel
	if v, e := mem.SwapMemoryWithContext(ctx); e == nil {
		p.SwapUsed, p.SwapTotal = &v.Used, &v.Total
	}
	if values, e := gnet.IOCountersWithContext(ctx, true); e == nil {
		next := map[string]gnet.IOCountersStat{}
		up, down := float64(0), float64(0)
		var sent, received uint64
		valid := !c.last.IsZero()
		for _, v := range values {
			iface, e := net.InterfaceByName(v.Name)
			if e == nil && iface.Flags&net.FlagLoopback != 0 {
				continue
			}
			next[v.Name] = v
			sent += v.BytesSent
			received += v.BytesRecv
			old, ok := c.previous[v.Name]
			if !ok || v.BytesSent < old.BytesSent || v.BytesRecv < old.BytesRecv {
				valid = false
				continue
			}
			up += float64(v.BytesSent - old.BytesSent)
			down += float64(v.BytesRecv - old.BytesRecv)
		}
		if valid && len(next) > 0 && now.After(c.last) {
			secs := now.Sub(c.last).Seconds()
			up /= secs
			down /= secs
			p.UploadBPS = &up
			p.DownloadBPS = &down
		}
		if len(next) > 0 {
			p.UploadTotal, p.DownloadTotal = &sent, &received
		}
		c.previous = next
	} else {
		c.previous = nil
	}
	c.last = now
	if now.Sub(c.ipSample) >= 10*time.Minute {
		c.ips = nil
		if len(c.EchoURLs) > 0 {
			for _, endpoint := range c.EchoURLs {
				if v, e := c.Observe(ctx, endpoint); e == nil {
					c.ips = append(c.ips, v)
				}
			}
		} else {
			c.ips = c.observeDefault(ctx)
		}
		c.ipSample = now
	}
	p.PublicIPs = append([]contract.IPObservation(nil), c.ips...)
	return p
}

// observeDefault probes the public address seen by an external service. IPv6
// failure is expected on IPv4-only hosts and is intentionally ignored.
func (c *Collector) observeDefault(ctx context.Context) []contract.IPObservation {
	type result struct {
		family string
		ip     contract.IPObservation
		err    error
	}
	results := make(chan result, 2)
	for _, family := range []string{"4", "6"} {
		go func(family string) {
			probeCtx, cancel := context.WithTimeout(ctx, publicIPTimeout)
			defer cancel()
			raw, err := runPublicIPCommand(probeCtx, family)
			if err != nil {
				results <- result{family: family, err: err}
				return
			}
			ip, err := parsePublicIP(strings.TrimSpace(string(raw)))
			if err != nil || (family == "4" && ip.To4() == nil) || (family == "6" && ip.To4() != nil) {
				results <- result{family: family, err: fmt.Errorf("invalid IPv%s public IP", family)}
				return
			}
			results <- result{family: family, ip: contract.IPObservation{
				Address: ip.String(), Family: "ipv" + family, Source: publicIPEchoURL, ObservedAt: time.Now().UTC(),
			}}
		}(family)
	}
	byFamily := map[string]contract.IPObservation{}
	for range 2 {
		v := <-results
		if v.err == nil {
			byFamily[v.family] = v.ip
		}
	}
	out := make([]contract.IPObservation, 0, len(byFamily))
	for _, family := range []string{"4", "6"} {
		if v, ok := byFamily[family]; ok {
			out = append(out, v)
		}
	}
	return out
}

func parsePublicIP(raw string) (net.IP, error) {
	ip := net.ParseIP(strings.TrimSpace(raw))
	if ip == nil || !publicIP(ip) {
		return nil, fmt.Errorf("invalid public IP")
	}
	return ip, nil
}

func publicIP(ip net.IP) bool {
	return ip != nil && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.IsMulticast() && !ip.IsUnspecified()
}

// Observe calls an operator-configured HTTPS service and validates that its
// response is a public unicast IP address.
func (c *Collector) Observe(ctx context.Context, endpoint string) (contract.IPObservation, error) {
	var zero contract.IPObservation
	u, e := url.Parse(endpoint)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return zero, fmt.Errorf("IP echo must be an explicit HTTPS URL")
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	req, e := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if e != nil {
		return zero, e
	}
	res, e := client.Do(req)
	if e != nil {
		return zero, e
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return zero, fmt.Errorf("IP echo status %d", res.StatusCode)
	}
	body, e := io.ReadAll(io.LimitReader(res.Body, 129))
	if e != nil || len(body) > 128 {
		return zero, fmt.Errorf("invalid IP response")
	}
	ip, e := parsePublicIP(string(body))
	if e != nil {
		return zero, fmt.Errorf("invalid IP response")
	}
	family := "ipv6"
	if ip.To4() != nil {
		family = "ipv4"
	}
	return contract.IPObservation{Address: ip.String(), Family: family, Source: endpoint, ObservedAt: time.Now().UTC()}, nil
}
