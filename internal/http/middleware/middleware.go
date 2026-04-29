// Package middleware は HTTP の横断的関心事を実装する。
//
// 設計書 02-architecture.md §3.4 / 07-security.md / ADR-008 に対応。
package middleware

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/okamyuji/sync-files-go/internal/repo"
)

// Chain は複数のミドルウェアを順に適用する。
func Chain(h http.Handler, mws ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// === Recovery ===

func Recovery(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if v := recover(); v != nil {
					logger.Error("panic recovered",
						"panic", v,
						"request_id", RequestIDFrom(r.Context()),
						"path", r.URL.Path, "method", r.Method)
					http.Error(w, "internal server error", http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// === RequestID ===

type ctxKeyRequestID struct{}

func RequestID() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get("X-Request-ID")
			if id == "" {
				id = uuid.NewString()
			}
			w.Header().Set("X-Request-ID", id)
			ctx := context.WithValue(r.Context(), ctxKeyRequestID{}, id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func RequestIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyRequestID{}).(string)
	return v
}

// === Logging ===

func Logging(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			started := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: 200}
			next.ServeHTTP(rec, r)
			logger.Info("http",
				"request_id", RequestIDFrom(r.Context()),
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"duration_ms", time.Since(started).Milliseconds(),
				"remote", r.RemoteAddr,
			)
		})
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(s int) {
	r.status = s
	r.ResponseWriter.WriteHeader(s)
}

// === SecurityHeaders ===

func SecurityHeaders() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains; preload")
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
			h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
			h.Set("Cross-Origin-Resource-Policy", "same-origin")
			h.Set("Cross-Origin-Opener-Policy", "same-origin")
			next.ServeHTTP(w, r)
		})
	}
}

// === CSP nonce ===

type ctxKeyCSPNonce struct{}

func CSPNonce() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b := make([]byte, 16)
			_, _ = rand.Read(b)
			n := base64.RawStdEncoding.EncodeToString(b)
			csp := strings.Join([]string{
				"default-src 'self'",
				"script-src 'self' 'nonce-" + n + "'",
				"style-src 'self' 'unsafe-inline'",
				"img-src 'self' data:",
				"font-src 'self'",
				"connect-src 'self'",
				"frame-src 'none'",
				"object-src 'none'",
				"base-uri 'self'",
				"form-action 'self'",
				"upgrade-insecure-requests",
			}, "; ")
			w.Header().Set("Content-Security-Policy", csp)
			ctx := context.WithValue(r.Context(), ctxKeyCSPNonce{}, n)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func CSPNonceFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyCSPNonce{}).(string)
	return v
}

// === RAW (Read-After-Write) Cookie ===

const RAWCookieName = "__Host-sync_raw_until"

// SetRAWCookie は書き込み完了直後のレスポンスで呼ぶ。
func SetRAWCookie(w http.ResponseWriter, sessionID uuid.UUID, until time.Time, signKey []byte) {
	payload := strconv.FormatInt(until.Unix(), 10)
	mac := hmacSHA256(signKey, sessionID[:], []byte(payload))
	value := payload + "." + base64.RawURLEncoding.EncodeToString(mac)
	http.SetCookie(w, &http.Cookie{
		Name:     RAWCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		Expires:  until.Add(time.Second),
	})
}

// RAWMiddleware は受信 Cookie を検証して ctx に Read-After-Write until を埋める。
func RAWMiddleware(signKey []byte, getSessionID func(r *http.Request) (uuid.UUID, bool)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := r.Cookie(RAWCookieName)
			if err != nil {
				next.ServeHTTP(w, r)
				return
			}
			sid, ok := getSessionID(r)
			if !ok {
				next.ServeHTTP(w, r)
				return
			}
			until, err := verifyRAWCookie(c.Value, sid, signKey)
			if err != nil || time.Now().After(until) {
				next.ServeHTTP(w, r)
				return
			}
			ctx := repo.WithReadAfterWrite(r.Context(), until)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func verifyRAWCookie(value string, sid uuid.UUID, signKey []byte) (time.Time, error) {
	parts := strings.SplitN(value, ".", 2)
	if len(parts) != 2 {
		return time.Time{}, errors.New("malformed cookie")
	}
	until, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return time.Time{}, errors.New("malformed timestamp")
	}
	got, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, err
	}
	expected := hmacSHA256(signKey, sid[:], []byte(parts[0]))
	if !hmac.Equal(got, expected) {
		return time.Time{}, errors.New("hmac mismatch")
	}
	return time.Unix(until, 0), nil
}

func hmacSHA256(key []byte, parts ...[]byte) []byte {
	h := hmac.New(sha256.New, key)
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}
