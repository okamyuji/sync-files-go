package http

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/okamyuji/sync-files-go/internal/crypto"
	"github.com/okamyuji/sync-files-go/internal/http/middleware"
	"github.com/okamyuji/sync-files-go/internal/repo/mysql"
	"github.com/okamyuji/sync-files-go/internal/ui"
)

// uiFormMaxBytes 認証 / 設定系のフォーム上限（CSRF middleware と同じ 64 KiB）。
const uiFormMaxBytes int64 = 64 << 10

// loginPageHTMLHandler GET /login。HTML テンプレートを返す。
func loginPageHTMLHandler(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// すでにログイン済みならホームへ
		if hasActiveSession(d, r) {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		_ = d.UI.Render(w, r, http.StatusOK, "auth/login", &ui.PageData{Title: "サインイン"})
	}
}

// signupPageHTMLHandler GET /signup。
func signupPageHTMLHandler(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if hasActiveSession(d, r) {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		_ = d.UI.Render(w, r, http.StatusOK, "auth/signup", &ui.PageData{Title: "アカウント作成"})
	}
}

// loginPostFormHandler POST /login (application/x-www-form-urlencoded)。
//
// JSON 版 (/api/login) と振る舞いを揃えつつ、フォーム送信に対しては HTML を返すかリダイレクトする。
func loginPostFormHandler(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// CSRF middleware が ParseForm 済みのため r.PostForm が使える。念のため再呼び出ししても冪等。
		if r.PostForm == nil {
			r.Body = http.MaxBytesReader(w, r.Body, uiFormMaxBytes)
			if err := r.ParseForm(); err != nil {
				http.Error(w, "bad form", http.StatusBadRequest)
				return
			}
		}
		email := strings.ToLower(strings.TrimSpace(r.PostFormValue("email")))
		password := r.PostFormValue("password")
		totp := strings.TrimSpace(r.PostFormValue("totp"))

		ctx := r.Context()
		u, err := d.Users.FindByEmail(ctx, email)
		if errors.Is(err, mysql.ErrNotFound) {
			renderLoginError(d, w, r, email, "メールアドレスかパスワードが違います")
			return
		} else if err != nil {
			internalError(w, d, ctx, "find email", err)
			return
		}
		if u.LockedUntil != nil && time.Now().Before(*u.LockedUntil) {
			renderLoginError(d, w, r, email, "アカウントが一時的にロックされています。しばらく待ってから再試行してください。")
			return
		}
		ok, err := crypto.VerifyPassword(u.PasswordHash, password)
		if err != nil || !ok {
			handleLoginFailure(d, ctx, u.ID)
			renderLoginError(d, w, r, email, "メールアドレスかパスワードが違います")
			return
		}
		if u.TOTPEnabled {
			if totp == "" {
				renderLoginError(d, w, r, email, "認証コードを入力してください")
				return
			}
			secret, serr := unwrapTOTPSecret(u, d.Cfg.AESMasterKey)
			if serr != nil {
				internalError(w, d, ctx, "unwrap totp secret", serr)
				return
			}
			if !crypto.VerifyTOTP(secret, totp, time.Now()) {
				handleLoginFailure(d, ctx, u.ID)
				renderLoginError(d, w, r, email, "認証コードが正しくありません")
				return
			}
		}

		sess, err := issueSession(d, r, u.ID)
		if err != nil {
			internalError(w, d, ctx, "issue session", err)
			return
		}
		middleware.SetSessionCookie(w, sess.ID, d.Cfg.SessionKey, sess.ExpiresAt)
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}

// signupPostFormHandler POST /signup (application/x-www-form-urlencoded)。
func signupPostFormHandler(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.PostForm == nil {
			r.Body = http.MaxBytesReader(w, r.Body, uiFormMaxBytes)
			if err := r.ParseForm(); err != nil {
				http.Error(w, "bad form", http.StatusBadRequest)
				return
			}
		}
		email := strings.ToLower(strings.TrimSpace(r.PostFormValue("email")))
		password := r.PostFormValue("password")

		if email == "" || len(password) < 8 {
			renderSignupError(d, w, r, "メールアドレスと 8 文字以上のパスワードを入力してください")
			return
		}
		ctx := r.Context()
		if _, err := d.Users.FindByEmail(ctx, email); err == nil {
			renderSignupError(d, w, r, "このメールアドレスは既に登録されています")
			return
		} else if !errors.Is(err, mysql.ErrNotFound) {
			internalError(w, d, ctx, "find email", err)
			return
		}

		hash, err := crypto.HashPassword(password)
		if err != nil {
			internalError(w, d, ctx, "hash password", err)
			return
		}
		kek := make([]byte, 32)
		if _, err := rand.Read(kek); err != nil {
			internalError(w, d, ctx, "generate kek", err)
			return
		}
		kekEnc := wrapKeyDev(kek, d.Cfg.AESMasterKey)
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
			Email:             email,
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
		_ = d.Audit.Insert(ctx, nil, &mysql.AuditEntry{
			ActorID: &u.ID, ActorKind: mysql.ActorUser,
			Action: "auth.signup", TargetKind: "user", TargetID: &u.ID,
		})

		_ = d.UI.Render(w, r, http.StatusCreated, "auth/signup", &ui.PageData{
			Title: "アカウント作成",
			Extra: map[string]any{"RecoveryCodes": codes},
		})
	}
}

// homePageHandler 認証済みユーザのホーム画面。`/api/files` の listFilesHandler と同じ表示を HTML で返す。
func homePageHandler(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := middleware.SessionFrom(r.Context())
		if !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		files, err := d.Files.ListActiveByOwner(r.Context(), sess.UserID, 100, 0)
		if err != nil {
			internalError(w, d, r.Context(), "list files", err)
			return
		}
		u, err := d.Users.FindByID(r.Context(), sess.UserID)
		if err != nil {
			internalError(w, d, r.Context(), "find user", err)
			return
		}
		_ = d.UI.Render(w, r, http.StatusOK, "home", &ui.PageData{
			Title:       "ホーム",
			CurrentUser: &ui.CurrentUser{ID: u.ID.String(), Email: u.Email},
			Extra:       map[string]any{"Files": fileSummaries(files)},
		})
	}
}

// === ヘルパ ===

func renderLoginError(d *Deps, w http.ResponseWriter, r *http.Request, email, msg string) {
	_ = d.UI.Render(w, r, http.StatusUnauthorized, "auth/login", &ui.PageData{
		Title: "サインイン",
		Extra: map[string]any{"Email": email, "Error": msg},
	})
}

func renderSignupError(d *Deps, w http.ResponseWriter, r *http.Request, msg string) {
	_ = d.UI.Render(w, r, http.StatusBadRequest, "auth/signup", &ui.PageData{
		Title: "アカウント作成",
		Extra: map[string]any{"Error": msg},
	})
}

// issueSession 認証成功後のセッション発行。loginPostJSONHandler と振る舞いを揃える。
func issueSession(d *Deps, r *http.Request, userID uuid.UUID) (*mysql.Session, error) {
	now := time.Now().UTC()
	sess := &mysql.Session{
		ID:         uuid.New(),
		UserID:     userID,
		CreatedAt:  now,
		LastSeenAt: now,
		ExpiresAt:  now.Add(sessionDuration),
		IPAddr:     clientIPBytes(r),
		UserAgent:  r.UserAgent(),
	}
	if err := d.Sessions.Insert(r.Context(), sess); err != nil {
		return nil, err
	}
	if err := d.Users.UpdateLastLogin(r.Context(), userID, now); err != nil {
		d.Logger.WarnContext(r.Context(), "update last login", "err", err)
	}
	_ = d.Audit.Insert(r.Context(), nil, &mysql.AuditEntry{
		ActorID: &userID, ActorKind: mysql.ActorUser,
		Action: "auth.login", TargetKind: "user", TargetID: &userID,
		IPAddr: clientIPBytes(r), UserAgent: r.UserAgent(),
	})
	return sess, nil
}

// hasActiveSession auth middleware に頼らず、現在の Cookie から有効セッションがあるかを判定する。
// /login と /signup の GET で「すでにログイン済みなら / にリダイレクト」するために使う。
func hasActiveSession(d *Deps, r *http.Request) bool {
	sid, err := middleware.SessionIDFromCookie(r, d.Cfg.SessionKey)
	if err != nil {
		return false
	}
	if _, err := d.Sessions.FindActive(r.Context(), sid); err != nil {
		return false
	}
	return true
}
