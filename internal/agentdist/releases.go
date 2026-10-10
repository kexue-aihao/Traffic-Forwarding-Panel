package agentdist

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

// ReleaseManifest travels with both Agent binaries in images and release bundles.
// The installation's persistent key signs URLs after its public origin is known.
type ReleaseManifest struct {
	Version string        `json:"version"`
	Files   []ReleaseFile `json:"files"`
}

type ReleaseFile struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type Releases struct {
	Manifest ReleaseManifest
	Dir      string
}

func releaseDir(dir string) string {
	if dir != "" {
		return dir
	}
	exe, _ := os.Executable()
	return filepath.Dir(exe)
}

func inspectRelease(dir, arch string) (ReleaseFile, error) {
	f, err := os.Open(filepath.Join(dir, "agent-linux-"+arch))
	if err != nil {
		return ReleaseFile{}, err
	}
	defer f.Close()
	var header [20]byte
	if _, err = io.ReadFull(f, header[:]); err != nil {
		return ReleaseFile{}, err
	}
	machine := uint16(62)
	if arch == "arm64" {
		machine = 183
	}
	if string(header[:6]) != "\x7fELF\x02\x01" || uint16(header[18])|uint16(header[19])<<8 != machine {
		return ReleaseFile{}, errors.New("Agent release ELF platform mismatch")
	}
	if _, err = f.Seek(0, 0); err != nil {
		return ReleaseFile{}, err
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, (256<<20)+1))
	if err != nil || n > 256<<20 {
		return ReleaseFile{}, errors.New("Agent release size invalid")
	}
	return ReleaseFile{OS: "linux", Arch: arch, SHA256: hex.EncodeToString(h.Sum(nil)), Size: n}, nil
}

func WriteReleaseManifest(dir, version string) error {
	m := ReleaseManifest{Version: version}
	for _, arch := range []string{"amd64", "arm64"} {
		f, err := inspectRelease(dir, arch)
		if err != nil {
			return fmt.Errorf("Agent %s: %w", arch, err)
		}
		m.Files = append(m.Files, f)
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "agent-release.json"), append(b, '\n'), 0644)
}

func LoadReleases(dir, version string) (*Releases, error) {
	dir = releaseDir(dir)
	b, err := os.ReadFile(filepath.Join(dir, "agent-release.json"))
	if err != nil {
		return nil, err
	}
	if len(b) > 64<<10 {
		return nil, errors.New("Agent manifest too large")
	}
	var m ReleaseManifest
	if err = json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if version == "" || m.Version != version || len(m.Files) != 2 {
		return nil, errors.New("Agent manifest must match panel version and both architectures")
	}
	for i, arch := range []string{"amd64", "arm64"} {
		actual, err := inspectRelease(dir, arch)
		if err != nil || actual != m.Files[i] {
			return nil, errors.New("Agent manifest checksum/platform mismatch")
		}
	}
	return &Releases{Manifest: m, Dir: dir}, nil
}

func (p *Releases) Upgrade(origin, arch string, key ed25519.PrivateKey) (contract.Upgrade, error) {
	for _, f := range p.Manifest.Files {
		if f.Arch != arch {
			continue
		}
		u := contract.Upgrade{URL: origin + "/download/agent/linux/" + arch + "?version=" + p.Manifest.Version + "&sha256=" + f.SHA256, SHA256: f.SHA256, Version: p.Manifest.Version, OS: f.OS, Arch: f.Arch}
		u.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(key, u.SignedMessage()))
		return u, u.Validate()
	}
	return contract.Upgrade{}, errors.New("Agent release architecture unavailable")
}
