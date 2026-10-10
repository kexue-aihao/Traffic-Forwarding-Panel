package detect

import (
	"bytes"
	"crypto/aes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/contract"
	ss2017 "github.com/sagernet/sing-shadowsocks/shadowaead"
	vmaead "github.com/v2fly/v2ray-core/v5/proxy/vmess/aead"
)

func blockedLayer(app, mode string, labels ...string) []contract.InboundPolicy {
	return []contract.InboundPolicy{{GroupID: "g", BlockedApps: []string{app}, Inspection: &contract.InspectionPolicy{Version: 1, Profiles: labels, Mode: mode}}}
}

func TestCompleteSOCKSStructuresAndUDPCompatibility(t *testing.T) {
	if d := Structural(append([]byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"), 0, 0, 0, 4, 0, 0, 0, 0, 0), true, "tcp"); d.Status != Match || d.Variant != "h2c" {
		t.Fatalf("h2 appended frames: %+v", d)
	}
	for _, b := range [][]byte{{5}, {5, 0}, {5, 2, 0}, {5, 1, 255}, {4, 3, 0, 80, 127, 0, 0, 1, 0}, {0x05, 0, 0, 0, 0}} {
		if d := Structural(b, true, "tcp"); d.Status == Match {
			t.Fatalf("invalid/partial greeting matched: %v %+v", b, d)
		}
	}
	for _, b := range [][]byte{{5, 1, 0}, {5, 2, 0, 2}, {4, 1, 0, 80, 127, 0, 0, 1, 0}, {4, 1, 0, 80, 0, 0, 0, 1, 'u', 0, 'e', '.', 't', 0}} {
		d := Structural(b, true, "tcp")
		if d.Status != Match || d.Evidence != StructuralEvidence {
			t.Fatalf("complete TCP structure: %+v", d)
		}
	}
	udp := []byte{0, 0, 0, 1, 127, 0, 0, 1, 0, 80, 'd'}
	d := Structural(udp, true, "udp")
	if d.Status != Match || d.Protocol != "socks5" || d.Evidence != StructuralEvidence {
		t.Fatal(d)
	}
	for _, bad := range [][]byte{{5, 1, 0}, {0, 0, 0, 1, 127}, {0, 0, 0, 3, 0, 0, 80}} {
		if Structural(bad, true, "udp").Status == Match {
			t.Fatalf("unrelated UDP classified: %v", bad)
		}
	}
	legacy, e := Prepare([]contract.InboundPolicy{{BlockedApps: []string{"socks"}}}, nil, Scope{Network: "udp"})
	if e != nil {
		t.Fatal(e)
	}
	if !errors.Is(legacy.Decision(legacy.Feed(udp, true, "udp")), ErrDenied) {
		t.Fatal("standard UDP legacy structural policy failed")
	}
	if _, e = Prepare(blockedLayer("socks5", "strict"), nil, Scope{Network: "udp"}); e == nil {
		t.Fatal("strict standalone UDP claimed association")
	}
	p, e := Prepare(blockedLayer("socks5", "strict", "udp"), Profiles{"udp": {Protocol: "socks5", UDPMode: "structural"}}, Scope{Network: "udp"})
	if e != nil {
		t.Fatal(e)
	}
	if !errors.Is(p.Decision(p.Feed(udp, true, "udp")), ErrDenied) {
		t.Fatal("explicit UDP structural mode failed")
	}
	observed, e := Prepare(blockedLayer("socks5", "observe"), nil, Scope{Network: "udp"})
	if e != nil {
		t.Fatal(e)
	}
	d = observed.Feed(udp, true, "udp")
	if d.Evidence != Probable || observed.Decision(d) != nil {
		t.Fatal("unassociated observation became enforced association")
	}
}

func TestPreparePrerequisitesAndScope(t *testing.T) {
	if _, e := Prepare(blockedLayer("vmess", "strict"), nil, Scope{Network: "tcp"}); e == nil {
		t.Fatal("missing UUID prepared")
	}
	profiles := Profiles{"auth": {Protocol: "trojan", Password: "known-pass", RuleIDs: []string{"r"}, GroupIDs: []string{"g"}, Targets: []string{"127.0.0.1:443"}}}
	layers := blockedLayer("trojan", "strict", "auth")
	if _, e := Prepare(layers, profiles, Scope{RuleID: "r", GroupIDs: []string{"g"}, Target: "127.0.0.1:443", Network: "tcp"}); e == nil {
		t.Fatal("raw TLS incorrectly prepared as decrypted")
	}
	for _, s := range []Scope{{RuleID: "other", GroupIDs: []string{"g"}, Target: "127.0.0.1:443", Network: "tcp", Visibility: "tls-plaintext"}, {RuleID: "r", GroupIDs: []string{"other"}, Target: "127.0.0.1:443", Network: "tcp", Visibility: "tls-plaintext"}, {RuleID: "r", GroupIDs: []string{"g"}, Target: "127.0.0.1:444", Network: "tcp", Visibility: "tls-plaintext"}} {
		if _, e := Prepare(layers, profiles, s); e == nil {
			t.Fatal("unauthorized local profile prepared")
		}
	}
	if _, e := Prepare(layers, profiles, Scope{RuleID: "r", GroupIDs: []string{"g"}, Target: "127.0.0.1:443", Network: "tcp", Visibility: "tls-plaintext"}); e != nil {
		t.Fatal(e)
	}
	observed, e := Prepare(blockedLayer("shadowsocks", "observe", "absent"), nil, Scope{Network: "tcp"})
	if e != nil {
		t.Fatal(e)
	}
	d := observed.Feed([]byte{99, 1, 2}, true, "tcp")
	if d.Status != Unavailable || observed.Decision(d) != nil {
		t.Fatal("observe missing prerequisite claimed ready or rejected")
	}
	tooMany := Profiles{}
	labels := []string{}
	for i := 0; i < 17; i++ {
		label := strings.Repeat("a", i+1)
		labels = append(labels, label)
		tooMany[label] = Profile{Protocol: "vmess", UUID: "0581b063-cc43-4719-bb46-736235a2c3e9"}
	}
	if _, e := Prepare(blockedLayer("vmess", "strict", labels...), tooMany, Scope{Network: "tcp"}); e == nil {
		t.Fatal("unbounded credentials allowed")
	}
	if _, e := compileProfile(Profile{Protocol: "shadowsocks", Method: "aes-128-cfb", Password: "legacy"}); e == nil {
		t.Fatal("legacy stream treated as authenticated")
	}
}

func TestTrojanAuthenticationAndInnerUDP(t *testing.T) {
	profile := Profile{Protocol: "trojan", Password: "known-secret"}
	plan, e := Prepare(blockedLayer("trojan", "strict", "auth"), Profiles{"auth": profile}, Scope{Network: "tcp", Visibility: "tls-plaintext"})
	if e != nil {
		t.Fatal(e)
	}
	hash := sha256.Sum224([]byte(profile.Password))
	prefix := make([]byte, 56)
	hex.Encode(prefix, hash[:])
	prefix = append(prefix, 13, 10, 1, 1, 127, 0, 0, 1, 0, 80, 13, 10)
	for _, command := range []byte{1, 3} {
		prefix[58] = command
		s := plan.NewSession()
		for n := 1; n <= len(prefix); n++ {
			d := s.Feed(prefix[:n], n == len(prefix), "tcp")
			if n == len(prefix) {
				if d.Status != Match || d.Evidence != Authenticated || !errors.Is(plan.Decision(d), ErrDenied) {
					t.Fatalf("authenticated Trojan: %+v", d)
				}
			} else if d.Status != NeedMore {
				t.Fatalf("partial Trojan hash classified at %d: %+v", n, d)
			}
		}
	}
	bad := append([]byte(nil), prefix...)
	bad[0] = '0'
	if bad[0] == prefix[0] {
		bad[0] = '1'
	}
	if d := plan.Feed(bad, true, "tcp"); d.Status == Match {
		t.Fatal("unknown credential matched")
	}
	bad = append([]byte(nil), prefix...)
	bad[len(bad)-1] = 0
	if d := plan.Feed(bad, true, "tcp"); d.Status == Match {
		t.Fatal("malformed Trojan CRLF matched")
	}
}

func TestAuthIDCRCIsNotAuthenticationAndClockWindow(t *testing.T) {
	p := Profile{Protocol: "vmess", UUID: "0581b063-cc43-4719-bb46-736235a2c3e9"}
	plan, e := Prepare(blockedLayer("vmess", "strict", "uuid"), Profiles{"uuid": p}, Scope{Network: "tcp"})
	if e != nil {
		t.Fatal(e)
	}
	c, _ := compileProfile(p)
	auth := vmaead.CreateAuthID(c.vmessKey[:], time.Now().Unix())
	wire := append(auth[:], make([]byte, 80)...)
	d := plan.Feed(wire, true, "tcp")
	if d.Status == Match || plan.Decision(d) != nil {
		t.Fatalf("CRC-only AuthID treated as authentication: %+v", d)
	}
	stale := vmaead.CreateAuthID(c.vmessKey[:], time.Now().Add(-10*time.Minute).Unix())
	wire = append(stale[:], make([]byte, 80)...)
	if plan.Feed(wire, true, "tcp").Status == Match {
		t.Fatal("stale timestamp matched")
	}
}

func TestCredentialFailuresCachedAndCryptoBudget(t *testing.T) {
	profiles := Profiles{"key": {Protocol: "shadowsocks", Method: "aes-128-gcm", Password: "known-secret"}}
	plan, e := Prepare(blockedLayer("shadowsocks", "strict", "key"), profiles, Scope{Network: "tcp"})
	if e != nil {
		t.Fatal(e)
	}
	s := plan.NewSession()
	wire := bytes.Repeat([]byte{0x61}, 1000)
	for n := 1; n <= len(wire); n++ {
		s.Feed(wire[:n], false, "tcp")
	}
	if spent := 256 - s.budget.remaining; spent > 2 {
		t.Fatalf("failed credential retried: %d crypto operations", spent)
	}
	if d := s.Feed(wire[:2], false, "tcp"); d.Reason != "non_monotonic_inspection_prefix" {
		t.Fatal("shrinking prefix reused stale detection")
	}
	// A reference AEAD Writer splits a legitimate address into many encrypted
	// chunks. The declared operation budget yields Unavailable, never Match.
	key := bytes.Repeat([]byte{0x41}, 16)
	profile := Profile{Protocol: "shadowsocks", Method: "aes-128-gcm", Key: base64.StdEncoding.EncodeToString(key)}
	salt := bytes.Repeat([]byte{0x22}, 16)
	a, _ := newAEAD(profile.Method, ss2017Key(key, salt))
	var output bytes.Buffer
	output.Write(salt)
	writer := ss2017.NewWriter(&output, a, 0x3fff)
	address := append([]byte{3, 200}, bytes.Repeat([]byte{'a'}, 200)...)
	address = append(address, 0, 80)
	for _, b := range address {
		if _, e = writer.Write([]byte{b}); e != nil {
			t.Fatal(e)
		}
	}
	plan, e = Prepare(blockedLayer("shadowsocks", "strict", "key"), Profiles{"key": profile}, Scope{Network: "tcp"})
	if e != nil {
		t.Fatal(e)
	}
	d := plan.Feed(output.Bytes(), true, "tcp")
	if d.Status != Unavailable || d.Reason != "crypto_attempt_budget_exceeded" {
		t.Fatalf("unbounded chunk authentication: %+v", d)
	}
}

func TestUnknownDeniedAndImmutablePlansConcurrent(t *testing.T) {
	plan, e := Prepare([]contract.InboundPolicy{{Inspection: &contract.InspectionPolicy{Version: 1, Unknown: "deny"}}}, nil, Scope{Network: "tcp"})
	if e != nil {
		t.Fatal(e)
	}
	if e = plan.Decision(plan.Feed([]byte{0x16, 3, 3, 0, 8}, true, "tcp")); !errors.Is(e, ErrDenied) {
		t.Fatal("opaque TLS treated as known application")
	}
	if e = plan.Decision(plan.Feed([]byte("GET / HTTP/1.1\r\nHost: example.test\r\n\r\n"), true, "tcp")); e != nil {
		t.Fatal("known HTTP treated as unknown")
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				d := plan.Feed([]byte{99, 0, 0}, true, "tcp")
				if !errors.Is(plan.Decision(d), ErrDenied) {
					t.Error("concurrent unknown leaked")
				}
			}
		}()
	}
	wg.Wait()
	if plan.Feed(make([]byte, MaxPrefix+1), true, "tcp").Status != Unavailable {
		t.Fatal("prefix limit missing")
	}
}

func TestProfileLoaderStrictScopedAndOpaqueGenerations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profiles.json")
	p := Profiles{"known": {Protocol: "trojan", Password: "top-secret", RuleIDs: []string{"r"}}}
	doc := struct {
		Version  int      `json:"version"`
		Profiles Profiles `json:"profiles"`
	}{1, p}
	b, _ := json.Marshal(doc)
	if e := os.WriteFile(path, b, 0600); e != nil {
		t.Fatal(e)
	}
	loaded, e := LoadProfiles(path)
	if e != nil {
		t.Fatal(e)
	}
	if loaded.Generation() != p.Generation() {
		t.Fatal("unchanged profiles revoked local generation")
	}
	p["known"] = Profile{Protocol: "trojan", Password: "changed-secret"}
	if loaded.Generation() == p.Generation() {
		t.Fatal("credential change preserved generation")
	}
	status := loaded.Statuses()
	if len(status) != 1 || !status[0].Ready || status[0].Protocol != "trojan" {
		t.Fatal(status)
	}
	safe, _ := json.Marshal(status)
	if bytes.Contains(safe, []byte("top-secret")) {
		t.Fatal("status leaked authentication secret")
	}
	for _, bad := range []string{`{"version":1,"profiles":{"bad":{"protocol":"trojan","password":"top-secret","unsupported":true}}}`, `{"version":99,"profiles":{}}`, `{"version":1,"profiles":{"bad":{"protocol":"trojan","password":"top-secret","trojan_hash":"secret"}}}`, `{"version":1,"profiles":{"../bad":{"protocol":"trojan","password":"top-secret"}}}`} {
		os.WriteFile(path, []byte(bad), 0600)
		_, e = LoadProfiles(path)
		if e == nil || strings.Contains(e.Error(), "top-secret") {
			t.Fatal("invalid local profile accepted or secret leaked")
		}
	}
	if _, e = LoadProfiles(filepath.Dir(path)); e == nil {
		t.Fatal("directory read as profile")
	}
}

func TestSSIDHeaderWithoutFinalTagIsNotConfirmed(t *testing.T) {
	key := bytes.Repeat([]byte{0x41}, 16)
	profile := Profile{Protocol: "shadowsocks", Method: "2022-blake3-aes-128-gcm", Key: base64.StdEncoding.EncodeToString(key) + ":" + base64.StdEncoding.EncodeToString(key)}
	plan, e := Prepare(blockedLayer("shadowsocks", "strict", "key"), Profiles{"key": profile}, Scope{Network: "tcp"})
	if e != nil {
		t.Fatal(e)
	}
	b := bytes.Repeat([]byte{0}, 100)
	block, _ := aes.NewCipher(key)
	block.Encrypt(b[16:32], b[16:32])
	if plan.Feed(b, true, "tcp").Status == Match {
		t.Fatal("identity bytes without final authenticated request confirmed")
	}
}

func FuzzBoundedStructuralAndAuthenticatedInspection(f *testing.F) {
	f.Add([]byte{5, 1, 0})
	f.Add([]byte{0, 0, 0, 1, 127, 0, 0, 1, 0, 80})
	f.Add([]byte("GET / HTTP/1.1\r\n"))
	f.Add([]byte{22, 3, 3, 0, 0})
	profiles := Profiles{"ss": {Protocol: "shadowsocks", Method: "aes-128-gcm", Password: "fuzz"}, "vm": {Protocol: "vmess", UUID: "0581b063-cc43-4719-bb46-736235a2c3e9"}}
	layers := []contract.InboundPolicy{{BlockedApps: []string{"socks5", "shadowsocks", "vmess"}, Inspection: &contract.InspectionPolicy{Version: 1, Profiles: []string{"ss", "vm"}}}}
	plan, e := Prepare(layers, profiles, Scope{Network: "tcp"})
	if e != nil {
		f.Fatal(e)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > MaxPrefix+1 {
			b = b[:MaxPrefix+1]
		}
		for _, network := range []string{"tcp", "udp"} {
			d := plan.Feed(b, true, network)
			if d.Status == NeedMore {
				t.Fatal("complete input needs unbounded further bytes")
			}
			if d.Status == Match && d.Evidence == Authenticated && !errors.Is(plan.Decision(d), ErrDenied) {
				t.Fatal("authenticated matching block not enforced")
			}
			Structural(b, true, network)
		}
	})
}

func TestTimestampBoundary(t *testing.T) {
	now := time.Unix(10000, 0)
	for _, offset := range []int64{-31, -30, 0, 30, 31} {
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(now.Unix()+offset))
		if timestampOK(b[:], now, 30) != (offset >= -30 && offset <= 30) {
			t.Fatal(offset)
		}
	}
}
