package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

func upgradeFixture(t *testing.T) (*Upgrader, contract.Upgrade, *httptest.Server, []byte) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("test Agent executable bytes")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(payload) }))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	u := &Upgrader{Binary: filepath.Join(dir, "tfp-agent"), Journal: filepath.Join(dir, "upgrade.json"), PublicKey: pub, HTTP: srv.Client()}
	h := sha256.Sum256(payload)
	release := contract.Upgrade{URL: srv.URL, SHA256: hex.EncodeToString(h[:]), Version: "0.1.42", OS: "linux", Arch: "amd64"}
	release.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(key, release.SignedMessage()))
	return u, release, srv, payload
}

func TestUpgradeSignatureDownloadAndRollbackKeepPreviousBinary(t *testing.T) {
	u, release, _, payload := upgradeFixture(t)
	old := []byte("previous Agent")
	if err := os.WriteFile(u.Binary, old, 0700); err != nil {
		t.Fatal(err)
	}
	if err := u.download(context.Background(), release); err != nil {
		t.Fatal(err)
	}
	j := upgradeJournal{Operation: contract.NodeOperation{ID: "upgrade-test", Claim: strings.Repeat("a", 64), Upgrade: &release}, Phase: "staged"}
	if err := u.save(j); err != nil {
		t.Fatal(err)
	}
	if err := u.install(j); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(u.Binary)
	if err != nil || string(b) != string(payload) {
		t.Fatal("verified executable not installed", err)
	}
	j, err = u.load()
	if err != nil || j.Phase != "testing" {
		t.Fatal("upgrade journal did not persist startup validation phase", err)
	}
	if err = u.rollback(j); err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile(u.Binary)
	if err != nil || string(b) != string(old) {
		t.Fatal("rollback failed to restore previous executable", err)
	}
	if _, err = os.Stat(u.Journal + ".result"); err != nil {
		t.Fatal("rollback result not durable")
	}
}

func TestUpgradeRejectsWrongKeyCorruptionRedirectAndChangedStage(t *testing.T) {
	for _, mode := range []string{"wrong-key", "changed-version", "corrupt-download", "redirect", "changed-stage"} {
		t.Run(mode, func(t *testing.T) {
			u, release, srv, _ := upgradeFixture(t)
			switch mode {
			case "wrong-key":
				pub, _, err := ed25519.GenerateKey(rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				u.PublicKey = pub
			case "changed-version":
				release.Version = "0.1.43"
			case "corrupt-download":
				srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("different executable")) })
			case "redirect":
				srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "https://example.invalid", 302) })
			case "changed-stage":
				if err := u.download(context.Background(), release); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(u.Binary+".tfp-next", []byte("tampered"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := u.install(upgradeJournal{Operation: contract.NodeOperation{Upgrade: &release}}); err == nil {
					t.Fatal("changed staged executable installed")
				}
				return
			}
			if err := u.download(context.Background(), release); err == nil {
				t.Fatal("invalid download accepted")
			}
			if _, err := os.Stat(u.Binary + ".tfp-next"); !os.IsNotExist(err) {
				t.Fatal("rejected download published executable")
			}
		})
	}
}
