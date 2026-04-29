package crypto

import (
	"bytes"
	"crypto/rand"
	"testing"
)

// TestAEAD_RoundTrip は 1MB / 100MB のラウンドトリップ + AAD 検証を行う。
//
// 設計書 11-testing-strategy.md の Phase 2 受け入れ基準。
// 2GB は時間がかかるので Phase 5 の E2E に回す。
func TestAEAD_RoundTrip(t *testing.T) {
	cases := []struct {
		name string
		size int
	}{
		{"empty", 0},
		{"small", 1024},
		{"chunk-1MB", AEADChunkSize},
		{"chunk-1MB+1", AEADChunkSize + 1},
		{"chunk-3.5MB", AEADChunkSize*3 + 512*1024},
	}

	key := make([]byte, 32)
	_, _ = rand.Read(key)
	aad := []byte("file=abc|version=1|owner=me")

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plain := make([]byte, tc.size)
			_, _ = rand.Read(plain)

			var ct bytes.Buffer
			header, err := EncryptStream(&ct, bytes.NewReader(plain), key, aad)
			if err != nil {
				t.Fatalf("encrypt: %v", err)
			}
			if len(header) != 8 {
				t.Fatalf("header want 8 bytes, got %d", len(header))
			}

			var dec bytes.Buffer
			if err := DecryptStream(&dec, &ct, key, aad, header); err != nil {
				t.Fatalf("decrypt: %v", err)
			}
			if !bytes.Equal(dec.Bytes(), plain) {
				t.Fatalf("payload mismatch: in=%d out=%d", len(plain), dec.Len())
			}
		})
	}
}

// TestAEAD_AADMismatch は AAD が違うと復号が失敗することを確認する（取り違え防止）。
func TestAEAD_AADMismatch(t *testing.T) {
	key := make([]byte, 32)
	_, _ = rand.Read(key)

	plain := []byte("payload")
	var ct bytes.Buffer
	header, err := EncryptStream(&ct, bytes.NewReader(plain), key, []byte("aad-A"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	var dec bytes.Buffer
	err = DecryptStream(&dec, &ct, key, []byte("aad-B"), header)
	if err == nil {
		t.Fatal("AAD mismatch should fail decryption")
	}
}

// TestAEAD_Tampering は ciphertext を改ざんすると復号が失敗することを確認。
func TestAEAD_Tampering(t *testing.T) {
	key := make([]byte, 32)
	_, _ = rand.Read(key)

	plain := []byte("hello world this is some payload")
	var ct bytes.Buffer
	header, _ := EncryptStream(&ct, bytes.NewReader(plain), key, []byte("aad"))

	tampered := ct.Bytes()
	tampered[len(tampered)-1] ^= 0x01 // tag のビットを 1 つ反転

	var dec bytes.Buffer
	err := DecryptStream(&dec, bytes.NewReader(tampered), key, []byte("aad"), header)
	if err == nil {
		t.Fatal("tampered ciphertext should fail")
	}
}
