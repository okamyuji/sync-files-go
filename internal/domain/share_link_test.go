package domain

import (
	"bytes"
	"encoding/base64"
	"testing"
	"time"
)

func TestGenerateShareToken(t *testing.T) {
	plain, hash, err := GenerateShareToken()
	if err != nil {
		t.Fatalf("GenerateShareToken: %v", err)
	}
	// base64url で 32 bytes → 43 文字（パディングなし）
	raw, err := base64.RawURLEncoding.DecodeString(plain)
	if err != nil || len(raw) != 32 {
		t.Fatalf("plain not 32 bytes base64url: %v len=%d", err, len(raw))
	}
	if len(hash) != 32 {
		t.Fatalf("hash not 32 bytes: %d", len(hash))
	}
	// 同じ平文を hash しても一致する
	if !bytes.Equal(hash, HashShareToken(plain)) {
		t.Fatalf("hash mismatch")
	}

	// 2 回生成すると別の token
	plain2, _, _ := GenerateShareToken()
	if plain == plain2 {
		t.Fatal("token must be random per call")
	}
}

func TestExpiresInOption_Duration(t *testing.T) {
	cases := map[ExpiresInOption]time.Duration{
		ExpiresIn1Hour: time.Hour,
		ExpiresIn1Day:  24 * time.Hour,
		ExpiresIn7Days: 7 * 24 * time.Hour,
	}
	for opt, want := range cases {
		got, ok := opt.Duration()
		if !ok || got != want {
			t.Fatalf("%s: want %v got %v", opt, want, got)
		}
	}
	// v1 では「期限なし」を許さない
	if _, ok := ExpiresInOption("none").Duration(); ok {
		t.Fatal("v1 must not accept 'none'")
	}
}

func TestShareLink_IsActive(t *testing.T) {
	now := time.Now()
	revoked := now.Add(-1 * time.Hour)

	// 期限内・取り消されていない → active
	s := &ShareLink{ExpiresAt: now.Add(1 * time.Hour)}
	if !s.IsActive(now) {
		t.Fatal("should be active")
	}

	// 取り消し済み
	s.RevokedAt = &revoked
	if s.IsActive(now) {
		t.Fatal("revoked should not be active")
	}

	// 期限切れ
	s.RevokedAt = nil
	s.ExpiresAt = now.Add(-1 * time.Second)
	if s.IsActive(now) {
		t.Fatal("expired should not be active")
	}
}
