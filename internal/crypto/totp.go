package crypto

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" // nolint:gosec // RFC 6238 標準
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

// TOTPSecretLen は共通秘密鍵の長さ（20 bytes、設計書 07-security.md §3.2）。
const TOTPSecretLen = 20

// TOTPDigits は表示桁数。
const TOTPDigits = 6

// TOTPInterval はステップ秒数。
const TOTPInterval = 30 * time.Second

// GenerateTOTPSecret は新しい共通秘密鍵を生成して、base32 文字列（手動入力用）と raw bytes を返す。
func GenerateTOTPSecret() (raw []byte, base32Encoded string, err error) {
	raw = make([]byte, TOTPSecretLen)
	if _, err := rand.Read(raw); err != nil {
		return nil, "", err
	}
	base32Encoded = base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
	return raw, base32Encoded, nil
}

// TOTPCode は RFC 6238 互換のコードを生成する（HMAC-SHA1, 6 桁, 30 秒）。
func TOTPCode(secret []byte, when time.Time) string {
	counter := uint64(when.UTC().Unix()) / uint64(TOTPInterval.Seconds())

	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, counter)

	h := hmac.New(sha1.New, secret)
	h.Write(buf)
	sum := h.Sum(nil)

	offset := sum[len(sum)-1] & 0x0f
	bin := (uint32(sum[offset])&0x7f)<<24 |
		uint32(sum[offset+1])<<16 |
		uint32(sum[offset+2])<<8 |
		uint32(sum[offset+3])

	mod := uint32(1)
	for i := 0; i < TOTPDigits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", TOTPDigits, bin%mod)
}

// VerifyTOTP は ±1 ステップ（30 秒）の許容で検証する（時計ずれ対策）。
func VerifyTOTP(secret []byte, code string, when time.Time) bool {
	code = strings.TrimSpace(code)
	if len(code) != TOTPDigits {
		return false
	}
	for offset := -1; offset <= 1; offset++ {
		t := when.Add(time.Duration(offset) * TOTPInterval)
		if hmacEqualString(TOTPCode(secret, t), code) {
			return true
		}
	}
	return false
}

func hmacEqualString(a, b string) bool {
	return hmac.Equal([]byte(a), []byte(b))
}
