package crypto

import (
	"encoding/base32"
	"fmt"
	"net/url"
)

// TOTPProvisioningURI は Google Authenticator / Authy / 1Password などが解釈する otpauth:// URI を生成する。
//
// 例: otpauth://totp/sync-files-go:user@example.com?secret=...&issuer=sync-files-go&algorithm=SHA1&digits=6&period=30
//
// secret は raw bytes（GenerateTOTPSecret の戻り値の 1 つ目）を渡す。base32 化はこの関数が行う。
func TOTPProvisioningURI(issuer, accountName string, secret []byte) string {
	b32 := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secret)
	v := url.Values{}
	v.Set("secret", b32)
	v.Set("issuer", issuer)
	v.Set("algorithm", "SHA1")
	v.Set("digits", fmt.Sprintf("%d", TOTPDigits))
	v.Set("period", fmt.Sprintf("%d", int(TOTPInterval.Seconds())))
	// label: <issuer>:<account>
	label := url.PathEscape(fmt.Sprintf("%s:%s", issuer, accountName))
	return fmt.Sprintf("otpauth://totp/%s?%s", label, v.Encode())
}
