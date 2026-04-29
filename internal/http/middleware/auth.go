package middleware

import (
	"context"
	"crypto/hmac"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// SessionCookieName ブラウザセッション用 Cookie 名（07-security.md §3.3）。
const SessionCookieName = "__Host-sync_session"

// SessionLookup セッション ID から有効なセッションかを確認するためのコールバック。
// MySQL の sessions テーブルを引く役割は呼び出し側（handler 層）に委ねる。
type SessionLookup func(ctx context.Context, sessionID uuid.UUID) (UserSession, bool)

// UserSession 認証済みユーザのセッション情報。ハンドラから ctx で取得する。
type UserSession struct {
	SessionID uuid.UUID
	UserID    uuid.UUID
	ExpiresAt time.Time
}

type ctxKeySession struct{}

// SessionFrom ハンドラ内で「現在ログイン中のセッション」を取り出す。
func SessionFrom(ctx context.Context) (UserSession, bool) {
	v, ok := ctx.Value(ctxKeySession{}).(UserSession)
	return v, ok
}

// SessionIDFromCookie 受信した Cookie の HMAC を検証し、session UUID を返す。
// 検証失敗（HMAC 不一致 / 形式不正）はエラーを返す。
func SessionIDFromCookie(r *http.Request, signKey []byte) (uuid.UUID, error) {
	c, err := r.Cookie(SessionCookieName)
	if err != nil {
		return uuid.Nil, err
	}
	parts := strings.SplitN(c.Value, ".", 2)
	if len(parts) != 2 {
		return uuid.Nil, errors.New("session cookie malformed")
	}
	sid, err := uuid.Parse(parts[0])
	if err != nil {
		return uuid.Nil, err
	}
	expected := hmacSHA256(signKey, sid[:])
	got, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return uuid.Nil, err
	}
	if !hmac.Equal(expected, got) {
		return uuid.Nil, errors.New("session cookie hmac mismatch")
	}
	return sid, nil
}

// SetSessionCookie 認証成功時に呼ぶ。HMAC 署名済み Cookie を発行。
func SetSessionCookie(w http.ResponseWriter, sid uuid.UUID, signKey []byte, expires time.Time) {
	mac := hmacSHA256(signKey, sid[:])
	value := sid.String() + "." + base64.RawURLEncoding.EncodeToString(mac)
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		Expires:  expires,
	})
}

// ClearSessionCookie ログアウト時に呼ぶ。
func ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// RequireAuth 認証必須エンドポイント用ミドルウェア。
// 未認証なら 302 で /login へリダイレクト（HTML フロー）または 401（API フロー）を返す。
func RequireAuth(signKey []byte, lookup SessionLookup, loginPath string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sid, err := SessionIDFromCookie(r, signKey)
			if err != nil {
				redirectOrUnauthorized(w, r, loginPath)
				return
			}
			sess, ok := lookup(r.Context(), sid)
			if !ok || time.Now().After(sess.ExpiresAt) {
				ClearSessionCookie(w)
				redirectOrUnauthorized(w, r, loginPath)
				return
			}
			ctx := context.WithValue(r.Context(), ctxKeySession{}, sess)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// OptionalAuth 認証してあれば ctx に乗せるが、無くても通すミドルウェア（公開リンク等で使う）。
func OptionalAuth(signKey []byte, lookup SessionLookup) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sid, err := SessionIDFromCookie(r, signKey)
			if err != nil {
				next.ServeHTTP(w, r)
				return
			}
			sess, ok := lookup(r.Context(), sid)
			if !ok || time.Now().After(sess.ExpiresAt) {
				next.ServeHTTP(w, r)
				return
			}
			ctx := context.WithValue(r.Context(), ctxKeySession{}, sess)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// SessionIDForRAW RAW middleware から呼ばれる。Cookie だけから session UUID を引き出す。
func SessionIDForRAW(signKey []byte) func(r *http.Request) (uuid.UUID, bool) {
	return func(r *http.Request) (uuid.UUID, bool) {
		sid, err := SessionIDFromCookie(r, signKey)
		if err != nil {
			return uuid.Nil, false
		}
		return sid, true
	}
}

func redirectOrUnauthorized(w http.ResponseWriter, r *http.Request, loginPath string) {
	if wantsHTML(r) {
		http.Redirect(w, r, loginPath, http.StatusSeeOther)
		return
	}
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

func wantsHTML(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	return strings.Contains(accept, "text/html") || strings.Contains(accept, "*/*") || accept == ""
}
