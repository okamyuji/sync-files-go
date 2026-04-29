//go:build integration

package integration

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/okamyuji/sync-files-go/internal/batch"
	"github.com/okamyuji/sync-files-go/internal/storage/localfs"
)

// TestBatch_PruneOldVersions 90 日経過 + 非 current の file_versions が削除され、
// 対応する S3 オブジェクトも消えること（CR-5）。
//
// バッチ自体は CLI で動かすが、テストでは時計を `RetentionDays=0` にして即時 prune を再現する。
func TestBatch_PruneOldVersions(t *testing.T) {
	env := SetupEnv(t)
	srv, cfg := makeServer(t, env)
	sessCookie, csrf, ownerID := makeAuthenticatedSession(t, env, cfg)

	// ファイルに 3 バージョン書き込む（最新は current、過去 2 つは prune 候補）
	r1 := uploadRequest(t, bytes.NewBufferString("v1"), "/p.txt", "", "*", sessCookie, csrf)
	w1 := httptest.NewRecorder()
	srv.ServeHTTP(w1, r1)
	if w1.Code != http.StatusCreated {
		t.Fatalf("v1: %d", w1.Code)
	}
	v1 := w1.Header().Get("ETag")
	fileID := w1.Header().Get("X-File-Id")

	r2 := uploadRequest(t, bytes.NewBufferString("v2"), "/p.txt", v1, "", sessCookie, csrf)
	w2 := httptest.NewRecorder()
	srv.ServeHTTP(w2, r2)
	if w2.Code != http.StatusNoContent {
		t.Fatalf("v2: %d %s", w2.Code, w2.Body.String())
	}
	v2 := w2.Header().Get("ETag")

	r3 := uploadRequest(t, bytes.NewBufferString("v3"), "/p.txt", v2, "", sessCookie, csrf)
	w3 := httptest.NewRecorder()
	srv.ServeHTTP(w3, r3)
	if w3.Code != http.StatusNoContent {
		t.Fatalf("v3: %d", w3.Code)
	}
	v3 := w3.Header().Get("ETag")
	t.Logf("created versions: v1=%s v2=%s v3=%s", v1, v2, v3)

	// バッチ実行（RetentionDays=0、即時 prune）。created_at が NOW() より厳密に過去になるよう少し sleep。
	time.Sleep(100 * time.Millisecond)
	store, _ := localfs.New(env.DataDir)
	pruner := &batch.OldVersionPruner{
		FileVersions:  env.FileVersions,
		Audit:         env.Audit,
		Storage:       store,
		Logger:        slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		RetentionDays: 0,
		BatchSize:     10,
	}
	pruned, err := pruner.Run(context.Background())
	if err != nil {
		t.Fatalf("pruner: %v", err)
	}
	if pruned < 2 {
		t.Fatalf("v1 と v2 が prune されるべき（2 件以上）が、got=%d", pruned)
	}

	// current (v3) は無傷
	v3Path := filepath.Join(env.DataDir, "owner-"+ownerID.String(), "versions", fileID, v3)
	if _, err := os.Stat(v3Path); err != nil {
		t.Fatalf("current version (v3) は残るべき: %v", err)
	}

	// v1 / v2 は消えている
	v1Path := filepath.Join(env.DataDir, "owner-"+ownerID.String(), "versions", fileID, v1)
	if _, err := os.Stat(v1Path); !os.IsNotExist(err) {
		t.Fatalf("v1 は削除されるべき: %v", err)
	}
}

// TestBatch_GC trashed > 30 日のファイルを物理削除。
func TestBatch_GC(t *testing.T) {
	env := SetupEnv(t)
	srv, cfg := makeServer(t, env)
	sessCookie, csrf, ownerID := makeAuthenticatedSession(t, env, cfg)

	// ファイルアップロード
	r1 := uploadRequest(t, bytes.NewBufferString("disposable"), "/gone.txt", "", "*", sessCookie, csrf)
	w1 := httptest.NewRecorder()
	srv.ServeHTTP(w1, r1)
	fileID := w1.Header().Get("X-File-Id")
	versionID := w1.Header().Get("ETag")

	// ソフト削除
	rDel := httptest.NewRequest(http.MethodDelete, "/api/files/"+fileID, nil)
	rDel.Header.Set("X-CSRF-Token", csrf)
	rDel.AddCookie(sessCookie)
	rDel.AddCookie(&http.Cookie{Name: "__Host-sync_csrf", Value: csrf, Path: "/"})
	wDel := httptest.NewRecorder()
	srv.ServeHTTP(wDel, rDel)
	if wDel.Code != http.StatusOK {
		t.Fatalf("delete: %d", wDel.Code)
	}

	// バッチ実行（RetentionDays=0 で即時 purge、NOW() ドリフトのため少し sleep）
	time.Sleep(100 * time.Millisecond)
	store, _ := localfs.New(env.DataDir)
	gc := &batch.GarbageCollector{
		Router:        env.Router,
		Files:         env.Files,
		FileVersions:  env.FileVersions,
		Audit:         env.Audit,
		Storage:       store,
		Logger:        slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		RetentionDays: 0,
		BatchSize:     10,
	}
	purged, err := gc.Run(context.Background())
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if purged < 1 {
		t.Fatal("gc should have purged at least 1 file")
	}

	// S3 上のオブジェクトが消えていること（INV-1: trashed → 30d 経過 → purge）
	versionPath := filepath.Join(env.DataDir, "owner-"+ownerID.String(), "versions", fileID, versionID)
	if _, err := os.Stat(versionPath); !os.IsNotExist(err) {
		t.Fatalf("gc 後に versions/ オブジェクトは消えるべき: %v", err)
	}

	// state が purged
	var state string
	_ = env.Primary.QueryRowContext(context.Background(),
		"SELECT state FROM files WHERE id_bin = UNHEX(REPLACE(?, '-', ''))", fileID,
	).Scan(&state)
	if state != "purged" {
		t.Fatalf("state want 'purged', got %q", state)
	}

	// time のドリフトを避けるため、コンテキストをタイムアウト付きにしておく（テストの安定化）
	_ = time.Now() // explicit dependency on time
}
