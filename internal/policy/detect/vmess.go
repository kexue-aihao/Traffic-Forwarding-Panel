package detect

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/md5" // VMess legacy/CmdKey wire protocol, not password storage.
	"crypto/subtle"
	"encoding/binary"
	"hash/crc32"
	"hash/fnv"
	"time"

	vmaead "github.com/v2fly/v2ray-core/v5/proxy/vmess/aead"
)

func vmessCommandKey(id []byte) [16]byte {
	return md5.Sum(append(append([]byte(nil), id...), []byte("c48619fe-8f02-49e0-b9e9-edf763e17e21")...))
}

func detectVMess(b []byte, end bool, network string, p compiledProfile, now time.Time, budget *cryptoBudget) Detection {
	if network != "tcp" {
		return Detection{Status: Unavailable, Reason: "vmess_udp_is_inside_stream"}
	}
	if p.legacy {
		return detectVMessLegacy(b, end, p, now, budget)
	}
	if len(b) < 16 {
		return need(16, end)
	}
	if !budget.take(2) {
		return budgetExceeded()
	}
	block := vmaead.NewCipherFromKey(p.vmessKey[:])
	auth := make([]byte, 16)
	block.Decrypt(auth, b[:16])
	if binary.BigEndian.Uint32(auth[12:]) != crc32.ChecksumIEEE(auth[:12]) || !timestampOK(auth[:8], now, 120) {
		return noMatch("authid_prefilter_failed")
	}
	if len(b) < 42 {
		return need(42, end)
	}
	if !budget.take(3) {
		return budgetExceeded()
	}
	lengthAEAD, e := vmessAEAD(p.vmessKey[:], "VMess Header AEAD Key_Length", b[:16], b[34:42])
	if e != nil {
		return noMatch("cipher_unavailable")
	}
	nonce := vmaead.KDF(p.vmessKey[:], "VMess Header AEAD Nonce_Length", string(b[:16]), string(b[34:42]))[:12]
	plain, e := lengthAEAD.Open(nil, nonce, b[16:34], b[:16])
	if e != nil {
		return noMatch("authentication_failed")
	}
	length := int(binary.BigEndian.Uint16(plain))
	if length < 42 || length > 512 {
		return noMatch("invalid_vmess_header_length")
	}
	total := 42 + length + 16
	if len(b) < total {
		return need(total, end)
	}
	if !budget.take(6) {
		return budgetExceeded()
	}
	var id [16]byte
	copy(id[:], b[:16])
	header, _, _, e := vmaead.OpenVMessAEADHeader(p.vmessKey, id, bytes.NewReader(b[16:total]))
	if e != nil {
		return noMatch("authentication_failed")
	}
	if !validVMessHeader(header) {
		return noMatch("invalid_vmess_header")
	}
	variant := "vmess-aead-tcp"
	if header[37] == 2 {
		variant = "vmess-aead-udp-over-stream"
	}
	if header[37] == 3 {
		variant = "vmess-aead-mux"
	}
	return Detection{Protocol: "vmess", Variant: variant, Status: Match, Evidence: Authenticated, Reason: "authenticated_header"}
}

func vmessAEAD(key []byte, path string, id, nonce []byte) (cipher.AEAD, error) {
	k := vmaead.KDF(key, path, string(id), string(nonce))[:16]
	b, e := aes.NewCipher(k)
	if e != nil {
		return nil, e
	}
	return cipher.NewGCM(b)
}

func vmessHeaderSize(b []byte) (int, int) {
	if len(b) < 38 {
		return 0, 38
	}
	if b[0] != 1 || b[36] != 0 || b[37] < 1 || b[37] > 3 || b[34]&0xe0 != 0 {
		return 0, -1
	}
	switch b[35] & 15 {
	case 1, 3, 4, 5, 6:
	default:
		return 0, -1
	}
	if b[37] == 3 {
		n := 38 + int(b[35]>>4) + 4
		if len(b) < n {
			return 0, n
		}
		return n, 0
	}
	if len(b) < 41 {
		return 0, 41
	}
	n := 41
	switch b[40] {
	case 1:
		n += 4
	case 3:
		n += 16
	case 2:
		if len(b) <= 41 {
			return 0, 42
		}
		if b[41] == 0 {
			return 0, -1
		}
		n += 1 + int(b[41])
	default:
		return 0, -1
	}
	n += int(b[35]>>4) + 4
	if len(b) < n {
		return 0, n
	}
	return n, 0
}
func validVMessHeader(b []byte) bool {
	n, state := vmessHeaderSize(b)
	if state != 0 || n != len(b) {
		return false
	}
	h := fnv.New32a()
	h.Write(b[:n-4])
	return h.Sum32() == binary.BigEndian.Uint32(b[n-4:])
}

func detectVMessLegacy(b []byte, end bool, p compiledProfile, now time.Time, budget *cryptoBudget) Detection {
	if len(b) < 16 {
		return need(16, end)
	}
	var timestamp [8]byte
	matched := false
	for delta := int64(-30); delta <= 30; delta++ {
		if !budget.take(1) {
			return budgetExceeded()
		}
		binary.BigEndian.PutUint64(timestamp[:], uint64(now.Unix()+delta))
		h := hmac.New(md5.New, p.keys[0])
		h.Write(timestamp[:])
		if subtle.ConstantTimeCompare(h.Sum(nil), b[:16]) == 1 {
			matched = true
			break
		}
	}
	if !matched {
		return noMatch("legacy_authentication_failed")
	}
	if len(b) < 16+41 {
		return need(16+41, end)
	}
	if !budget.take(1) {
		return budgetExceeded()
	}
	var ivMaterial [32]byte
	for i := 0; i < 4; i++ {
		copy(ivMaterial[8*i:], timestamp[:])
	}
	iv := md5.Sum(ivMaterial[:])
	block, _ := aes.NewCipher(p.vmessKey[:])
	header := make([]byte, min(len(b)-16, 512))
	cipher.NewCFBDecrypter(block, iv[:]).XORKeyStream(header, b[16:16+len(header)])
	n, state := vmessHeaderSize(header)
	if state < 0 {
		return noMatch("invalid_legacy_header")
	}
	if state > 0 {
		return need(16+state, end)
	}
	if !validVMessHeader(header[:n]) {
		return noMatch("invalid_legacy_checksum")
	}
	return Detection{Protocol: "vmess", Variant: "vmess-legacy-auth-token", Status: Match, Evidence: LegacyAuth, Reason: "known_legacy_token_and_header"}
}
