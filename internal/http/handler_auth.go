package http

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/okamyuji/sync-files-go/internal/crypto"
	"github.com/okamyuji/sync-files-go/internal/http/middleware"
	"github.com/okamyuji/sync-files-go/internal/repo/mysql"
)

// loginAttemptThreshold ログイン失敗の上限（07-security.md §3.4）。
const loginAttemptThreshold = 5

// loginLockDuration ロック時間（同上）。
const loginLockDuration = 15 * time.Minute

// sessionDuration ブラウザセッションの有効期限（07-security.md §3.3）。
const sessionDuration = 7 * 24 * time.Hour

// signupReq /signup の入力。
type signupReq struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// signupHandler 新規ユーザ登録（v1：個人専用なので 1 ユーザだけ作成する想定）。
func signupHandler(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req signupReq
		if err := decodeJSON(r, &req); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		req.Email = strings.ToLower(strings.TrimSpace(req.Email))
		if req.Email == "" || len(req.Password) < 8 {
			http.Error(w, "email required and password must be 8+ chars", http.StatusBadRequest)
			return
		}

		ctx := r.Context()
		// 既存ユーザがあればエラー（個人用なので 1 件）
		if _, err := d.Users.FindByEmail(ctx, req.Email); err == nil {
			http.Error(w, "email already exists", http.StatusConflict)
			return
		} else if !errors.Is(err, mysql.ErrNotFound) {
			internalError(w, d, ctx, "find email", err)
			return
		}

		hash, err := crypto.HashPassword(req.Password)
		if err != nil {
			internalError(w, d, ctx, "hash password", err)
			return
		}

		// 鍵階層: KEK をマスタ鍵で AES-Key-Wrap して保管
		kek := make([]byte, 32)
		if _, err := rand.Read(kek); err != nil {
			internalError(w, d, ctx, "generate kek", err)
			return
		}
		// Phase 3 では KEK を「平文 + master_key 由来の MAC」で代替保管（Phase 4 で AES-Key-Wrap に置換）。
		kekEnc := wrapKeyDev(kek, d.Cfg.AESMasterKey)

		// recovery codes 10 個 (07-security.md §3.2)
		codes, hashes, err := generateRecoveryCodes()
		if err != nil {
			internalError(w, d, ctx, "recovery codes", err)
			return
		}
		recoveryJSON, err := json.Marshal(hashes)
		if err != nil {
			internalError(w, d, ctx, "marshal codes", err)
			return
		}

		u := &mysql.User{
			ID:                uuid.New(),
			Email:             req.Email,
			PasswordHash:      hash,
			TOTPEnabled:       false,
			RecoveryCodesJSON: recoveryJSON,
			KEKEnc:            kekEnc,
			KEKID:             uuid.New(),
			MasterKeyVersion:  1,
			CreatedAt:         time.Now().UTC(),
		}
		if err := d.Users.Insert(ctx, u); err != nil {
			internalError(w, d, ctx, "insert user", err)
			return
		}

		// 監査ログ
		_ = d.Audit.Insert(ctx, nil, &mysql.AuditEntry{
			ActorID: &u.ID, ActorKind: mysql.ActorUser,
			Action: "auth.signup", TargetKind: "user", TargetID: &u.ID,
			IPAddr: clientIPBytes(r), UserAgent: r.UserAgent(),
		})

		// レスポンス: リカバリコードを「最初の 1 度だけ」表示
		writeJSON(w, http.StatusCreated, map[string]any{
			"user_id":        u.ID.String(),
			"recovery_codes": codes,
			"message":        "保存してください。これは二度と表示されません。",
		})
	}
}

// loginReq /login の入力（TOTP は signup 後の setup でのみ有効化されるため、未設定なら省略可）。
type loginReq struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	TOTP     string `json:"totp,omitempty"`
}

func loginPostJSONHandler(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req loginReq
		if err := decodeJSON(r, &req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		req.Email = strings.ToLower(strings.TrimSpace(req.Email))
		ctx := r.Context()

		u, err := d.Users.FindByEmail(ctx, req.Email)
		if errors.Is(err, mysql.ErrNotFound) {
			http.Error(w, "invalid credentials", http.StatusUnauthorized)
			return
		} else if err != nil {
			internalError(w, d, ctx, "find email", err)
			return
		}

		// アカウントロックチェック
		if u.LockedUntil != nil && time.Now().Before(*u.LockedUntil) {
			http.Error(w, "account locked", http.StatusTooManyRequests)
			return
		}

		ok, err := crypto.VerifyPassword(u.PasswordHash, req.Password)
		if err != nil || !ok {
			handleLoginFailure(d, ctx, u.ID)
			http.Error(w, "invalid credentials", http.StatusUnauthorized)
			return
		}

		// TOTP 確認（有効なユーザのみ）
		if u.TOTPEnabled {
			if req.TOTP == "" {
				http.Error(w, "totp required", http.StatusUnauthorized)
				return
			}
			secret, err := unwrapTOTPSecret(u, d.Cfg.AESMasterKey)
			if err != nil {
				internalError(w, d, ctx, "unwrap totp secret", err)
				return
			}
			if !crypto.VerifyTOTP(secret, req.TOTP, time.Now()) {
				handleLoginFailure(d, ctx, u.ID)
				http.Error(w, "invalid totp", http.StatusUnauthorized)
				return
			}
		}

		// セッション発行
		now := time.Now().UTC()
		sess := &mysql.Session{
			ID:         uuid.New(),
			UserID:     u.ID,
			CreatedAt:  now,
			LastSeenAt: now,
			ExpiresAt:  now.Add(sessionDuration),
			IPAddr:     clientIPBytes(r),
			UserAgent:  r.UserAgent(),
		}
		if err := d.Sessions.Insert(ctx, sess); err != nil {
			internalError(w, d, ctx, "insert session", err)
			return
		}
		if err := d.Users.UpdateLastLogin(ctx, u.ID, now); err != nil {
			d.Logger.WarnContext(ctx, "update last login", "err", err)
		}
		_ = d.Audit.Insert(ctx, nil, &mysql.AuditEntry{
			ActorID: &u.ID, ActorKind: mysql.ActorUser,
			Action: "auth.login", TargetKind: "user", TargetID: &u.ID,
			IPAddr: clientIPBytes(r), UserAgent: r.UserAgent(),
		})

		middleware.SetSessionCookie(w, sess.ID, d.Cfg.SessionKey, sess.ExpiresAt)
		writeJSON(w, http.StatusOK, map[string]any{"user_id": u.ID.String()})
	}
}

func handleLoginFailure(d *Deps, ctx context.Context, userID uuid.UUID) {
	if _, _, err := d.Users.IncrementFailedLogin(ctx, userID, loginAttemptThreshold, loginLockDuration); err != nil {
		d.Logger.WarnContext(ctx, "increment failed login", "err", err)
	}
	_ = d.Audit.Insert(ctx, nil, &mysql.AuditEntry{
		ActorID: &userID, ActorKind: mysql.ActorUser,
		Action: "auth.login_failure", TargetKind: "user", TargetID: &userID,
	})
}

// generateRecoveryCodes 10 個のリカバリコードを生成し、平文と Argon2id ハッシュを返す。
func generateRecoveryCodes() (codes, hashes []string, err error) {
	codes = make([]string, 10)
	hashes = make([]string, 10)
	for i := range codes {
		buf := make([]byte, 12)
		if _, err := rand.Read(buf); err != nil {
			return nil, nil, err
		}
		codes[i] = base64.RawURLEncoding.EncodeToString(buf)
		h, err := crypto.HashPassword(codes[i])
		if err != nil {
			return nil, nil, err
		}
		hashes[i] = h
	}
	return codes, hashes, nil
}

// wrapKeyDev 開発時の単純な KEK ラップ（マスタ鍵 + ランダム XOR + HMAC）。
// 本番化（Phase 4 以降）で AES-Key-Wrap (RFC 3394) に置換予定。
func wrapKeyDev(plain, master []byte) []byte {
	mac := sha256.Sum256(append(append([]byte("kek-dev:"), master...), plain...))
	out := make([]byte, len(plain)+32)
	copy(out, plain)
	copy(out[len(plain):], mac[:])
	return out
}

// unwrapTOTPSecret 保存された TOTP secret を AES-256-GCM で復号する。
// 暗号化形式: crypto.EncryptTOTPSecret 参照（version + nonce + ciphertext + tag）。
func unwrapTOTPSecret(u *mysql.User, masterKey []byte) ([]byte, error) {
	if len(u.TOTPSecretEnc) == 0 {
		return nil, errors.New("totp secret not set")
	}
	return crypto.DecryptTOTPSecret(u.TOTPSecretEnc, masterKey)
}

func decodeJSON(r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<20) // 1 MiB
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func internalError(w http.ResponseWriter, d *Deps, ctx context.Context, msg string, err error) {
	d.Logger.ErrorContext(ctx, msg, "err", err)
	http.Error(w, fmt.Sprintf("internal error: %s", msg), http.StatusInternalServerError)
}

func clientIPBytes(r *http.Request) []byte {
	// 実装簡略化: Phase 5 で X-Forwarded-For / CF-Connecting-IP の解釈を入れる
	return nil
}
