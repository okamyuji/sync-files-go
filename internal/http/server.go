// Package http は HTTP サーバの組み立てを行う。
//
// 設計書 02-architecture.md §3.4 / 04-sync-semantics.md / 05-file-operations-logic-tree.md に対応。
// Phase 5 で UI レンダリング層 (internal/ui) を結線し、HTML フローと JSON フローを分離している。
package http

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/okamyuji/sync-files-go/internal/config"
	"github.com/okamyuji/sync-files-go/internal/http/middleware"
	"github.com/okamyuji/sync-files-go/internal/repo"
	"github.com/okamyuji/sync-files-go/internal/repo/mysql"
	"github.com/okamyuji/sync-files-go/internal/storage"
	"github.com/okamyuji/sync-files-go/internal/ui"
)

// Deps サーバ全体の依存性。main からまとめて注入する。
type Deps struct {
	Cfg     *config.Config
	Logger  *slog.Logger
	Router  *repo.DBRouter
	Storage storage.Storage
	UI      *ui.Renderer

	Users        *mysql.UsersRepo
	Sessions     *mysql.SessionsRepo
	Files        *mysql.FilesRepo
	FileVersions *mysql.FileVersionsRepo
	ShareLinks   *mysql.ShareLinksRepo
	Audit        *mysql.AuditRepo
}

// NewServer ハンドラと middleware を結線して http.Handler を返す。
func NewServer(d *Deps) http.Handler {
	mux := http.NewServeMux()

	// ヘルスチェック
	mux.HandleFunc("GET /healthz", healthzHandler())
	mux.HandleFunc("GET /readyz", readyzHandler(d))

	// 静的アセット (embed.FS、Cache-Control immutable + 1y)
	if d.UI != nil {
		mux.Handle("GET /static/", staticAssetsHandler(ui.StaticFS()))
	}

	// 認証フロー
	if d.UI != nil {
		mux.HandleFunc("GET /login", loginPageHTMLHandler(d))
		mux.HandleFunc("GET /signup", signupPageHTMLHandler(d))
		mux.HandleFunc("POST /login", loginPostFormHandler(d))
		mux.HandleFunc("POST /signup", signupPostFormHandler(d))
	}
	// JSON 互換エンドポイント（API クライアント / 統合テスト用）
	mux.HandleFunc("POST /api/signup", signupHandler(d))
	mux.HandleFunc("POST /api/login", loginPostJSONHandler(d))
	mux.HandleFunc("POST /logout", logoutHandler(d))

	// 認証必須エンドポイント
	authMW := middleware.RequireAuth(d.Cfg.SessionKey, sessionLookup(d), "/login")
	if d.UI != nil {
		mux.Handle("GET /", authMW(homePageHandler(d)))
	} else {
		mux.Handle("GET /", authMW(filesIndexFallback()))
	}
	mux.Handle("GET /api/files", authMW(listFilesHandler(d)))
	mux.Handle("POST /api/files", authMW(uploadFileHandler(d)))
	mux.Handle("GET /api/files/{id}", authMW(downloadFileHandler(d)))
	mux.Handle("DELETE /api/files/{id}", authMW(deleteFileHandler(d)))
	mux.Handle("POST /api/files/{id}/restore", authMW(restoreFileHandler(d)))
	mux.Handle("POST /api/files/{id}/save-as-copy", authMW(saveAsCopyHandler(d)))

	// 公開リンク（認証必須側）
	mux.Handle("POST /api/files/{id}/share-links", authMW(createShareLinkHandler(d)))
	mux.Handle("DELETE /api/share-links/{id}", authMW(revokeShareLinkHandler(d)))

	// 公開リンク（未認証側）。ShareLinksRepo が Primary を読むので RAW window 不要。
	mux.HandleFunc("GET /share/{token}", publicShareDownloadHandler(d))

	// 共通ミドルウェア (chain)
	chain := middleware.Chain(mux,
		middleware.RequestID(),
		middleware.Logging(d.Logger),
		middleware.Recovery(d.Logger),
		middleware.SecurityHeaders(),
		middleware.CSPNonce(),
		middleware.CSRF(d.Cfg.CSRFKey),
		middleware.RAWMiddleware(d.Cfg.SessionKey, middleware.SessionIDForRAW(d.Cfg.SessionKey)),
	)
	return chain
}

// staticAssetsHandler  embed.FS を /static/ で配信する。
//
// `Cache-Control: public, max-age=31536000, immutable` を強く付ける（バージョニング URL 前提）。
// 8 §9 に従い、フォントや CSS の長期キャッシュを許す。
func staticAssetsHandler(fsys fs.FS) http.Handler {
	fileServer := http.FileServerFS(fsys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /static/foo → /foo
		r2 := r.Clone(r.Context())
		r2.URL.Path = strings.TrimPrefix(r.URL.Path, "/static")
		if r2.URL.Path == "" {
			r2.URL.Path = "/"
		}
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		fileServer.ServeHTTP(w, r2)
	})
}

// sessionLookup auth middleware に渡す「sessionID から有効 session を引く」関数。
func sessionLookup(d *Deps) middleware.SessionLookup {
	return func(ctx context.Context, sessionID uuid.UUID) (middleware.UserSession, bool) {
		s, err := d.Sessions.FindActive(ctx, sessionID)
		if err != nil {
			return middleware.UserSession{}, false
		}
		return middleware.UserSession{
			SessionID: s.ID,
			UserID:    s.UserID,
			ExpiresAt: s.ExpiresAt,
		}, true
	}
}

func healthzHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	}
}

// readyzHandler DB と Storage の到達確認を行う。
func readyzHandler(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()

		if err := d.Router.Writer(ctx).PingContext(ctx); err != nil {
			d.Logger.WarnContext(ctx, "readyz: primary ping failed", "err", err)
			http.Error(w, "primary db not ready", http.StatusServiceUnavailable)
			return
		}
		if err := d.Router.Reader(ctx).PingContext(ctx); err != nil {
			d.Logger.WarnContext(ctx, "readyz: reader ping failed", "err", err)
			http.Error(w, "reader db not ready", http.StatusServiceUnavailable)
			return
		}
		if _, err := d.Storage.VersionExists(ctx, "_system", "_health", "probe"); err != nil {
			d.Logger.WarnContext(ctx, "readyz: storage probe failed", "err", err)
			http.Error(w, "storage not ready", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ready\n"))
	}
}

// logoutHandler 認証済みユーザのセッションを失効させる。
func logoutHandler(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sid, err := middleware.SessionIDFromCookie(r, d.Cfg.SessionKey)
		if err == nil {
			if delErr := d.Sessions.Delete(r.Context(), sid); delErr != nil && !errors.Is(delErr, mysql.ErrNotFound) {
				d.Logger.WarnContext(r.Context(), "logout: session delete failed", "err", delErr)
			}
		}
		middleware.ClearSessionCookie(w)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	}
}

// filesIndexFallback UI Renderer が無いときの素朴な応答（統合テスト用）。
func filesIndexFallback() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, _ := middleware.SessionFrom(r.Context())
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<!doctype html><html><body><h1>files</h1><p>user: " + sess.UserID.String() + "</p></body></html>"))
	}
}

// OpenDBs DSN から Primary/Replica の *sql.DB を作って共通設定を当てる。
func OpenDBs(cfg *config.Config) (primary, replica *sql.DB, err error) {
	primary, err = openDB(cfg.MySQLDSN(cfg.DB.PrimaryHost))
	if err != nil {
		return nil, nil, err
	}
	replica, err = openDB(cfg.MySQLDSN(cfg.DB.ReplicaHost))
	if err != nil {
		_ = primary.Close()
		return nil, nil, err
	}
	return primary, replica, nil
}

func openDB(dsn string) (*sql.DB, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(50)
	db.SetMaxIdleConns(25)
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)
	return db, nil
}
