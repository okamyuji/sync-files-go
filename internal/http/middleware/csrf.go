package middleware

import (
	"crypto/hmac"
	"crypto/rand"
	"encoding/base64"
	"net/http"
)

// CSRFCookieName double-submit パターンの Cookie 名。
const CSRFCookieName = "__Host-sync_csrf"

// CSRFHeaderName HTMX が送る CSRF トークンヘッダ。
const CSRFHeaderName = "X-CSRF-Token"

// CSRFFormField hidden form field で送る場合の名前（v1 ではヘッダ送信に統一、Phase 5 で再評価）。
const CSRFFormField = "csrf_token"

// CSRF double-submit cookie 方式の CSRF 保護ミドルウェア。
// state-changing メソッド（POST/PUT/PATCH/DELETE）で Cookie とリクエスト側の値を比較する。
func CSRF(signKey []byte) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ensureCSRFCookie(w, r, signKey)

			if !isStateChanging(r.Method) {
				next.ServeHTTP(w, r)
				return
			}
			c, err := r.Cookie(CSRFCookieName)
			if err != nil {
				http.Error(w, "csrf cookie missing", http.StatusForbidden)
				return
			}
			// gosec G120 対応: PostFormValue は ParseForm を呼びサイズ無制限になるので、
			// CSRF 検証は header (X-CSRF-Token) のみを使う。HTMX の hx-headers / JS で
			// CSRF Cookie を読んで X-CSRF-Token に積む方式（Phase 5 テンプレートで配布）。
			submitted := r.Header.Get(CSRFHeaderName)
			if submitted == "" || !hmac.Equal([]byte(c.Value), []byte(submitted)) {
				http.Error(w, "csrf token mismatch", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ensureCSRFCookie まだ Cookie が無ければランダム値を発行する（GET でテンプレ埋め込みできるように）。
func ensureCSRFCookie(w http.ResponseWriter, r *http.Request, _ []byte) {
	if _, err := r.Cookie(CSRFCookieName); err == nil {
		return
	}
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	val := base64.RawURLEncoding.EncodeToString(b)
	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookieName,
		Value:    val,
		Path:     "/",
		HttpOnly: false, // JS（HTMX）から読んで X-CSRF-Token に積むため
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

// CSRFTokenForTemplate ハンドラ内で、テンプレートに埋める CSRF トークンを取り出す。
func CSRFTokenForTemplate(r *http.Request) string {
	if c, err := r.Cookie(CSRFCookieName); err == nil {
		return c.Value
	}
	return ""
}

func isStateChanging(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}
