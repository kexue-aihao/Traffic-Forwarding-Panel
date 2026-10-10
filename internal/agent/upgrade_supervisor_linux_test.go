package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

func quoteUpgradeShell(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func awaitUpgradeFile(t *testing.T, path string) []byte {
	t.Helper()
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			return data
		}
		select {
		case <-deadline.C:
			t.Fatal("upgrade did not produce", path)
		case <-tick.C:
		}
	}
}

func TestUpgradeSupervisorReplacementRollbackAndGracefulStop(t *testing.T) {
	for _, healthy := range []bool{true, false} {
		t.Run(map[bool]string{true: "healthy", false: "failed-startup"}[healthy], func(t *testing.T) {
			dir := t.TempDir()
			pub, key, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			u := &Upgrader{Binary: filepath.Join(dir, "agent"), Journal: filepath.Join(dir, "upgrade.json"), PublicKey: pub}
			signalFile := filepath.Join(dir, "interrupted")
			oldStarted := filepath.Join(dir, "old-started")
			trap := "trap " + quoteUpgradeShell("echo graceful > "+quoteUpgradeShell(signalFile)+"; exit 0") + " INT TERM\n"
			wait := "while :; do sleep 0.05; done\n"
			old := []byte("#!/bin/sh\n" + trap + "echo restored > " + quoteUpgradeShell(oldStarted) + "\n" + wait)
			candidate := []byte("#!/bin/sh\nexit 7\n")
			if healthy {
				candidate = []byte("#!/bin/sh\n" + trap + "printf 'supervisor-test\\n0.1.42' > " + quoteUpgradeShell(u.Journal+".health") + "\n" + wait)
			}
			for path, data := range map[string][]byte{u.Binary: old, u.Binary + ".tfp-next": candidate} {
				if err = os.WriteFile(path, data, 0700); err != nil {
					t.Fatal(err)
				}
			}
			hash := sha256.Sum256(candidate)
			release := contract.Upgrade{URL: "https://panel.example/agent", SHA256: hex.EncodeToString(hash[:]), Version: "0.1.42", OS: "linux", Arch: "amd64"}
			release.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(key, release.SignedMessage()))
			if err = u.save(upgradeJournal{Operation: contract.NodeOperation{ID: "supervisor-test", Claim: "claim", Upgrade: &release}, Phase: "staged"}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- u.Supervise(ctx, nil) }()
			t.Cleanup(func() {
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(17 * time.Second):
					t.Error("supervisor did not stop")
				}
			})
			var result contract.OperationResult
			if err = json.Unmarshal(awaitUpgradeFile(t, u.Journal+".result"), &result); err != nil {
				t.Fatal(err)
			}
			want := "rolled_back"
			if healthy {
				want = "succeeded"
			} else {
				awaitUpgradeFile(t, oldStarted)
			}
			if result.Status != want {
				t.Fatalf("result %s, want %s", result.Status, want)
			}
			data, err := os.ReadFile(u.Binary)
			if err != nil {
				t.Fatal(err)
			}
			if healthy && string(data) != string(candidate) || !healthy && string(data) != string(old) {
				t.Fatal("wrong executable retained")
			}
			cancel()
			if string(awaitUpgradeFile(t, signalFile)) != "graceful\n" {
				t.Fatal("worker was not interrupted gracefully")
			}
		})
	}
}

func TestUpgradeCompanionOwnershipAndRestartFailure(t *testing.T) {
	dir := t.TempDir()
	marker, unit := filepath.Join(dir, "managed-exit"), filepath.Join(dir, "tfp-exit.service")
	if err := os.WriteFile(marker, []byte("tfp-exit.service\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"exit", "secure-direct", "custom"} {
		if err := os.WriteFile(unit, []byte("[Service]\nExecStart=/usr/local/bin/tfp-agent -mode "+mode+" -listen :9443\n"), 0644); err != nil {
			t.Fatal(err)
		}
		var calls []string
		run := func(_ context.Context, args ...string) error {
			calls = append(calls, strings.Join(args, " "))
			return nil
		}
		if err := restartManagedExit(context.Background(), marker, unit, run); err != nil {
			t.Fatal(err)
		}
		if mode == "custom" {
			if len(calls) != 0 {
				t.Fatal("custom service restarted")
			}
			continue
		}
		if strings.Join(calls, ";") != "restart tfp-exit.service;is-active --quiet tfp-exit.service" {
			t.Fatal(calls)
		}
		failed := errors.New("restart failed")
		if err := restartManagedExit(context.Background(), marker, unit, func(context.Context, ...string) error { return failed }); !errors.Is(err, failed) {
			t.Fatal("activation failure hidden")
		}
	}
}
