//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/okamyuji/sync-files-go/internal/config"
	hsrv "github.com/okamyuji/sync-files-go/internal/http"
	"github.com/okamyuji/sync-files-go/internal/http/middleware"
	"github.com/okamyuji/sync-files-go/internal/storage/localfs"
)

// makeServer 統合テスト用に handler 結線済みの http.Handler と Cookie ストアを返す。
func makeServer(t *testing.T, env *Env) (http.Handler, *config.Config) {
	t.Helper()

	masterKey := make([]byte, 32)
	_, _ = rand.Read(masterKey)
	sessionKey := make([]byte, 32)
	_, _ = rand.Read(sessionKey)
	csrfKey := make([]byte, 32)
	_, _ = rand.Read(csrfKey)

	cfg := &config.Config{
		AppEnv:         "test",
		Port:           0,
		DataDir:        env.DataDir,
		BaseURL:        "http://test.local",
		LogLevel:       "warn",
		MaxUploadBytes: 16 * 1024 * 1024,
		AESMasterKey:   masterKey,
		TOTPHMACKey:    masterKey,
		CSRFKey:        csrfKey,
		SessionKey:     sessionKey,
	}
	cfg.RAWWindow = 5e9 // 5 秒
	cfg.ReplicaLagDegrade = 10e9

	store, err := localfs.New(env.DataDir)
	if err != nil {
		t.Fatalf("localfs: %v", err)
	}

	deps := &hsrv.Deps{
		Cfg:          cfg,
		Logger:       slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		Router:       env.Router,
		Storage:      store,
		Users:        env.Users,
		Sessions:     env.Sessions,
		Files:        env.Files,
		FileVersions: env.FileVersions,
		ShareLinks:   env.ShareLinks,
		Audit:        env.Audit,
	}
	return hsrv.NewServer(deps), cfg
}

// signedSessionCookie 認証済みリクエストを作るためのヘルパー。
func signedSessionCookie(sessionID uuid.UUID, signKey []byte) *http.Cookie {
	mac := hmacSHA256Bytes(signKey, sessionID[:])
	return &http.Cookie{
		Name:  middleware.SessionCookieName,
		Value: sessionID.String() + "." + base64.RawURLEncoding.EncodeToString(mac),
		Path:  "/",
	}
}

// makeAuthenticatedSession テスト用に user + session を DB に登録して、
// セッション cookie + CSRF 用ヘッダ値を返す。
func makeAuthenticatedSession(t *testing.T, env *Env, cfg *config.Config) (sessCookie *http.Cookie, csrfToken string, userID uuid.UUID) {
	t.Helper()
	u := MakeUser(t, env)
	now := nowUTC()
	s := newSession(u.ID, now)
	if err := env.Sessions.Insert(context.Background(), s); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	csrfToken = "test-csrf-" + uuid.NewString()
	return signedSessionCookie(s.ID, cfg.SessionKey), csrfToken, u.ID
}

// uploadRequest 認証済みリクエストを組み立てる。
func uploadRequest(t *testing.T, body io.Reader, path, ifMatch, ifNoneMatch string, sessCookie *http.Cookie, csrfToken string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/files", body)
	r.Header.Set("Content-Type", "application/octet-stream")
	r.Header.Set("X-File-Path", path)
	if ifMatch != "" {
		r.Header.Set("If-Match", ifMatch)
	}
	if ifNoneMatch != "" {
		r.Header.Set("If-None-Match", ifNoneMatch)
	}
	r.Header.Set("X-CSRF-Token", csrfToken)
	r.AddCookie(sessCookie)
	r.AddCookie(&http.Cookie{Name: middleware.CSRFCookieName, Value: csrfToken, Path: "/"})
	return r
}

// TestUpload_NewFile 新規アップロード（If-None-Match: *）が 201 を返し、
// versions/{file_id}/{version_id} の immutable key が作られることを確認。
func TestUpload_NewFile(t *testing.T) {
	env := SetupEnv(t)
	srv, cfg := makeServer(t, env)
	sessCookie, csrf, ownerID := makeAuthenticatedSession(t, env, cfg)

	body := bytes.NewBufferString("hello world")
	r := uploadRequest(t, body, "/hello.txt", "", "*", sessCookie, csrf)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)

	if w.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d. body=%s", w.Code, w.Body.String())
	}
	versionID := w.Header().Get("ETag")
	fileID := w.Header().Get("X-File-Id")
	if versionID == "" || fileID == "" {
		t.Fatalf("missing ETag or X-File-Id headers")
	}

	// versions/{owner}/{file_id}/{version_id} ファイルが存在
	versionPath := filepath.Join(env.DataDir, "owner-"+ownerID.String(), "versions", fileID, versionID)
	if _, err := os.Stat(versionPath); err != nil {
		t.Fatalf("expected version file at %s: %v", versionPath, err)
	}

	// RAW Cookie 発行確認（HIGH-1）
	rawCookieFound := false
	for _, c := range w.Result().Cookies() {
		if c.Name == middleware.RAWCookieName {
			rawCookieFound = true
		}
	}
	if !rawCookieFound {
		t.Fatal("RAW cookie should be set on upload completion")
	}
}

// TestUpload_DuplicateRejected 同名 active が既に存在するとき、If-None-Match: * は 412 を返す。
func TestUpload_DuplicateRejected(t *testing.T) {
	env := SetupEnv(t)
	srv, cfg := makeServer(t, env)
	sessCookie, csrf, _ := makeAuthenticatedSession(t, env, cfg)

	// 1 回目
	r1 := uploadRequest(t, bytes.NewBufferString("v1"), "/dup.txt", "", "*", sessCookie, csrf)
	w1 := httptest.NewRecorder()
	srv.ServeHTTP(w1, r1)
	if w1.Code != http.StatusCreated {
		t.Fatalf("first upload want 201, got %d", w1.Code)
	}

	// 2 回目 - 同じパス、If-None-Match: * → 412
	r2 := uploadRequest(t, bytes.NewBufferString("v2"), "/dup.txt", "", "*", sessCookie, csrf)
	w2 := httptest.NewRecorder()
	srv.ServeHTTP(w2, r2)
	if w2.Code != http.StatusPreconditionFailed {
		t.Fatalf("second upload should fail with 412, got %d", w2.Code)
	}
}

// TestUpload_Overwrite_OCC_Match If-Match: <current_version_id> が一致したとき 204、
// 一致しないとき 409 を返す。
func TestUpload_Overwrite_OCC(t *testing.T) {
	env := SetupEnv(t)
	srv, cfg := makeServer(t, env)
	sessCookie, csrf, _ := makeAuthenticatedSession(t, env, cfg)

	// 新規作成
	r1 := uploadRequest(t, bytes.NewBufferString("v1"), "/occ.txt", "", "*", sessCookie, csrf)
	w1 := httptest.NewRecorder()
	srv.ServeHTTP(w1, r1)
	versionID := w1.Header().Get("ETag")
	if versionID == "" {
		t.Fatalf("first upload failed: %d %s", w1.Code, w1.Body.String())
	}

	// OCC 一致 → 上書き成功
	r2 := uploadRequest(t, bytes.NewBufferString("v2 newer"), "/occ.txt", versionID, "", sessCookie, csrf)
	w2 := httptest.NewRecorder()
	srv.ServeHTTP(w2, r2)
	if w2.Code != http.StatusNoContent {
		t.Fatalf("OCC match overwrite want 204, got %d. body=%s", w2.Code, w2.Body.String())
	}
	newVer := w2.Header().Get("ETag")
	if newVer == "" || newVer == versionID {
		t.Fatalf("ETag should be a new version, got %q (was %q)", newVer, versionID)
	}

	// OCC 不一致 → 409 Conflict
	r3 := uploadRequest(t, bytes.NewBufferString("v3 stale"), "/occ.txt", versionID, "", sessCookie, csrf)
	w3 := httptest.NewRecorder()
	srv.ServeHTTP(w3, r3)
	if w3.Code != http.StatusConflict {
		t.Fatalf("stale OCC want 409, got %d", w3.Code)
	}
	var conflict map[string]any
	if err := json.NewDecoder(w3.Body).Decode(&conflict); err != nil {
		t.Fatalf("conflict body decode: %v", err)
	}
	if conflict["kind"] != "version_mismatch" {
		t.Fatalf("expected kind=version_mismatch, got %v", conflict["kind"])
	}
	if got := w3.Header().Get("HX-Trigger"); got != "openConflictModal" {
		t.Fatalf("HX-Trigger want openConflictModal, got %q", got)
	}
}

// TestUpload_NeedsPrecondition Precondition ヘッダなしは 428 で拒否。
func TestUpload_NeedsPrecondition(t *testing.T) {
	env := SetupEnv(t)
	srv, cfg := makeServer(t, env)
	sessCookie, csrf, _ := makeAuthenticatedSession(t, env, cfg)

	// 既存ファイルを 1 つ作って、Precondition ヘッダなしで上書き試行
	r0 := uploadRequest(t, bytes.NewBufferString("v1"), "/needs.txt", "", "*", sessCookie, csrf)
	w0 := httptest.NewRecorder()
	srv.ServeHTTP(w0, r0)

	r := uploadRequest(t, bytes.NewBufferString("v2"), "/needs.txt", "", "", sessCookie, csrf)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	if w.Code != http.StatusPreconditionRequired {
		t.Fatalf("no precondition should yield 428, got %d", w.Code)
	}
}

// TestUpload_DownloadRoundTrip アップロード → ダウンロードでバイト列が一致する。
func TestUpload_DownloadRoundTrip(t *testing.T) {
	env := SetupEnv(t)
	srv, cfg := makeServer(t, env)
	sessCookie, csrf, _ := makeAuthenticatedSession(t, env, cfg)

	plain := []byte("the quick brown fox jumps over the lazy dog")
	r1 := uploadRequest(t, bytes.NewReader(plain), "/payload.bin", "", "*", sessCookie, csrf)
	w1 := httptest.NewRecorder()
	srv.ServeHTTP(w1, r1)
	if w1.Code != http.StatusCreated {
		t.Fatalf("upload want 201, got %d. body=%s", w1.Code, w1.Body.String())
	}
	fileID := w1.Header().Get("X-File-Id")

	// GET /api/files/{id}
	r2 := httptest.NewRequest(http.MethodGet, "/api/files/"+fileID, nil)
	r2.AddCookie(sessCookie)
	w2 := httptest.NewRecorder()
	srv.ServeHTTP(w2, r2)
	if w2.Code != http.StatusOK {
		t.Fatalf("download want 200, got %d. body=%s", w2.Code, w2.Body.String())
	}
	if !bytes.Equal(w2.Body.Bytes(), plain) {
		t.Fatalf("payload mismatch.\n want: %s\n got:  %s", plain, w2.Body.String())
	}
}

// TestUpload_DeleteSoftPreservesVersion ソフト削除後も versions/ のオブジェクトは残る (INV-1)。
func TestUpload_DeleteSoftPreservesVersion(t *testing.T) {
	env := SetupEnv(t)
	srv, cfg := makeServer(t, env)
	sessCookie, csrf, ownerID := makeAuthenticatedSession(t, env, cfg)

	r1 := uploadRequest(t, bytes.NewBufferString("preserved"), "/keep.txt", "", "*", sessCookie, csrf)
	w1 := httptest.NewRecorder()
	srv.ServeHTTP(w1, r1)
	versionID := w1.Header().Get("ETag")
	fileID := w1.Header().Get("X-File-Id")
	versionPath := filepath.Join(env.DataDir, "owner-"+ownerID.String(), "versions", fileID, versionID)

	// DELETE /api/files/{id}
	r2 := httptest.NewRequest(http.MethodDelete, "/api/files/"+fileID, nil)
	r2.Header.Set("X-CSRF-Token", csrf)
	r2.AddCookie(sessCookie)
	r2.AddCookie(&http.Cookie{Name: middleware.CSRFCookieName, Value: csrf, Path: "/"})
	w2 := httptest.NewRecorder()
	srv.ServeHTTP(w2, r2)
	if w2.Code != http.StatusOK {
		t.Fatalf("delete want 200, got %d", w2.Code)
	}

	// INV-1: S3 上のオブジェクトは無傷
	if _, err := os.Stat(versionPath); err != nil {
		t.Fatalf("INV-1 違反: versions/ オブジェクトが消えている: %v", err)
	}

	// DB では state = 'trashed'
	var state string
	if err := env.Primary.QueryRowContext(context.Background(),
		"SELECT state FROM files WHERE id_bin = UNHEX(REPLACE(?, '-', ''))", fileID,
	).Scan(&state); err != nil && err != sql.ErrNoRows {
		t.Fatalf("query state: %v", err)
	}
	if state != "trashed" {
		t.Fatalf("after delete, state want 'trashed', got %q", state)
	}
}
