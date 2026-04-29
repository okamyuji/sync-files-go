package ui

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestLoad embed.FS にあるすべてのページがパースできることを確認する。
// テンプレート構文ミスの早期検知が目的。
func TestLoad(t *testing.T) {
	t.Parallel()
	r, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{
		"home",
		"auth/login",
		"auth/signup",
	}
	for _, n := range want {
		if _, ok := r.pages[n]; !ok {
			t.Errorf("page %q not registered", n)
		}
	}
}

func TestRender_Login(t *testing.T) {
	t.Parallel()
	r, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/login", nil)
	if err := r.Render(rec, req, 200, "auth/login", &PageData{Title: "サインイン"}); err != nil {
		t.Fatalf("Render: %v", err)
	}
	body := rec.Body.String()
	prefix := body
	if len(prefix) > 200 {
		prefix = prefix[:200]
	}
	if !strings.Contains(body, "<title>サインイン · sync-files-go</title>") {
		t.Errorf("title not rendered: %s", prefix)
	}
	if !strings.Contains(body, "/static/css/tokens.css") {
		t.Errorf("CSS link missing")
	}
	if !strings.Contains(body, "サインイン") {
		t.Errorf("page heading missing")
	}
}

func TestRender_Home_EmptyState(t *testing.T) {
	t.Parallel()
	r, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	data := &PageData{
		Title:       "ホーム",
		CurrentUser: &CurrentUser{ID: "u1", Email: "x@example.com"},
		Extra:       map[string]any{"Files": []map[string]any{}},
	}
	if err := r.Render(rec, req, 200, "home", data); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(rec.Body.String(), "まだファイルがありません") {
		t.Errorf("empty state not rendered")
	}
}

func TestRender_Home_WithFile(t *testing.T) {
	t.Parallel()
	r, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	files := []map[string]any{
		{
			"id":         "f1",
			"name":       "report.pdf",
			"size_bytes": int64(2048),
			"updated_at": "2026-04-29T12:00:00Z",
		},
	}
	data := &PageData{
		CurrentUser: &CurrentUser{ID: "u1", Email: "x@example.com"},
		Extra:       map[string]any{"Files": files},
	}
	if err := r.Render(rec, req, 200, "home", data); err != nil {
		t.Fatalf("Render: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "report.pdf") {
		t.Errorf("file name not rendered")
	}
	if !strings.Contains(body, "2.0 KiB") {
		t.Errorf("formatBytes did not apply: %s", body)
	}
}

func TestFormatBytes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1024 * 1024, "1.0 MiB"},
		{int64(1.5 * 1024), "1.5 KiB"},
	}
	for _, c := range cases {
		if got := formatBytes(c.in); got != c.want {
			t.Errorf("formatBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}
