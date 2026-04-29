package crypto

import (
	"net/url"
	"strings"
	"testing"
)

func TestTOTPProvisioningURI(t *testing.T) {
	t.Parallel()
	secret := []byte("01234567890123456789") // 20 bytes
	uri := TOTPProvisioningURI("sync-files-go", "user@example.com", secret)

	if !strings.HasPrefix(uri, "otpauth://totp/") {
		t.Fatalf("missing scheme: %s", uri)
	}
	u, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	q := u.Query()
	if q.Get("issuer") != "sync-files-go" {
		t.Errorf("issuer: %q", q.Get("issuer"))
	}
	if q.Get("digits") != "6" {
		t.Errorf("digits: %q", q.Get("digits"))
	}
	if q.Get("period") != "30" {
		t.Errorf("period: %q", q.Get("period"))
	}
	if got := q.Get("secret"); got == "" {
		t.Error("secret missing")
	}
	// パスにラベル
	if !strings.Contains(u.Path, "user@example.com") && !strings.Contains(u.EscapedPath(), "user%40example.com") {
		t.Errorf("path missing account: %q (esc=%q)", u.Path, u.EscapedPath())
	}
}
