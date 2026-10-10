package agentdist

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func releaseArtifacts(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, arch := range []string{"amd64", "arm64"} {
		b := make([]byte, 32)
		copy(b, "\x7fELF\x02\x01")
		b[18] = 62
		if arch == "arm64" {
			b[18] = 183
		}
		if err := os.WriteFile(filepath.Join(dir, "agent-linux-"+arch), b, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := WriteReleaseManifest(dir, "0.1.42"); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestBundledAgentManifestAndPinnedDownload(t *testing.T) {
	dir := releaseArtifacts(t)
	p, err := LoadReleases(dir, "0.1.42")
	if err != nil {
		t.Fatal(err)
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		u, err := p.Upgrade("https://panel.example", arch, key)
		if err != nil {
			t.Fatal(err)
		}
		sig, _ := base64.StdEncoding.DecodeString(u.Signature)
		if !ed25519.Verify(pub, u.SignedMessage(), sig) {
			t.Fatal("signature does not bind release")
		}
		w := get(t, dir, u.URL)
		if w.Code != 200 {
			t.Fatal("pinned release unavailable", w.Code)
		}
		if err := os.WriteFile(filepath.Join(dir, "agent-linux-"+arch), []byte("changed after panel startup"), 0755); err != nil {
			t.Fatal(err)
		}
		w = get(t, dir, u.URL)
		if w.Code != 404 {
			t.Fatal("stale release URL served different executable", w.Code)
		}
	}
}

func TestBundledAgentRejectsVersionChecksumAndArchitectureMismatch(t *testing.T) {
	for _, mode := range []string{"version", "checksum", "architecture", "missing"} {
		t.Run(mode, func(t *testing.T) {
			dir := releaseArtifacts(t)
			version := "0.1.42"
			switch mode {
			case "version":
				version = "0.1.43"
			case "checksum":
				if err := os.WriteFile(filepath.Join(dir, "agent-linux-amd64"), []byte("broken"), 0755); err != nil {
					t.Fatal(err)
				}
			case "architecture":
				b, err := os.ReadFile(filepath.Join(dir, "agent-linux-arm64"))
				if err != nil {
					t.Fatal(err)
				}
				b[18] = 62
				if err = os.WriteFile(filepath.Join(dir, "agent-linux-arm64"), b, 0755); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := os.Remove(filepath.Join(dir, "agent-linux-arm64")); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := LoadReleases(dir, version); err == nil {
				t.Fatal("invalid bundle trusted")
			}
		})
	}
}
