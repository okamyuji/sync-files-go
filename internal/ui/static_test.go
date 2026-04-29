package ui

import (
	"io/fs"
	"strings"
	"testing"
)

// TestStaticFS  embed.FS に必要な静的アセットが含まれることを確認する。
// HTMX は make ui-vendor で導入される前提のためスキップする。
func TestStaticFS(t *testing.T) {
	t.Parallel()
	sub := StaticFS()

	required := []string{
		"css/tokens.css",
		"css/reset.css",
		"css/layout.css",
		"css/components.css",
		"js/app.js",
	}
	for _, name := range required {
		if _, err := fs.Stat(sub, name); err != nil {
			t.Errorf("missing static asset %q: %v", name, err)
		}
	}
}

// TestStaticFS_HTMXVendored ローカルで `make ui-vendor` 済みなら HTMX が embed されていることを確認する。
// 未取得時はスキップ（CI でも事前に make ui-vendor を実行する）。
func TestStaticFS_HTMXVendored(t *testing.T) {
	t.Parallel()
	sub := StaticFS()
	for _, name := range []string{"js/htmx.min.js", "js/htmx-ext-sse.js"} {
		if _, err := fs.Stat(sub, name); err != nil {
			t.Skipf("vendored htmx not present (%s): run `make ui-vendor`", name)
			return
		}
	}
	// HTMX 本体には typically "htmx" 文字列が含まれる
	b, err := fs.ReadFile(sub, "js/htmx.min.js")
	if err != nil {
		t.Fatalf("read htmx.min.js: %v", err)
	}
	if !strings.Contains(string(b[:min2(2000, len(b))]), "htmx") {
		t.Errorf("htmx.min.js does not contain expected marker; first bytes: %q", b[:min2(120, len(b))])
	}
}

func min2(a, b int) int {
	if a < b {
		return a
	}
	return b
}
