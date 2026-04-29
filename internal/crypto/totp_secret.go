package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

// TOTP secret を AES-256-GCM で暗号化する独立ヘルパ。
//
// 設計書 07-security.md §3.2 / §4.1。MasterKey は 32 bytes。
// フォーマット: version(1) || nonce(12) || ciphertext(n) || tag(16)
//   - version 0x01: AES-256-GCM
//
// EncryptStream (chunked) は大容量ファイル向け。TOTP secret は 20 bytes 固定なので
// 1 ブロックの単純 GCM で十分。

// totpSecretFormatV1 暗号文先頭バイト。
const totpSecretFormatV1 byte = 0x01

// EncryptTOTPSecret 共通秘密鍵をマスタ鍵で AES-256-GCM 暗号化する。
//
// AAD: 固定文字列 "totp-secret-v1"（同じ master_key を別用途と取り違えないため）。
// masterKey は 32 バイト以上を受け取り、先頭 32 バイトを AES-256 鍵として使う
// （config 側が「>= 32 bytes」で許容しているため、運用との整合性を取る）。
func EncryptTOTPSecret(plain, masterKey []byte) ([]byte, error) {
	if len(masterKey) < 32 {
		return nil, errors.New("encrypt totp: master key must be at least 32 bytes")
	}
	block, err := aes.NewCipher(masterKey[:32])
	if err != nil {
		return nil, fmt.Errorf("encrypt totp: new cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("encrypt totp: new gcm: %w", err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("encrypt totp: nonce: %w", err)
	}
	ct := aead.Seal(nil, nonce, plain, []byte("totp-secret-v1"))
	out := make([]byte, 0, 1+len(nonce)+len(ct))
	out = append(out, totpSecretFormatV1)
	out = append(out, nonce...)
	out = append(out, ct...)
	return out, nil
}

// DecryptTOTPSecret EncryptTOTPSecret の逆。形式不一致は明確にエラーを返す。
func DecryptTOTPSecret(enc, masterKey []byte) ([]byte, error) {
	if len(masterKey) < 32 {
		return nil, errors.New("decrypt totp: master key must be at least 32 bytes")
	}
	if len(enc) < 1+12+16 {
		return nil, errors.New("decrypt totp: ciphertext too short")
	}
	if enc[0] != totpSecretFormatV1 {
		return nil, fmt.Errorf("decrypt totp: unsupported format version %#x", enc[0])
	}
	nonce := enc[1:13]
	ct := enc[13:]
	block, err := aes.NewCipher(masterKey[:32])
	if err != nil {
		return nil, fmt.Errorf("decrypt totp: new cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("decrypt totp: new gcm: %w", err)
	}
	plain, err := aead.Open(nil, nonce, ct, []byte("totp-secret-v1"))
	if err != nil {
		return nil, fmt.Errorf("decrypt totp: open: %w", err)
	}
	return plain, nil
}
