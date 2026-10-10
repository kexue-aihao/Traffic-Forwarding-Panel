package detect

import (
	"crypto/aes"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/binary"
	"io"
	"strings"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
	"lukechampine.com/blake3"
)

func detectSS(b []byte, end bool, network string, p compiledProfile, now time.Time, budget *cryptoBudget) Detection {
	if strings.HasPrefix(p.method, "2022-") {
		return ss2022Detect(b, end, network, p, now, budget)
	}
	key := p.keys[0]
	saltSize := len(key)
	if network == "udp" {
		if len(b) < saltSize+16+7 {
			return noMatch("truncated_ss_udp")
		}
		if !budget.take(2) {
			return budgetExceeded()
		}
		a, e := newAEAD(p.method, ss2017Key(key, b[:saltSize]))
		if e != nil {
			return noMatch("cipher_unavailable")
		}
		plain, e := a.Open(nil, make([]byte, a.NonceSize()), b[saltSize:], nil)
		if e != nil {
			return noMatch("authentication_failed")
		}
		_, state := addressLength(plain, 0)
		if state != 0 {
			return noMatch("invalid_ss_udp_address")
		}
		return ssMatch(p)
	}
	if len(b) < saltSize+18 {
		return need(saltSize+18, end)
	}
	if !budget.take(1) {
		return budgetExceeded()
	}
	a, e := newAEAD(p.method, ss2017Key(key, b[:saltSize]))
	if e != nil {
		return noMatch("cipher_unavailable")
	}
	nonce := make([]byte, a.NonceSize())
	pos := saltSize
	address := make([]byte, 0, 259)
	for chunks := 0; chunks < 512; chunks++ {
		if len(b) < pos+18 {
			return need(pos+18, end)
		}
		if !budget.take(1) {
			return budgetExceeded()
		}
		n, e := a.Open(nil, nonce, b[pos:pos+18], nil)
		if e != nil {
			return noMatch("authentication_failed")
		}
		incrementNonce(nonce)
		length := int(binary.BigEndian.Uint16(n))
		if length == 0 || length > 0x3fff {
			return noMatch("invalid_ss_length")
		}
		pos += 18
		if len(b) < pos+length+16 {
			return need(pos+length+16, end)
		}
		if !budget.take(1) {
			return budgetExceeded()
		}
		payload, e := a.Open(nil, nonce, b[pos:pos+length+16], nil)
		if e != nil {
			return noMatch("authentication_failed")
		}
		incrementNonce(nonce)
		pos += length + 16
		address = append(address, payload[:min(len(payload), 259-len(address))]...)
		_, state := addressLength(address, 0)
		if state == 0 {
			return ssMatch(p)
		}
		if state < 0 {
			return noMatch("invalid_ss_address")
		}
	}
	return Detection{Status: Unavailable, Reason: "chunk_budget_exceeded"}
}

func ss2017Key(key, salt []byte) []byte {
	out := make([]byte, len(key))
	_, _ = io.ReadFull(hkdf.New(sha1.New, key, salt, []byte("ss-subkey")), out)
	return out
}
func incrementNonce(n []byte) {
	for i := range n {
		n[i]++
		if n[i] != 0 {
			return
		}
	}
}
func ssMatch(p compiledProfile) Detection {
	return Detection{Protocol: "shadowsocks", Variant: p.variant + ":" + p.method, Status: Match, Evidence: Authenticated, Reason: "authenticated_request"}
}
func timestampOK(b []byte, now time.Time, window int64) bool {
	if len(b) < 8 {
		return false
	}
	t := binary.BigEndian.Uint64(b)
	unix := now.Unix()
	if unix < 0 || t > uint64(^uint64(0)>>1) {
		return false
	}
	delta := int64(t) - unix
	return delta >= -window && delta <= window
}

func ss2022Detect(b []byte, end bool, network string, p compiledProfile, now time.Time, budget *cryptoBudget) Detection {
	if network == "udp" {
		return ss2022UDP(b, p, now, budget)
	}
	key := p.keys[len(p.keys)-1]
	saltSize := len(key)
	identities := (len(p.keys) - 1) * 16
	fixedStart := saltSize + identities
	if len(b) < fixedStart+27 {
		return need(fixedStart+27, end)
	}
	salt := b[:saltSize]
	for i := 0; i < len(p.keys)-1; i++ {
		if !budget.take(2) {
			return budgetExceeded()
		}
		derived := make([]byte, saltSize)
		material := append(append([]byte(nil), p.keys[i]...), salt...)
		blake3.DeriveKey(derived, "shadowsocks 2022 identity subkey", material)
		block, _ := aes.NewCipher(derived)
		plain := make([]byte, 16)
		block.Decrypt(plain, b[saltSize+i*16:saltSize+(i+1)*16])
		hash := blake3.Sum512(p.keys[i+1])
		if subtle.ConstantTimeCompare(plain, hash[:16]) != 1 {
			return noMatch("identity_mismatch")
		}
	}
	if !budget.take(2) {
		return budgetExceeded()
	}
	a, e := newAEAD(p.method, ss2022SessionKey(key, salt))
	if e != nil {
		return noMatch("cipher_unavailable")
	}
	nonce := make([]byte, 12)
	fixed, e := a.Open(nil, nonce, b[fixedStart:fixedStart+27], nil)
	if e != nil {
		return noMatch("authentication_failed")
	}
	if fixed[0] != 0 || !timestampOK(fixed[1:9], now, 30) {
		return noMatch("invalid_ss2022_header")
	}
	length := int(binary.BigEndian.Uint16(fixed[9:11]))
	start := fixedStart + 27
	total := start + length + 16
	if len(b) < total {
		return need(total, end)
	}
	if !budget.take(1) {
		return budgetExceeded()
	}
	nonce[0] = 1
	variable, e := a.Open(nil, nonce, b[start:total], nil)
	if e != nil {
		return noMatch("authentication_failed")
	}
	n, state := addressLength(variable, 0)
	if state != 0 || len(variable) < n+2 {
		return noMatch("invalid_ss2022_address")
	}
	padding := int(binary.BigEndian.Uint16(variable[n : n+2]))
	if len(variable) < n+2+padding || len(variable) == n+2 && padding == 0 {
		return noMatch("invalid_ss2022_padding")
	}
	return ssMatch(p)
}

func ss2022UDP(b []byte, p compiledProfile, now time.Time, budget *cryptoBudget) Detection {
	if len(b) < 16+16+11+7 {
		return noMatch("truncated_ss2022_udp")
	}
	var body []byte
	if !budget.take(3) {
		return budgetExceeded()
	}
	if strings.Contains(p.method, "chacha20") {
		if len(b) < 24+16+16+11+7 {
			return noMatch("truncated_ss2022_udp")
		}
		a, _ := chacha20poly1305.NewX(p.keys[0])
		plain, e := a.Open(nil, b[:24], b[24:], nil)
		if e != nil {
			return noMatch("authentication_failed")
		}
		body = plain[16:]
	} else {
		header := make([]byte, 16)
		block, _ := aes.NewCipher(p.keys[0])
		block.Decrypt(header, b[:16])
		offset := 16
		for i := 0; i < len(p.keys)-1; i++ {
			if !budget.take(1) {
				return budgetExceeded()
			}
			if len(b) < offset+16+16 {
				return noMatch("truncated_identity")
			}
			block, _ = aes.NewCipher(p.keys[i])
			identity := make([]byte, 16)
			block.Decrypt(identity, b[offset:offset+16])
			hash := blake3.Sum512(p.keys[i+1])
			for j := range identity {
				identity[j] ^= header[j]
			}
			if subtle.ConstantTimeCompare(identity, hash[:16]) != 1 {
				return noMatch("identity_mismatch")
			}
			offset += 16
		}
		key := p.keys[len(p.keys)-1]
		a, e := newAEAD(p.method, ss2022SessionKey(key, header[:8]))
		if e != nil {
			return noMatch("cipher_unavailable")
		}
		body, e = a.Open(nil, header[4:16], b[offset:], nil)
		if e != nil {
			return noMatch("authentication_failed")
		}
	}
	if len(body) < 11 || body[0] != 0 || !timestampOK(body[1:9], now, 30) {
		return noMatch("invalid_ss2022_udp_header")
	}
	padding := int(binary.BigEndian.Uint16(body[9:11]))
	if len(body) < 11+padding {
		return noMatch("invalid_ss2022_padding")
	}
	_, state := addressLength(body, 11+padding)
	if state != 0 {
		return noMatch("invalid_ss2022_udp_address")
	}
	return ssMatch(p)
}

func ss2022SessionKey(key, salt []byte) []byte {
	material := append(append([]byte(nil), key...), salt...)
	out := make([]byte, len(key))
	blake3.DeriveKey(out, "shadowsocks 2022 session subkey", material)
	return out
}
