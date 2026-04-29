package crypto

import (
	"bytes"
	"crypto/rand"
	"testing"
)

func TestTOTPSecret_RoundTrip(t *testing.T) {
	t.Parallel()
	master := make([]byte, 32)
	if _, err := rand.Read(master); err != nil {
		t.Fatal(err)
	}
	secret := []byte("01234567890123456789") // 20 bytes RFC 6238

	ct, err := EncryptTOTPSecret(secret, master)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if ct[0] != totpSecretFormatV1 {
		t.Fatalf("expected version v1, got %#x", ct[0])
	}

	plain, err := DecryptTOTPSecret(ct, master)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(plain, secret) {
		t.Fatalf("round trip mismatch: %x vs %x", plain, secret)
	}
}

func TestTOTPSecret_TamperDetected(t *testing.T) {
	t.Parallel()
	master := make([]byte, 32)
	_, _ = rand.Read(master)
	ct, err := EncryptTOTPSecret([]byte("0123456789abcdefghij"), master)
	if err != nil {
		t.Fatal(err)
	}
	// flip 1 bit in ciphertext body
	ct[len(ct)-1] ^= 0x01
	if _, err := DecryptTOTPSecret(ct, master); err == nil {
		t.Fatalf("expected error after tamper")
	}
}

func TestTOTPSecret_WrongKeyRejected(t *testing.T) {
	t.Parallel()
	masterA := make([]byte, 32)
	masterB := make([]byte, 32)
	_, _ = rand.Read(masterA)
	_, _ = rand.Read(masterB)
	ct, err := EncryptTOTPSecret([]byte("0123456789abcdefghij"), masterA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecryptTOTPSecret(ct, masterB); err == nil {
		t.Fatalf("expected error with wrong key")
	}
}

func TestTOTPSecret_TooShortRejected(t *testing.T) {
	t.Parallel()
	master := make([]byte, 32)
	_, _ = rand.Read(master)
	if _, err := DecryptTOTPSecret([]byte{0x01, 0x02}, master); err == nil {
		t.Fatalf("expected error for short ciphertext")
	}
}

func TestTOTPSecret_KeyLengthEnforced(t *testing.T) {
	t.Parallel()
	// 16 バイトは短すぎる
	if _, err := EncryptTOTPSecret([]byte("plain"), make([]byte, 16)); err == nil {
		t.Fatalf("expected error for 16-byte key")
	}
	// 32 バイト以上は OK（先頭 32 バイトを使う、config と整合）
	master := make([]byte, 38)
	_, _ = rand.Read(master)
	ct, err := EncryptTOTPSecret([]byte("0123456789abcdefghij"), master)
	if err != nil {
		t.Fatalf("expected encrypt to succeed with 38-byte key: %v", err)
	}
	if _, err := DecryptTOTPSecret(ct, master); err != nil {
		t.Fatalf("expected decrypt to succeed with 38-byte key: %v", err)
	}
}
