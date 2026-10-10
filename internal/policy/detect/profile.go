// Package detect implements bounded, evidence-based business protocol inspection.
// It never sends probes or modifies the inspected bytes.
package detect

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"slices"
	"strings"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
)

const MaxProfiles = 256
const MaxCredentials = 16
const MaxPrefix = 65535

// Profile is local-only. In particular TrojanHash is an authentication secret.
// Key is a base64 master key (AEAD2017) or colon-delimited PSK chain (2022).
type Profile struct {
	Protocol       string   `json:"protocol"`
	Method         string   `json:"method,omitempty"`
	Password       string   `json:"password,omitempty"`
	Key            string   `json:"key,omitempty"`
	UUID           string   `json:"uuid,omitempty"`
	TrojanHash     string   `json:"trojan_hash,omitempty"`
	UDPMode        string   `json:"udp_mode,omitempty"`
	ControlRuleIDs []string `json:"control_rule_ids,omitempty"`
	RelayEndpoint  string   `json:"relay_endpoint,omitempty"`
	RuleIDs        []string `json:"rule_ids,omitempty"`
	GroupIDs       []string `json:"group_ids,omitempty"`
	Targets        []string `json:"targets,omitempty"`
}

// Profiles is immutable after loading. Prepare copies and compiles selected data.
type Profiles map[string]Profile

func validLabel(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func LoadProfiles(path string) (Profiles, error) {
	if path == "" {
		return Profiles{}, nil
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, errors.New("inspection profile file unavailable")
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return nil, errors.New("inspection profile file must be regular and at most 1 MiB")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("inspection profile file must not be accessible by group or others")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("inspection profile file unavailable")
	}
	defer f.Close()
	var v struct {
		Version  int      `json:"version"`
		Profiles Profiles `json:"profiles"`
	}
	d := json.NewDecoder(io.LimitReader(f, (1<<20)+1))
	d.DisallowUnknownFields()
	if err = d.Decode(&v); err != nil {
		return nil, errors.New("invalid inspection profile document")
	}
	if d.Decode(new(any)) != io.EOF || v.Version != 1 || len(v.Profiles) > MaxProfiles {
		return nil, errors.New("invalid inspection profile version or count")
	}
	for label, p := range v.Profiles {
		if !validLabel(label) {
			return nil, errors.New("invalid inspection profile label")
		}
		if _, err = compileProfile(p); err != nil {
			return nil, fmt.Errorf("inspection profile %s: %w", label, err)
		}
	}
	return v.Profiles, nil
}

var generationSalt = func() []byte {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		panic("inspection generation entropy unavailable")
	}
	return b
}()

// Generation is an opaque process-local HMAC. Do not expose it to the control
// plane; it only invalidates local plans/caches when their credentials change.
func (p Profiles) Generation() string {
	b, _ := json.Marshal(p)
	h := hmac.New(sha256.New, generationSalt)
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

func (p Profiles) Statuses() []contract.InspectionProfileStatus {
	labels := make([]string, 0, len(p))
	for label := range p {
		labels = append(labels, label)
	}
	slices.Sort(labels)
	out := make([]contract.InspectionProfileStatus, 0, len(labels))
	for _, label := range labels {
		profile := p[label]
		c, e := compileProfile(profile)
		s := contract.InspectionProfileStatus{Label: label, Protocol: profile.Protocol, Ready: e == nil}
		if e != nil {
			s.Reason = "profile_invalid"
		} else {
			s.Variants = []string{c.variant}
			s.Networks = c.networks
		}
		out = append(out, s)
	}
	return out
}

func matchesScope(p Profile, s Scope) bool {
	if len(p.RuleIDs) > 0 && !slices.Contains(p.RuleIDs, s.RuleID) {
		return false
	}
	if len(p.Targets) > 0 && !slices.Contains(p.Targets, s.Target) {
		return false
	}
	if len(p.GroupIDs) > 0 {
		found := false
		for _, g := range s.GroupIDs {
			found = found || slices.Contains(p.GroupIDs, g)
		}
		if !found {
			return false
		}
	}
	return true
}

func profileSecretBounds(p Profile) error {
	if len(p.Password) > 4096 || len(p.Key) > 4096 || len(p.UUID) > 64 || len(p.TrojanHash) > 56 {
		return errors.New("credential size exceeds limit")
	}
	if len(p.RuleIDs)+len(p.GroupIDs)+len(p.Targets)+len(p.ControlRuleIDs) > 256 {
		return errors.New("profile scope count exceeds limit")
	}
	if len(p.RelayEndpoint) > 512 {
		return errors.New("invalid relay endpoint")
	}
	for _, a := range [][]string{p.RuleIDs, p.GroupIDs, p.Targets, p.ControlRuleIDs} {
		for _, s := range a {
			if s == "" || len(s) > 512 || strings.ContainsAny(s, "\x00\r\n") {
				return errors.New("invalid profile scope")
			}
		}
	}
	return nil
}

func decodeTrojanHash(p Profile) ([]byte, error) {
	if (p.Password == "") == (p.TrojanHash == "") {
		return nil, errors.New("exactly one Trojan password or hash required")
	}
	if p.Password != "" {
		h := sha256.Sum224([]byte(p.Password))
		out := make([]byte, 56)
		hex.Encode(out, h[:])
		return out, nil
	}
	b, e := hex.DecodeString(p.TrojanHash)
	if e != nil || len(b) != 28 {
		return nil, errors.New("Trojan hash must contain 56 hexadecimal characters")
	}
	out := make([]byte, 56)
	hex.Encode(out, b)
	return out, nil
}

// redact does not interpolate untrusted secret values into preparation errors.
func requireEmpty(values ...string) bool {
	for _, v := range values {
		if len(bytes.TrimSpace([]byte(v))) > 0 {
			return false
		}
	}
	return true
}
