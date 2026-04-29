package http

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"html/template"
	"net/http"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"github.com/okamyuji/sync-files-go/internal/crypto"
	"github.com/okamyuji/sync-files-go/internal/repo/mysql"
	"github.com/okamyuji/sync-files-go/internal/ui"
)

// TOTP setup 中の一時状態 Cookie。secret raw を base32 + HMAC で署名して短命に保持する。
const (
	totpSetupCookieName = "__Host-sync_totp_setup"
	totpSetupCookieTTL  = 15 * time.Minute
)

// totpSetupPageHandler GET /settings/security/totp/setup。
func totpSetupPageHandler(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, u, ok := requireUser(d, w, r)
		if !ok {
			return
		}
		if u.TOTPEnabled {
			_ = d.UI.Render(w, r, http.StatusOK, "settings_totp", &ui.PageData{
				Title:       "2 段階認証",
				CurrentUser: currentUserView(u),
				Extra:       map[string]any{"Enabled": true, "Email": u.Email},
			})
			return
		}

		raw, b32, err := crypto.GenerateTOTPSecret()
		if err != nil {
			internalError(w, d, r.Context(), "totp generate", err)
			return
		}

		signed := signTOTPSetupCookie(raw, d.Cfg.SessionKey)
		http.SetCookie(w, &http.Cookie{
			Name:     totpSetupCookieName,
			Value:    signed,
			Path:     "/", // __Host- prefix の必須要件 (Path=/ かつ Domain 未指定 + Secure)
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteStrictMode,
			MaxAge:   int(totpSetupCookieTTL.Seconds()),
		})

		issuer := "sync-files-go"
		uri := crypto.TOTPProvisioningURI(issuer, u.Email, raw)
		png, err := qrcode.Encode(uri, qrcode.Medium, 256)
		if err != nil {
			internalError(w, d, r.Context(), "totp qr", err)
			return
		}
		// html/template は data: URL を `#ZgotmplZ` にサニタイズしてしまうため、
		// サーバが構築した安全な URL であることを template.URL で明示する。
		// 中身は qrcode.Encode が生成した PNG (バイナリ) を base64 した固定 prefix 付き文字列で、
		// ユーザ入力は混入しないため XSS にならない。
		dataURL := template.URL("data:image/png;base64," + base64.StdEncoding.EncodeToString(png)) // #nosec G203 -- server-built data URL, no user input

		_ = d.UI.Render(w, r, http.StatusOK, "settings_totp", &ui.PageData{
			Title:       "2 段階認証",
			CurrentUser: currentUserView(u),
			Extra: map[string]any{
				"Enabled":      false,
				"Email":        u.Email,
				"OtpAuthURI":   uri,
				"SecretBase32": b32,
				"QRDataURL":    dataURL,
				"Issuer":       issuer,
			},
		})
	}
}

// totpEnableHandler POST /settings/security/totp/enable。
func totpEnableHandler(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, u, ok := requireUser(d, w, r)
		if !ok {
			return
		}
		if u.TOTPEnabled {
			http.Redirect(w, r, "/settings", http.StatusSeeOther)
			return
		}
		if r.PostForm == nil {
			r.Body = http.MaxBytesReader(w, r.Body, uiFormMaxBytes)
			if err := r.ParseForm(); err != nil {
				http.Error(w, "bad form", http.StatusBadRequest)
				return
			}
		}
		code := strings.TrimSpace(r.PostFormValue("code"))
		if len(code) != crypto.TOTPDigits {
			renderTOTPSetupError(d, w, r, u, "6 桁の数字を入力してください")
			return
		}

		c, err := r.Cookie(totpSetupCookieName)
		if err != nil {
			renderTOTPSetupError(d, w, r, u, "セッションが切れました。やり直してください")
			return
		}
		raw, err := verifyTOTPSetupCookie(c.Value, d.Cfg.SessionKey)
		if err != nil {
			renderTOTPSetupError(d, w, r, u, "セッションが無効です。やり直してください")
			return
		}
		if !crypto.VerifyTOTP(raw, code, time.Now()) {
			renderTOTPSetupError(d, w, r, u, "コードが一致しませんでした。Authenticator アプリの時計と再確認してください")
			return
		}

		enc, err := crypto.EncryptTOTPSecret(raw, d.Cfg.AESMasterKey)
		if err != nil {
			internalError(w, d, r.Context(), "totp encrypt", err)
			return
		}
		if err := d.Users.SetTOTP(r.Context(), u.ID, enc, true); err != nil {
			internalError(w, d, r.Context(), "totp save", err)
			return
		}
		_ = d.Audit.Insert(r.Context(), nil, &mysql.AuditEntry{
			ActorID: &sess.UserID, ActorKind: mysql.ActorUser,
			Action: "auth.totp_enable", TargetKind: "user", TargetID: &sess.UserID,
		})

		http.SetCookie(w, &http.Cookie{
			Name: totpSetupCookieName, Value: "", Path: "/",
			Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1,
		})
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
	}
}

// totpDisableHandler POST /settings/security/totp/disable。
//
// パスワード再認証 + 現行 TOTP の両方を要求する（重要な認証要素を外す操作のため）。
func totpDisableHandler(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, u, ok := requireUser(d, w, r)
		if !ok {
			return
		}
		if !u.TOTPEnabled {
			http.Redirect(w, r, "/settings", http.StatusSeeOther)
			return
		}
		if r.PostForm == nil {
			r.Body = http.MaxBytesReader(w, r.Body, uiFormMaxBytes)
			if err := r.ParseForm(); err != nil {
				http.Error(w, "bad form", http.StatusBadRequest)
				return
			}
		}
		password := r.PostFormValue("password")
		code := strings.TrimSpace(r.PostFormValue("code"))

		passOK, err := crypto.VerifyPassword(u.PasswordHash, password)
		if err != nil || !passOK {
			renderTOTPSetupError(d, w, r, u, "パスワードが違います")
			return
		}
		secret, err := unwrapTOTPSecret(u, d.Cfg.AESMasterKey)
		if err != nil {
			internalError(w, d, r.Context(), "totp unwrap", err)
			return
		}
		if !crypto.VerifyTOTP(secret, code, time.Now()) {
			renderTOTPSetupError(d, w, r, u, "認証コードが違います")
			return
		}

		if err := d.Users.SetTOTP(r.Context(), u.ID, nil, false); err != nil {
			internalError(w, d, r.Context(), "totp disable", err)
			return
		}
		_ = d.Audit.Insert(r.Context(), nil, &mysql.AuditEntry{
			ActorID: &sess.UserID, ActorKind: mysql.ActorUser,
			Action: "auth.totp_disable", TargetKind: "user", TargetID: &sess.UserID,
		})
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
	}
}

func renderTOTPSetupError(d *Deps, w http.ResponseWriter, r *http.Request, u *mysql.User, msg string) {
	_ = d.UI.Render(w, r, http.StatusBadRequest, "settings_totp", &ui.PageData{
		Title:       "2 段階認証",
		CurrentUser: currentUserView(u),
		Extra:       map[string]any{"Enabled": u.TOTPEnabled, "Email": u.Email, "Error": msg},
	})
}

// === Setup Cookie 署名 (HMAC-SHA256) ===
// 形式: <b32(raw_secret)>.<b64url(hmac)>

func signTOTPSetupCookie(raw, signKey []byte) string {
	b32 := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
	mac := hmacB64(signKey, []byte(b32))
	return b32 + "." + mac
}

func verifyTOTPSetupCookie(value string, signKey []byte) ([]byte, error) {
	parts := strings.SplitN(value, ".", 2)
	if len(parts) != 2 {
		return nil, errors.New("malformed cookie")
	}
	want := hmacB64(signKey, []byte(parts[0]))
	if subtle.ConstantTimeCompare([]byte(parts[1]), []byte(want)) != 1 {
		return nil, errors.New("hmac mismatch")
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(parts[0])
}

func hmacB64(key, data []byte) string {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}
