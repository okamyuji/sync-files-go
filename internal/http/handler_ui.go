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

// requireUser auth-required な handler 共通の前処理。
// セッションが無いか、user 行が引けないなら適切な応答を出して (sess, nil, false) を返す。
func requireUser(d *Deps, w http.ResponseWriter, r *http.Request) (middleware.UserSession, *mysql.User, bool) {
	sess, ok := middleware.SessionFrom(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return sess, nil, false
	}
	u, err := d.Users.FindByID(r.Context(), sess.UserID)
	if err != nil {
		internalError(w, d, r.Context(), "find user", err)
		return sess, nil, false
	}
	return sess, u, true
}

// currentUserView ヘッダ表示用の最小ビュー。
func currentUserView(u *mysql.User) *ui.CurrentUser {
	return &ui.CurrentUser{ID: u.ID.String(), Email: u.Email}
}

// trashPageHandler GET /trash。ゴミ箱ファイル一覧を表示する。
func trashPageHandler(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, u, ok := requireUser(d, w, r)
		if !ok {
			return
		}
		files, err := d.Files.ListTrashedByOwner(r.Context(), sess.UserID, 100, 0)
		if err != nil {
			internalError(w, d, r.Context(), "list trashed", err)
			return
		}
		_ = d.UI.Render(w, r, http.StatusOK, "trash", &ui.PageData{
			Title:       "ゴミ箱",
			CurrentUser: currentUserView(u),
			Extra:       map[string]any{"Files": fileSummaries(files)},
		})
	}
}

// shareLinksPageHandler GET /share-links。発行済み公開リンクの管理画面。
func shareLinksPageHandler(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, u, ok := requireUser(d, w, r)
		if !ok {
			return
		}
		links, err := d.ShareLinks.ListActiveByOwner(r.Context(), sess.UserID, 100, 0)
		if err != nil {
			internalError(w, d, r.Context(), "list share links", err)
			return
		}
		rows := make([]map[string]any, 0, len(links))
		for _, l := range links {
			rows = append(rows, map[string]any{
				"id":             l.Link.ID.String(),
				"file_name":      l.FileName,
				"file_path":      l.FilePath,
				"file_id":        l.Link.FileID.String(),
				"expires_at":     l.Link.ExpiresAt.UTC().Format(time.RFC3339Nano),
				"view_count":     l.Link.ViewCount,
				"download_count": l.Link.DownloadCount,
				"has_password":   l.Link.PasswordHash != "",
			})
		}
		_ = d.UI.Render(w, r, http.StatusOK, "share_links", &ui.PageData{
			Title:       "共有リンク",
			CurrentUser: currentUserView(u),
			Extra:       map[string]any{"Links": rows},
		})
	}
}

// activityPageHandler GET /activity。監査ログから自分の操作履歴をタイムラインで見せる。
func activityPageHandler(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, u, ok := requireUser(d, w, r)
		if !ok {
			return
		}
		entries, err := d.Audit.ListByActor(r.Context(), sess.UserID, 200, 0)
		if err != nil {
			internalError(w, d, r.Context(), "list audit", err)
			return
		}
		rows := make([]map[string]any, 0, len(entries))
		for _, e := range entries {
			rows = append(rows, map[string]any{
				"action":      e.Action,
				"action_ja":   actionLabelJA(e.Action),
				"target_kind": e.TargetKind,
				"occurred_at": e.OccurredAt.UTC().Format(time.RFC3339Nano),
			})
		}
		_ = d.UI.Render(w, r, http.StatusOK, "activity", &ui.PageData{
			Title:       "アクティビティ",
			CurrentUser: currentUserView(u),
			Extra:       map[string]any{"Entries": rows},
		})
	}
}

// settingsPageHandler GET /settings。最低限の表示（v1: メールアドレスとログアウトリンク）。
func settingsPageHandler(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, u, ok := requireUser(d, w, r)
		if !ok {
			return
		}
		_ = d.UI.Render(w, r, http.StatusOK, "settings", &ui.PageData{
			Title:       "設定",
			CurrentUser: currentUserView(u),
			Extra:       map[string]any{"Email": u.Email, "TOTPEnabled": u.TOTPEnabled},
		})
	}
}

// sharePageHandler GET /share/{token}。公開リンクの未認証ランディングページ。
//
// 既存の publicShareDownloadHandler は Accept: */* やヘッダ無しのときに直接ファイルを返す。
// ここではブラウザ来訪時 (Accept: text/html) に「ファイル名 + サイズ + ダウンロードボタン」のページを表示する。
func sharePageHandler(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.PathValue("token")
		if token == "" {
			http.NotFound(w, r)
			return
		}
		// Accept ヘッダで HTML 要求でなければ既存の直接ダウンロードに委譲（curl 等の互換）。
		if !strings.Contains(r.Header.Get("Accept"), "text/html") {
			publicShareDownloadHandler(d).ServeHTTP(w, r)
			return
		}
		ctx := r.Context()
		s, err := resolveShareLink(ctx, d, token)
		if err != nil {
			emitShareError(w, d, ctx, err)
			return
		}
		f, _, _, err := loadShareTarget(ctx, d, s)
		if err != nil {
			emitShareError(w, d, ctx, err)
			return
		}
		_ = d.ShareLinks.IncrementViewCount(ctx, s.ID)
		_ = d.UI.Render(w, r, http.StatusOK, "share", &ui.PageData{
			Title: f.Name,
			Extra: map[string]any{
				"FileName":      f.Name,
				"SizeBytes":     f.SizeBytes,
				"ContentType":   contentTypeOrDefault(f.ContentType),
				"DownloadURL":   "/share/" + token,
				"HasPassword":   s.PasswordHash != "",
				"ExpiresAt":     s.ExpiresAt.UTC().Format(time.RFC3339Nano),
				"DownloadCount": s.DownloadCount,
			},
		})
	}
}

// actionLabelJA audit_logs.action を日本語ラベルに写像。テンプレ側で見やすくするためのヘルパ。
func actionLabelJA(action string) string {
	switch action {
	case "auth.signup":
		return "アカウント作成"
	case "auth.login":
		return "ログイン"
	case "auth.login_failure":
		return "ログイン失敗"
	case "auth.logout":
		return "ログアウト"
	case "file.upload":
		return "ファイルをアップロード"
	case "file.update":
		return "ファイルを更新"
	case "file.delete":
		return "ファイルを削除"
	case "file.restore":
		return "ファイルを復元"
	case "share.create":
		return "公開リンクを発行"
	case "share.revoke":
		return "公開リンクを取り消し"
	case "share.download":
		return "公開リンクからダウンロード"
	}
	return action
}

// homePageHandler 認証済みユーザのホーム画面。`/api/files` の listFilesHandler と同じ表示を HTML で返す。
func homePageHandler(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, u, ok := requireUser(d, w, r)
		if !ok {
			return
		}
		files, err := d.Files.ListActiveByOwner(r.Context(), sess.UserID, 100, 0)
		if err != nil {
			internalError(w, d, r.Context(), "list files", err)
			return
		}
		_ = d.UI.Render(w, r, http.StatusOK, "home", &ui.PageData{
			Title:       "ホーム",
			CurrentUser: currentUserView(u),
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
