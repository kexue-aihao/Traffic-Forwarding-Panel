package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

const RestartExitCode = 75
const maxReleaseSize = 256 << 20

var ErrRestart = errors.New("upgrade restart requested")

type Upgrader struct {
	Binary    string
	Journal   string
	PublicKey ed25519.PublicKey
	HTTP      *http.Client
	Restart   func()
}
type upgradeJournal struct {
	Operation contract.NodeOperation `json:"operation"`
	Phase     string                 `json:"phase"`
}

func LoadReleaseKey(path string) (ed25519.PublicKey, error) {
	data, e := os.ReadFile(path)
	if e != nil {
		return nil, e
	}
	key, e := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
	if e != nil || len(key) != ed25519.PublicKeySize {
		return nil, errors.New("release key must be base64 Ed25519 public key")
	}
	return ed25519.PublicKey(key), nil
}
func (u *Upgrader) verify(release contract.Upgrade) error {
	if e := release.Validate(); e != nil {
		return e
	}
	sig, _ := base64.StdEncoding.DecodeString(release.Signature)
	if len(u.PublicKey) != ed25519.PublicKeySize || !ed25519.Verify(u.PublicKey, release.SignedMessage(), sig) {
		return errors.New("release signature rejected")
	}
	return nil
}
func writeUpgradeFile(path string, data []byte) error {
	f, e := os.CreateTemp(filepath.Dir(path), ".tfp-upgrade-*")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(data)
	}
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e == nil {
		e = closeErr
	}
	if e != nil {
		return e
	}
	if e = os.Rename(name, path); e != nil {
		return e
	}
	return syncDirectory(path)
}
func (u *Upgrader) save(j upgradeJournal) error {
	data, e := json.Marshal(j)
	if e != nil {
		return e
	}
	return writeUpgradeFile(u.Journal, data)
}
func (u *Upgrader) load() (upgradeJournal, error) {
	var j upgradeJournal
	data, e := os.ReadFile(u.Journal)
	if e != nil {
		return j, e
	}
	e = json.Unmarshal(data, &j)
	return j, e
}
func (u *Upgrader) Stage(ctx context.Context, op contract.NodeOperation) error {
	if runtime.GOOS != "linux" || u.Restart == nil || op.Upgrade == nil || op.Upgrade.OS != runtime.GOOS || op.Upgrade.Arch != runtime.GOARCH || op.Upgrade.Version == Version {
		return errors.New("upgrade platform/version or supervisor unavailable")
	}
	if _, e := os.Stat(u.Journal); !errors.Is(e, os.ErrNotExist) {
		return errors.New("upgrade already pending")
	}
	if _, e := os.Stat(u.Journal + ".result"); !errors.Is(e, os.ErrNotExist) {
		return errors.New("previous upgrade result pending")
	}
	if e := u.download(ctx, *op.Upgrade); e != nil {
		return e
	}
	return u.save(upgradeJournal{Operation: op, Phase: "staged"})
}
func (u *Upgrader) download(ctx context.Context, release contract.Upgrade) error {
	if e := u.verify(release); e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	req, e := http.NewRequestWithContext(ctx, "GET", release.URL, nil)
	if e != nil {
		return e
	}
	client := http.Client{}
	if u.HTTP != nil {
		client = *u.HTTP
	}
	client.Timeout = 3 * time.Minute
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, e := client.Do(req)
	if e != nil {
		return e
	}
	defer res.Body.Close()
	if res.StatusCode != 200 || res.ContentLength > maxReleaseSize {
		return errors.New("release download rejected")
	}
	f, e := os.CreateTemp(filepath.Dir(u.Binary), ".tfp-release-*")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	hash := sha256.New()
	n, e := io.Copy(io.MultiWriter(f, hash), io.LimitReader(res.Body, maxReleaseSize+1))
	if e == nil && (n == 0 || n > maxReleaseSize || !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), release.SHA256)) {
		e = errors.New("release checksum mismatch")
	}
	if e == nil {
		e = f.Chmod(0700)
	}
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e == nil {
		e = closeErr
	}
	if e != nil {
		return e
	}
	if e = os.Rename(name, u.Binary+".tfp-next"); e != nil {
		return e
	}
	return syncDirectory(u.Binary)
}
func fileHash(path string) (string, error) {
	f, e := os.Open(path)
	if e != nil {
		return "", e
	}
	defer f.Close()
	h := sha256.New()
	_, e = io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), e
}
func copyExecutable(from, to string) error {
	in, e := os.Open(from)
	if e != nil {
		return e
	}
	defer in.Close()
	out, e := os.CreateTemp(filepath.Dir(to), ".tfp-binary-*")
	if e != nil {
		return e
	}
	name := out.Name()
	defer os.Remove(name)
	_, e = io.Copy(out, in)
	if e == nil {
		e = out.Chmod(0700)
	}
	if e == nil {
		e = out.Sync()
	}
	closeErr := out.Close()
	if e == nil {
		e = closeErr
	}
	if e != nil {
		return e
	}
	if e = os.Rename(name, to); e != nil {
		return e
	}
	return syncDirectory(to)
}
func (u *Upgrader) install(j upgradeJournal) error {
	if j.Operation.Upgrade == nil {
		return errors.New("missing release")
	}
	if e := u.verify(*j.Operation.Upgrade); e != nil {
		return e
	}
	hash, e := fileHash(u.Binary + ".tfp-next")
	if e != nil || !strings.EqualFold(hash, j.Operation.Upgrade.SHA256) {
		return errors.New("staged release changed")
	}
	if e = copyExecutable(u.Binary, u.Binary+".tfp-previous"); e != nil {
		return e
	}
	j.Phase = "installing"
	if e = u.save(j); e != nil {
		return e
	}
	if e = os.Rename(u.Binary+".tfp-next", u.Binary); e != nil {
		return e
	}
	if e = syncDirectory(u.Binary); e != nil {
		return e
	}
	j.Phase = "testing"
	return u.save(j)
}
func (u *Upgrader) finish(j upgradeJournal, status string) error {
	result := contract.OperationResult{ID: j.Operation.ID, Claim: j.Operation.Claim, Status: status}
	data, _ := json.Marshal(result)
	if e := writeUpgradeFile(u.Journal+".result", data); e != nil {
		return e
	}
	if e := os.Remove(u.Journal); e != nil && !errors.Is(e, os.ErrNotExist) {
		return e
	}
	_ = os.Remove(u.Journal + ".health")
	return syncDirectory(u.Journal)
}
func (u *Upgrader) rollback(j upgradeJournal) error {
	if e := copyExecutable(u.Binary+".tfp-previous", u.Binary); e != nil {
		return e
	}
	return u.finish(j, "rolled_back")
}
func (u *Upgrader) Healthy() error {
	j, e := u.load()
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	if j.Phase != "testing" {
		return nil
	}
	if j.Operation.Upgrade == nil || j.Operation.Upgrade.Version != Version {
		return errors.New("running release version differs from signed manifest")
	}
	return writeUpgradeFile(u.Journal+".health", []byte(j.Operation.ID+"\n"+Version))
}
func (u *Upgrader) Report(ctx context.Context, a *Agent) error {
	data, e := os.ReadFile(u.Journal + ".result")
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	var result contract.OperationResult
	if e = json.Unmarshal(data, &result); e != nil {
		return e
	}
	if e = a.request(ctx, "POST", "/agent/control/result", result, nil); e != nil {
		return e
	}
	return os.Remove(u.Journal + ".result")
}

// Supervise runs outside the forwarding worker and holds the previous executable
// until the replacement completes a real authenticated config/ACK/probe cycle.
// An interrupted installation or a failed 60-second startup restores the backup.
func (u *Upgrader) Supervise(ctx context.Context, args []string) error {
	if runtime.GOOS != "linux" {
		return errors.New("managed upgrades require Linux")
	}
	lock := flock.New(u.Journal + ".lock")
	ok, e := lock.TryLock()
	if e != nil {
		return e
	}
	if !ok {
		return errors.New("upgrade supervisor already running")
	}
	defer lock.Unlock()
	j, e := u.load()
	if e == nil && (j.Phase == "testing" || j.Phase == "installing") {
		if e = u.rollback(j); e != nil {
			return e
		}
	} else if e != nil && !errors.Is(e, os.ErrNotExist) {
		return e
	}
	for {
		if ctx.Err() != nil {
			return nil
		}
		candidate := false
		j, e = u.load()
		if e == nil {
			if j.Phase != "staged" {
				return errors.New("invalid upgrade journal phase")
			}
			if e = u.install(j); e != nil {
				current, loadErr := u.load()
				if loadErr != nil {
					return loadErr
				}
				if current.Phase == "installing" || current.Phase == "testing" {
					if err := u.rollback(current); err != nil {
						return err
					}
				} else if err := u.finish(j, "failed"); err != nil {
					return err
				}
			} else {
				candidate = true
			}
		} else if !errors.Is(e, os.ErrNotExist) {
			return e
		}
		childCtx, stop := context.WithCancel(ctx)
		cmd := exec.CommandContext(childCtx, u.Binary, args...)
		cmd.Env = append(os.Environ(), "TFP_AGENT_WORKER=1")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Stdin = os.Stdin
		e = cmd.Start()
		if e != nil {
			stop()
			if candidate {
				if e = u.rollback(j); e != nil {
					return e
				}
				continue
			}
			return e
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		deadline := time.NewTimer(60 * time.Second)
		tick := time.NewTicker(250 * time.Millisecond)
		running := true
		for running {
			select {
			case e = <-done:
				running = false
			case <-ctx.Done():
				stop()
				e = <-done
				running = false
			case <-deadline.C:
				if candidate {
					stop()
					e = <-done
					running = false
				}
			case <-tick.C:
				if candidate {
					data, readErr := os.ReadFile(u.Journal + ".health")
					if readErr == nil && string(data) == j.Operation.ID+"\n"+j.Operation.Upgrade.Version {
						if e = u.finish(j, "succeeded"); e != nil {
							stop()
							<-done
							tick.Stop()
							deadline.Stop()
							return e
						}
						candidate = false
					}
				}
			}
		}
		tick.Stop()
		deadline.Stop()
		stop()
		if candidate {
			if err := u.rollback(j); err != nil {
				return err
			}
			if ctx.Err() != nil {
				return nil
			}
			continue
		}
		if ctx.Err() != nil {
			return nil
		}
		if e != nil && cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == RestartExitCode {
			// The worker requested a supervised replacement after reporting a
			// successful staged upgrade.
			continue
		}
		if _, loadErr := u.load(); loadErr == nil {
			continue
		}
		if e != nil {
			return fmt.Errorf("Agent worker exited: %w", e)
		}
		return nil
	}
}
