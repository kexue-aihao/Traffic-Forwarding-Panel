package contract

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/url"
	"regexp"
	"time"
)

var releaseVersion = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._+-]{0,63}$`)

func (u Upgrade) Validate() error {
	parsed, e := url.Parse(u.URL)
	if e != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || len(u.URL) > 2048 {
		return errors.New("release requires an HTTPS URL without userinfo or fragment")
	}
	hash, e := hex.DecodeString(u.SHA256)
	if e != nil || len(hash) != 32 {
		return errors.New("release requires SHA256")
	}
	sig, e := base64.StdEncoding.DecodeString(u.Signature)
	if e != nil || len(sig) != 64 {
		return errors.New("release requires an Ed25519 signature")
	}
	if !releaseVersion.MatchString(u.Version) || u.OS != "linux" || (u.Arch != "amd64" && u.Arch != "arm64") {
		return errors.New("release requires a version and supported Linux platform")
	}
	return nil
}

type Upgrade struct {
	URL       string `json:"url"`
	SHA256    string `json:"sha256"`
	Signature string `json:"signature"`
	Version   string `json:"version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
}

func (u Upgrade) SignedMessage() []byte {
	return []byte("tfp-agent-release-v1\n" + u.Version + "\n" + u.OS + "\n" + u.Arch + "\n" + u.SHA256 + "\n")
}

type NodeOperation struct {
	ID        string    `json:"id"`
	NodeID    string    `json:"node_id"`
	Kind      string    `json:"kind"`
	Status    string    `json:"status"`
	Claim     string    `json:"claim,omitempty"`
	Upgrade   *Upgrade  `json:"upgrade,omitempty"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}
type OperationResult struct {
	ID     string `json:"id"`
	Claim  string `json:"claim"`
	Status string `json:"status"`
	Error  string `json:"error"`
}

type Control struct {
	Operation *NodeOperation `json:"operation,omitempty"`
	Active    []string       `json:"active"`
}

// TerminalMessage carries complete commands, never shell fragments. Each command
// is audited before execution; output is streamed and is not stored by the panel.
type TerminalMessage struct {
	Type    string `json:"type"`
	Command string `json:"command,omitempty"`
	Data    string `json:"data,omitempty"`
	Code    int    `json:"code,omitempty"`
	Cols    uint16 `json:"cols,omitempty"`
	Rows    uint16 `json:"rows,omitempty"`
}

// ValidShellInput bounds interactive input and terminal dimensions. Keystrokes
// are never retained in the command audit (they may contain passwords).
func ValidShellInput(m TerminalMessage) bool {
	return m.Type == "input" && len(m.Data) > 0 && len(m.Data) <= 4096 ||
		m.Type == "resize" && m.Cols >= 2 && m.Cols <= 500 && m.Rows >= 2 && m.Rows <= 300
}
