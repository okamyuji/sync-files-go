// Package crypto はパスワードハッシュ・TOTP・乱数の最小実装を提供する。
//
// 設計書 07-security.md §3.1 / §3.2 を正準とする。
package crypto

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2 のパラメータ（設計書 07-security.md §3.1）。
const (
	argon2Memory      uint32 = 64 * 1024 // 64 MiB
	argon2Iterations  uint32 = 3
	argon2Parallelism uint8  = 2
	argon2KeyLen      uint32 = 32
	argon2SaltLen     int    = 16
)

// HashPassword Argon2id でハッシュ化した文字列（PHC 形式）を返す。
func HashPassword(plain string) (string, error) {
	if plain == "" {
		return "", errors.New("password is empty")
	}
	salt := make([]byte, argon2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(plain), salt, argon2Iterations, argon2Memory, argon2Parallelism, argon2KeyLen)
	return formatArgon2(salt, key), nil
}

// VerifyPassword HashPassword の出力と平文を比較する（定数時間）。
func VerifyPassword(hash, plain string) (bool, error) {
	salt, key, m, t, p, err := parseArgon2(hash)
	if err != nil {
		return false, err
	}
	if len(key) > 1<<24 { // Argon2 KeyLen は 32 bytes 想定。極端に長ければ拒否
		return false, errors.New("argon2: key length too large")
	}
	cand := argon2.IDKey([]byte(plain), salt, t, m, p, uint32(len(key))) // #nosec G115 -- 上で長さ検証済み
	return subtle.ConstantTimeCompare(cand, key) == 1, nil
}

func formatArgon2(salt, key []byte) string {
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		argon2Memory, argon2Iterations, argon2Parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	)
}

func parseArgon2(s string) (salt, key []byte, m, t uint32, p uint8, err error) {
	parts := strings.Split(s, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return nil, nil, 0, 0, 0, fmt.Errorf("invalid argon2 hash")
	}
	if parts[2] != "v=19" {
		return nil, nil, 0, 0, 0, fmt.Errorf("unsupported argon2 version: %s", parts[2])
	}
	var mP int
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &mP); err != nil {
		return nil, nil, 0, 0, 0, fmt.Errorf("invalid argon2 params: %w", err)
	}
	if mP < 1 || mP > 255 {
		return nil, nil, 0, 0, 0, fmt.Errorf("argon2 parallelism out of range: %d", mP)
	}
	p = uint8(mP) // #nosec G115 -- 上で 1..255 に検証済み
	salt, err = base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return nil, nil, 0, 0, 0, fmt.Errorf("invalid salt: %w", err)
	}
	key, err = base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return nil, nil, 0, 0, 0, fmt.Errorf("invalid key: %w", err)
	}
	return salt, key, m, t, p, nil
}
