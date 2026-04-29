package middleware

import (
	"crypto/hmac"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strings"
)

// CSRFCookieName double-submit パターンの Cookie 名。
const CSRFCookieName = "__Host-sync_csrf"

// CSRFHeaderName HTMX や JS が送る CSRF トークンヘッダ。
const CSRFHeaderName = "X-CSRF-Token"

// CSRFFormField hidden form フィールドで送る場合の名前（プログレッシブエンハンスメント用）。
const CSRFFormField = "_csrf"

// csrfMaxFormBytes フォーム解析時の上限。CSRF 検証用フォーム値（数百バイト）に十分な余裕。
// gosec G120 (ParseForm がサイズ無制限) 対策。
const csrfMaxFormBytes int64 = 64 << 10

// CSRF double-submit cookie 方式の CSRF 保護ミドルウェア。
//
// state-changing メソッド（POST/PUT/PATCH/DELETE）で Cookie とリクエスト側の値を比較する。
// 検証順:
//  1. X-CSRF-Token ヘッダ（HTMX / fetch / XHR 用）
//  2. application/x-www-form-urlencoded フォームの `_csrf` フィールド（JS 無効ブラウザ用）
//
// multipart/form-data はサイズが任意で大きいため、CSRF はヘッダで送る運用に統一する
// （ファイルアップロードフォームは HTMX で hx-post を使い、app.js がヘッダを積む）。
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
			submitted := r.Header.Get(CSRFHeaderName)
			if submitted == "" {
				submitted = csrfTokenFromForm(r)
			}
			if submitted == "" || !hmac.Equal([]byte(c.Value), []byte(submitted)) {
				http.Error(w, "csrf token mismatch", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// csrfTokenFromForm  Content-Type が application/x-www-form-urlencoded の場合のみ、
// MaxBytesReader で上限を強制したうえで `_csrf` を取り出す。
//
// 取り出した body は ParseForm により消費されるが、後段ハンドラは r.PostFormValue を再呼び出しできる
// （ParseForm 結果は r.PostForm にキャッシュされる）。
func csrfTokenFromForm(r *http.Request) string {
	if !isFormURLEncoded(r.Header.Get("Content-Type")) {
		return ""
	}
	r.Body = http.MaxBytesReader(nil, r.Body, csrfMaxFormBytes)
	if err := r.ParseForm(); err != nil {
		return ""
	}
	return r.PostFormValue(CSRFFormField)
}

// isFormURLEncoded "application/x-www-form-urlencoded[; charset=...]" を判定。
func isFormURLEncoded(ct string) bool {
	if ct == "" {
		return false
	}
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.TrimSpace(ct) == "application/x-www-form-urlencoded"
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
