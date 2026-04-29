//go:build integration

package integration

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/okamyuji/sync-files-go/internal/http/middleware"
	"github.com/okamyuji/sync-files-go/internal/repo"
)

// TestRAW_CookieSetAndPropagate HIGH-1 修正の確認：
//   - 書き込み完了レスポンスで __Host-sync_raw_until cookie が出る
//   - 後続リクエストでミドルウェアがその cookie を検証して ctx に焼き、Reader が Primary を返す
func TestRAW_CookieSetAndPropagate(t *testing.T) {
	env := SetupEnv(t)
	srv, cfg := makeServer(t, env)
	sessCookie, csrf, _ := makeAuthenticatedSession(t, env, cfg)

	r1 := uploadRequest(t, bytes.NewBufferString("payload"), "/raw.txt", "", "*", sessCookie, csrf)
	w1 := httptest.NewRecorder()
	srv.ServeHTTP(w1, r1)
	if w1.Code != http.StatusCreated {
		t.Fatalf("upload failed: %d %s", w1.Code, w1.Body.String())
	}

	var rawCookie *http.Cookie
	for _, c := range w1.Result().Cookies() {
		if c.Name == middleware.RAWCookieName {
			rawCookie = c
			break
		}
	}
	if rawCookie == nil {
		t.Fatal("expected RAW cookie to be set")
	}
	if rawCookie.Value == "" {
		t.Fatal("RAW cookie value empty")
	}

	// 後続リクエストに RAW cookie を載せて、ミドルウェアが ctx を作るか確認
	r2 := httptest.NewRequest(http.MethodGet, "/api/files", nil)
	r2.AddCookie(sessCookie)
	r2.AddCookie(rawCookie)
	w2 := httptest.NewRecorder()
	srv.ServeHTTP(w2, r2)
	if w2.Code != http.StatusOK {
		t.Fatalf("list with RAW cookie want 200, got %d", w2.Code)
	}
	// 直接 ctx の中身を見ることはできないが、ハンドラ内で DBRouter.Reader が
	// 動作することは別途 dbrouter_test.go で単体テスト済み。ここはエンドツーエンドの疎通確認。
}

// TestRAW_TamperedCookieRejected HMAC が壊れた cookie は無視される（攻撃シナリオ）。
func TestRAW_TamperedCookieRejected(t *testing.T) {
	env := SetupEnv(t)
	srv, cfg := makeServer(t, env)
	sessCookie, _, _ := makeAuthenticatedSession(t, env, cfg)

	// 偽の RAW cookie を作る（HMAC が嘘）
	tampered := &http.Cookie{
		Name:  middleware.RAWCookieName,
		Value: "9999999999.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", // 改ざん
		Path:  "/",
	}

	r := httptest.NewRequest(http.MethodGet, "/api/files", nil)
	r.AddCookie(sessCookie)
	r.AddCookie(tampered)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	// 正常応答を期待（無効な cookie は単に無視される。エラーを返さない）
	if w.Code != http.StatusOK {
		t.Fatalf("tampered cookie should be ignored, want 200 got %d", w.Code)
	}
}

// TestRAW_ExpiredCookieIgnored 期限切れの cookie は ctx に乗らないこと。
//
// 詳細な ctx 検査は単体テスト dbrouter_test.go 側で実施済み。ここは
// DBRouter が期限切れ ctx を Reader → Replica にフォールバックさせることを実 DB 接続で確認。
func TestRAW_ExpiredCookieIgnored(t *testing.T) {
	env := SetupEnv(t)
	expired := time.Now().Add(-1 * time.Hour)
	expiredCtx := repo.WithReadAfterWrite(context.Background(), expired)
	if env.Router.Reader(expiredCtx) != env.Replica {
		t.Fatal("expired RAW window should fall back to Replica")
	}
}
