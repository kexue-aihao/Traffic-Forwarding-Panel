package agent

import (
	"context"
	"errors"
	"os"
	"strings"
)

// A legacy standalone exit shares the installed executable but is a separate
// systemd process. Refresh it after both replacement and rollback, only when
// the installer explicitly adopted its unit as part of this installation.
func upgradeCompanionActive(binary string) (bool, error) {
	if binary != "/usr/local/bin/tfp-agent" || os.Geteuid() != 0 {
		return false, nil
	}
	owned, err := managedExitOwned("/etc/tfp-agent/managed-exit", "/etc/systemd/system/tfp-exit.service")
	return owned && systemctl(context.Background(), "is-active", "--quiet", "tfp-exit.service") == nil, err
}

func restartUpgradeCompanion(binary string, active bool) error {
	if !active || binary != "/usr/local/bin/tfp-agent" || os.Geteuid() != 0 {
		return nil
	}
	return restartManagedExit(context.Background(), "/etc/tfp-agent/managed-exit", "/etc/systemd/system/tfp-exit.service", systemctl)
}

func managedExitOwned(marker, unit string) (bool, error) {
	for _, path := range []string{marker, unit} {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !info.Mode().IsRegular() {
			return false, nil
		}
	}
	owned, err := os.ReadFile(marker)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(string(owned)) != "tfp-exit.service" {
		return false, nil
	}
	data, err := os.ReadFile(unit)
	if err != nil {
		return false, err
	}
	starts := 0
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "ExecStart=") {
			if !strings.HasPrefix(line, "ExecStart=/usr/local/bin/tfp-agent -mode exit ") && !strings.HasPrefix(line, "ExecStart=/usr/local/bin/tfp-agent -mode secure-direct ") {
				return false, nil
			}
			starts++
		}
	}
	return starts == 1, nil
}

func restartManagedExit(ctx context.Context, marker, unit string, run func(context.Context, ...string) error) error {
	owned, err := managedExitOwned(marker, unit)
	if err != nil {
		return err
	}
	if !owned {
		return nil
	}
	if err = run(ctx, "restart", "tfp-exit.service"); err != nil {
		return err
	}
	return run(ctx, "is-active", "--quiet", "tfp-exit.service")
}
