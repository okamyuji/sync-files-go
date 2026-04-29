package config

import (
	"strings"
	"testing"
)

// TestLoad_RequiresKeys は必須キーが欠けていればエラーになることを確認する。
func TestLoad_RequiresKeys(t *testing.T) {
	t.Setenv("APP_ENV", "local")
	t.Setenv("AES_MASTER_KEY", "")

	if _, err := Load(); err == nil {
		t.Fatal("Load should fail when AES_MASTER_KEY is empty")
	}
}

// TestLoad_DecodesKeys base64 エンコードされた鍵が復号されることを確認する。
func TestLoad_DecodesKeys(t *testing.T) {
	// 32 bytes 以上の鍵（base64）
	const k = "REDACTED_LOCAL_KEY"

	t.Setenv("APP_ENV", "local")
	t.Setenv("AES_MASTER_KEY", k)
	t.Setenv("TOTP_HMAC_KEY", k)
	t.Setenv("CSRF_KEY", k)
	t.Setenv("SESSION_KEY", k)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.AESMasterKey) < 32 {
		t.Fatalf("AESMasterKey too short: %d", len(cfg.AESMasterKey))
	}
}

// TestLoad_ShortKey はキーが 32 bytes 未満ならエラーになることを確認する。
func TestLoad_ShortKey(t *testing.T) {
	t.Setenv("APP_ENV", "local")
	t.Setenv("AES_MASTER_KEY", "c2hvcnQ=") // "short"
	t.Setenv("TOTP_HMAC_KEY", "c2hvcnQ=")
	t.Setenv("CSRF_KEY", "c2hvcnQ=")
	t.Setenv("SESSION_KEY", "c2hvcnQ=")

	_, err := Load()
	if err == nil {
		t.Fatal("Load should fail with short key")
	}
	if !strings.Contains(err.Error(), "32 bytes") {
		t.Fatalf("expected '32 bytes' in error, got: %v", err)
	}
}

// TestMySQLDSN DSN が hostname を可変にして組み立てられることを確認する。
func TestMySQLDSN(t *testing.T) {
	c := &Config{DB: DBConfig{User: "u", Password: "p", Port: 3306, Name: "sync", TLS: "preferred"}}
	got := c.MySQLDSN("primary.example.local")
	if !strings.Contains(got, "@tcp(primary.example.local:3306)/sync") {
		t.Fatalf("unexpected DSN: %s", got)
	}
	if !strings.Contains(got, "tls=preferred") {
		t.Fatalf("DSN missing tls flag: %s", got)
	}
}
