package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

const uninstallDir = "/var/lib/tfp-agent-uninstall"
const uninstallUnit = "/etc/systemd/system/tfp-agent-uninstall.service"
const managedState = "/var/lib/tfp-agent/agent-state.json"

type uninstallJob struct {
	URL    string
	Token  string
	CA     []byte
	Result contract.OperationResult
}

func managedInstall(state string) error {
	exe, e := os.Executable()
	if e != nil {
		return e
	}
	if os.Geteuid() != 0 || filepath.Clean(state) != managedState || exe != "/usr/local/bin/tfp-agent" {
		return errors.New("requires the official root systemd installation")
	}
	for _, path := range []string{"/etc/tfp-agent/managed-install", "/etc/systemd/system/tfp-agent.service"} {
		info, e := os.Lstat(path)
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() {
			return errors.New("invalid managed installation")
		}
	}
	return nil
}

func systemctl(ctx context.Context, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "systemctl", args...).Run()
}

func (a *Agent) stageUninstall(ctx context.Context, op contract.NodeOperation) error {
	if raw, err := os.ReadFile(uninstallDir + "/job.json"); err == nil {
		var job uninstallJob
		if json.Unmarshal(raw, &job) == nil && job.Result.ID == op.ID && job.Result.Claim == op.Claim {
			// Recover the same claim after an Agent restart. The helper may
			// already have removed files; never turn that into a failed task.
			_ = launchUninstallWorker(ctx)
		}
		return nil
	}
	if e := managedInstall(a.Store.path); e != nil {
		return e
	}
	// The panel has reserved the node and requires an acknowledged empty rule
	// set. Flush the remaining WAL usage before the detached worker stops us.
	if e := a.syncConfig(ctx); e != nil {
		return e
	}
	if len(a.Store.Config().Rules) != 0 {
		return errors.New("rules remain")
	}
	if e := a.syncUsage(ctx); e != nil {
		return e
	}
	if len(a.Store.Pending()) != 0 {
		return errors.New("usage upload incomplete")
	}
	if e := os.Mkdir(uninstallDir, 0700); e != nil {
		return e
	}
	staged := false
	defer func() {
		if !staged {
			_ = os.RemoveAll(uninstallDir)
			_ = os.Remove(uninstallUnit)
		}
	}()
	source, e := os.Open("/usr/local/bin/tfp-agent")
	if e != nil {
		return e
	}
	defer source.Close()
	target, e := os.OpenFile(uninstallDir+"/worker", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
	if e != nil {
		return e
	}
	_, e = io.Copy(target, source)
	if e == nil {
		e = target.Sync()
	}
	closeErr := target.Close()
	if e != nil {
		return e
	}
	if closeErr != nil {
		return closeErr
	}
	job := uninstallJob{URL: a.URL, Token: a.Store.Identity().Token, CA: a.PanelCA, Result: contract.OperationResult{ID: op.ID, Claim: op.Claim, Status: "succeeded"}}
	raw, e := json.Marshal(job)
	if e != nil {
		return e
	}
	if e = os.WriteFile(uninstallDir+"/job.json", raw, 0600); e != nil {
		return e
	}
	if e = writeUninstallUnit(); e != nil {
		return e
	}
	if e = systemctl(ctx, "daemon-reload"); e != nil {
		return e
	}
	if e = systemctl(ctx, "enable", "tfp-agent-uninstall.service"); e != nil {
		return e
	}
	// A start response can be lost when this service is stopped by the worker.
	// From here only the durable helper may report the final outcome.
	staged = true
	_ = systemctl(ctx, "start", "--no-block", "tfp-agent-uninstall.service")
	return nil
}

func writeUninstallUnit() error {
	unit := "[Unit]\nDescription=Traffic Forwarding Panel uninstall and acknowledgement\nAfter=network-online.target\nWants=network-online.target\n[Service]\nType=simple\nExecStart=" + uninstallDir + "/worker -mode uninstall-worker\nRestart=on-failure\nRestartSec=15\n[Install]\nWantedBy=multi-user.target\n"
	return os.WriteFile(uninstallUnit, []byte(unit), 0644)
}
func launchUninstallWorker(ctx context.Context) error {
	if e := writeUninstallUnit(); e != nil {
		return e
	}
	if e := systemctl(ctx, "daemon-reload"); e != nil {
		return e
	}
	return systemctl(ctx, "enable", "--now", "tfp-agent-uninstall.service")
}

// RunUninstallWorker lives in its own persistent systemd unit so stopping the
// Agent, a reboot or an unavailable panel cannot lose the final acknowledgement.
func RunUninstallWorker(ctx context.Context) error {
	if os.Geteuid() != 0 {
		return errors.New("uninstall worker requires root")
	}
	raw, e := os.ReadFile(uninstallDir + "/job.json")
	if e != nil {
		return e
	}
	var job uninstallJob
	if e = json.Unmarshal(raw, &job); e != nil {
		return e
	}
	if _, e = os.Stat(uninstallDir + "/completed"); errors.Is(e, os.ErrNotExist) {
		for _, unit := range []string{"tfp-agent.service", "tfp-exit.service"} {
			if _, e = os.Lstat("/etc/systemd/system/" + unit); e == nil {
				if e = systemctl(ctx, "disable", "--now", unit); e != nil {
					return e
				}
			}
		}
		// Remove only known managed files; user certificates and other files in
		// these directories are deliberately preserved.
		paths := []string{"/etc/systemd/system/tfp-agent.service", "/etc/systemd/system/tfp-exit.service", "/usr/local/bin/tfp-agent", "/etc/tfp-agent/agent.env", "/etc/tfp-agent/exit.env", "/etc/tfp-agent/managed-install", managedState, managedState + ".wal", managedState + ".lock"}
		for _, p := range paths {
			if e = os.Remove(p); e != nil && !errors.Is(e, os.ErrNotExist) {
				return e
			}
		}
		if e = systemctl(ctx, "daemon-reload"); e != nil {
			return e
		}
		if e = os.WriteFile(uninstallDir+"/completed", []byte("complete\n"), 0600); e != nil {
			return e
		}
	} else if e != nil {
		return e
	}
	if e = reportUninstall(ctx, job); e != nil {
		return e
	}
	if e = systemctl(ctx, "disable", "tfp-agent-uninstall.service"); e != nil {
		return e
	}
	if e = os.Remove(uninstallUnit); e != nil && !errors.Is(e, os.ErrNotExist) {
		return e
	}
	if e = systemctl(ctx, "daemon-reload"); e != nil {
		return e
	}
	return os.RemoveAll(uninstallDir)
}

func reportUninstall(ctx context.Context, job uninstallJob) error {
	roots, e := x509.SystemCertPool()
	if e != nil {
		roots = x509.NewCertPool()
	}
	if len(job.CA) > 0 && !roots.AppendCertsFromPEM(job.CA) {
		return errors.New("invalid panel CA")
	}
	client := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots}}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	raw, e := json.Marshal(job.Result)
	if e != nil {
		return e
	}
	req, e := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(job.URL, "/")+"/api/v1/agent/control/result", bytes.NewReader(raw))
	if e != nil {
		return e
	}
	req.Header.Set("Authorization", "Bearer "+job.Token)
	req.Header.Set("Content-Type", "application/json")
	res, e := client.Do(req)
	if e != nil {
		return e
	}
	defer res.Body.Close()
	if res.StatusCode != 204 {
		return errors.New("uninstall acknowledgement pending")
	}
	return nil
}
