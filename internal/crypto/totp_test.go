package crypto

import (
	"testing"
	"time"
)

func TestTOTP_RoundTrip(t *testing.T) {
	secret, _, err := GenerateTOTPSecret()
	if err != nil {
		t.Fatalf("GenerateTOTPSecret: %v", err)
	}

	now := time.Date(2026, 4, 29, 14, 32, 0, 0, time.UTC)
	code := TOTPCode(secret, now)

	if !VerifyTOTP(secret, code, now) {
		t.Fatal("self-generated code should verify")
	}
	// ±30 秒は許容
	if !VerifyTOTP(secret, code, now.Add(-25*time.Second)) {
		t.Fatal("should verify within ±30s")
	}
	if !VerifyTOTP(secret, code, now.Add(25*time.Second)) {
		t.Fatal("should verify within ±30s")
	}
	// 大きく外れた時刻では失敗
	if VerifyTOTP(secret, code, now.Add(2*time.Minute)) {
		t.Fatal("should fail when far away")
	}
}

func TestPasswordRoundTrip(t *testing.T) {
	hash, err := HashPassword("super-secret-pass")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if hash == "" {
		t.Fatal("empty hash")
	}
	ok, err := VerifyPassword(hash, "super-secret-pass")
	if err != nil || !ok {
		t.Fatalf("verify same password: ok=%v err=%v", ok, err)
	}
	ok, _ = VerifyPassword(hash, "wrong-pass")
	if ok {
		t.Fatal("wrong password should not verify")
	}
}
