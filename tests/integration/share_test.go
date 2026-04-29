//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/okamyuji/sync-files-go/internal/http/middleware"
)

// shareRequest 認証済みで share-link 作成のリクエストを組み立てる。
func shareRequest(t *testing.T, fileID, body string, sessCookie *http.Cookie, csrf string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/files/"+fileID+"/share-links", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-CSRF-Token", csrf)
	r.AddCookie(sessCookie)
	r.AddCookie(&http.Cookie{Name: middleware.CSRFCookieName, Value: csrf, Path: "/"})
	return r
}

// TestShare_CreateAndAccess 公開リンクを作成して、未認証ダウンロードが通る。
func TestShare_CreateAndAccess(t *testing.T) {
	env := SetupEnv(t)
	srv, cfg := makeServer(t, env)
	sessCookie, csrf, _ := makeAuthenticatedSession(t, env, cfg)

	// 1) ファイルアップロード
	r1 := uploadRequest(t, bytes.NewBufferString("public payload"), "/public.txt", "", "*", sessCookie, csrf)
	w1 := httptest.NewRecorder()
	srv.ServeHTTP(w1, r1)
	if w1.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", w1.Code, w1.Body.String())
	}
	fileID := w1.Header().Get("X-File-Id")

	// 2) share link 作成
	r2 := shareRequest(t, fileID, `{"expires_in":"1h"}`, sessCookie, csrf)
	w2 := httptest.NewRecorder()
	srv.ServeHTTP(w2, r2)
	if w2.Code != http.StatusCreated {
		t.Fatalf("create share want 201, got %d %s", w2.Code, w2.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(w2.Body).Decode(&resp); err != nil {
		t.Fatalf("decode share: %v", err)
	}
	url, _ := resp["url"].(string)
	if url == "" {
		t.Fatal("share url empty")
	}
	// URL から token を抽出
	token := url[strings.LastIndex(url, "/")+1:]

	// 3) 未認証で /share/{token} にアクセス
	r3 := httptest.NewRequest(http.MethodGet, "/share/"+token, nil)
	w3 := httptest.NewRecorder()
	srv.ServeHTTP(w3, r3)
	if w3.Code != http.StatusOK {
		t.Fatalf("public access want 200, got %d %s", w3.Code, w3.Body.String())
	}
	if w3.Body.String() != "public payload" {
		t.Fatalf("payload mismatch: %q", w3.Body.String())
	}
}

// TestShare_AutoRevokeOnDelete 元ファイルを削除すると公開リンクが auto-revoke される（H-2）。
func TestShare_AutoRevokeOnDelete(t *testing.T) {
	env := SetupEnv(t)
	srv, cfg := makeServer(t, env)
	sessCookie, csrf, _ := makeAuthenticatedSession(t, env, cfg)

	// アップロード
	r1 := uploadRequest(t, bytes.NewBufferString("disposable"), "/d.txt", "", "*", sessCookie, csrf)
	w1 := httptest.NewRecorder()
	srv.ServeHTTP(w1, r1)
	fileID := w1.Header().Get("X-File-Id")

	// share link 作成
	r2 := shareRequest(t, fileID, `{"expires_in":"1d"}`, sessCookie, csrf)
	w2 := httptest.NewRecorder()
	srv.ServeHTTP(w2, r2)
	var resp map[string]any
	_ = json.NewDecoder(w2.Body).Decode(&resp)
	url, _ := resp["url"].(string)
	token := url[strings.LastIndex(url, "/")+1:]

	// 元ファイルを削除
	rDel := httptest.NewRequest(http.MethodDelete, "/api/files/"+fileID, nil)
	rDel.Header.Set("X-CSRF-Token", csrf)
	rDel.AddCookie(sessCookie)
	rDel.AddCookie(&http.Cookie{Name: middleware.CSRFCookieName, Value: csrf, Path: "/"})
	wDel := httptest.NewRecorder()
	srv.ServeHTTP(wDel, rDel)
	if wDel.Code != http.StatusOK {
		t.Fatalf("delete want 200, got %d", wDel.Code)
	}

	// 公開リンクから取得 → 410 Gone（auto-revoke）
	r3 := httptest.NewRequest(http.MethodGet, "/share/"+token, nil)
	w3 := httptest.NewRecorder()
	srv.ServeHTTP(w3, r3)
	if w3.Code != http.StatusGone {
		t.Fatalf("after file delete, share access should be 410 Gone, got %d", w3.Code)
	}
}

// TestShare_RejectsExpiresInNone v1 では「期限なし」を拒否する（HIGH-3）。
func TestShare_RejectsExpiresInNone(t *testing.T) {
	env := SetupEnv(t)
	srv, cfg := makeServer(t, env)
	sessCookie, csrf, _ := makeAuthenticatedSession(t, env, cfg)

	// ファイル作成
	r1 := uploadRequest(t, bytes.NewBufferString("x"), "/expnone.txt", "", "*", sessCookie, csrf)
	w1 := httptest.NewRecorder()
	srv.ServeHTTP(w1, r1)
	fileID := w1.Header().Get("X-File-Id")

	// expires_in を空や "none" にすると拒否
	for _, body := range []string{`{"expires_in":""}`, `{"expires_in":"none"}`, `{"expires_in":"forever"}`} {
		r := shareRequest(t, fileID, body, sessCookie, csrf)
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("expires_in=%s: want 400, got %d", body, w.Code)
		}
	}
}

// TestShare_TokenIsRandom 同じファイルに 2 回 share link を作っても token は別。
func TestShare_TokenIsRandom(t *testing.T) {
	env := SetupEnv(t)
	srv, cfg := makeServer(t, env)
	sessCookie, csrf, _ := makeAuthenticatedSession(t, env, cfg)

	r1 := uploadRequest(t, bytes.NewBufferString("payload"), "/twin.txt", "", "*", sessCookie, csrf)
	w1 := httptest.NewRecorder()
	srv.ServeHTTP(w1, r1)
	fileID := w1.Header().Get("X-File-Id")

	tokens := make(map[string]bool)
	for i := 0; i < 2; i++ {
		r := shareRequest(t, fileID, `{"expires_in":"1h"}`, sessCookie, csrf)
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		var resp map[string]any
		_ = json.NewDecoder(w.Body).Decode(&resp)
		url, _ := resp["url"].(string)
		tokens[url] = true
	}
	if len(tokens) != 2 {
		t.Fatalf("expected 2 unique URLs, got %d", len(tokens))
	}
}
