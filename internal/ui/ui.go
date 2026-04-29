// Package ui HTML テンプレートと静的アセット配信を担当する。
//
// 設計書: docs/08-frontend-design.md §7（テンプレート構成）/ §5（デザイン方針）/ §6（HTMX）。
//
// 構成:
//   - templates/base.gohtml      共通レイアウト。各ページが `content` ブロックで埋める
//   - templates/partials/*       header / sidebar / file_row 等の再利用可能フラグメント
//   - templates/pages/*          ログイン後の各ページ
//   - templates/auth/*           未ログイン時のページ
//   - static/css/*, static/js/*  Brotli 圧縮配信向けに immutable + max-age=1y を付ける想定
//
// 起動時に `Load` で全テンプレートをパースし、以後は読み取り専用 map から参照する。
// 開発中のホットリロードは `dev` モードで `parseAll` を呼び直すことで対応（Phase 6 で考慮）。
package ui

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/okamyuji/sync-files-go/internal/http/middleware"
)

//go:embed templates
var templatesFS embed.FS

//go:embed static
var staticFS embed.FS

// StaticFS Embed 済み /static の fs.FS を返す。server で http.FileServerFS と組み合わせて使う。
func StaticFS() fs.FS {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(fmt.Errorf("ui: static sub fs: %w", err))
	}
	return sub
}

// Renderer 全テンプレートを保持し、`Render(w, r, name, data)` で 1 ページを返す。
type Renderer struct {
	pages map[string]*template.Template
}

// Load embed.FS から全テンプレートをパースし Renderer を返す。
//
// 各ページは「base.gohtml + 必要な partials + 自身」の単位で 1 つの *template.Template に束ねる。
// これにより `t.ExecuteTemplate(w, "base", data)` だけでページが描画できる。
func Load() (*Renderer, error) {
	r := &Renderer{pages: make(map[string]*template.Template)}

	pageFiles, err := collectPageFiles()
	if err != nil {
		return nil, err
	}

	partials, err := fs.Glob(templatesFS, "templates/partials/*.gohtml")
	if err != nil {
		return nil, fmt.Errorf("ui: glob partials: %w", err)
	}
	base := make([]string, 0, 1+len(partials))
	base = append(base, "templates/base.gohtml")
	base = append(base, partials...)

	for _, pf := range pageFiles {
		name := pageNameFromPath(pf)
		files := append([]string{}, base...)
		files = append(files, pf)
		t := template.New("base.gohtml").Funcs(templateFuncs())
		if _, err := t.ParseFS(templatesFS, files...); err != nil {
			return nil, fmt.Errorf("ui: parse %s: %w", pf, err)
		}
		r.pages[name] = t
	}
	return r, nil
}

// Render 指定ページを base レイアウトに流し込んで HTTP レスポンスに書く。
//
// `data` には PageData を渡すのが基本。テンプレ側からは `{{ .CSPNonce }}` `{{ .CSRFToken }}` 等で参照できる。
// ページ専用フィールドが欲しい場合は PageData に Extra を持たせる。
func (r *Renderer) Render(w http.ResponseWriter, req *http.Request, status int, page string, data *PageData) error {
	t, ok := r.pages[page]
	if !ok {
		return fmt.Errorf("ui: page %q not registered", page)
	}
	if data == nil {
		data = &PageData{}
	}
	data.fillFromRequest(req)

	// 先にバッファに描画してエラーを検知してから書き出す（途中で 200 を返してから panic を避けるため）。
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "base.gohtml", data); err != nil {
		return fmt.Errorf("ui: execute %s: %w", page, err)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
	return nil
}

// PageData 全テンプレートに渡される共通データ。各ページは Extra で固有情報を渡す。
type PageData struct {
	Title       string
	Path        string // 現在のリクエストパス（aria-current 判定用）
	CSPNonce    string
	CSRFToken   string
	CurrentUser *CurrentUser
	Flash       *Flash
	Extra       any
}

// CurrentUser ヘッダ・サイドバーで表示する最低限のユーザ情報。
type CurrentUser struct {
	ID    string
	Email string
}

// Flash 画面遷移後に 1 度だけ表示する通知。
type Flash struct {
	Kind    string // "success" | "warn" | "error"
	Message string
}

// fillFromRequest middleware が context に積んだ CSPNonce / CSRFToken を埋める。
func (p *PageData) fillFromRequest(r *http.Request) {
	if p.CSPNonce == "" {
		p.CSPNonce = middleware.CSPNonceFrom(r.Context())
	}
	if p.CSRFToken == "" {
		p.CSRFToken = middleware.CSRFTokenForTemplate(r)
	}
	if p.Path == "" {
		p.Path = r.URL.Path
	}
}

// collectPageFiles pages / auth 配下の .gohtml を相対パス配列で返す。
func collectPageFiles() ([]string, error) {
	var out []string
	for _, dir := range []string{"templates/pages", "templates/auth"} {
		entries, err := fs.ReadDir(templatesFS, dir)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("ui: read dir %s: %w", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if !strings.HasSuffix(e.Name(), ".gohtml") {
				continue
			}
			out = append(out, dir+"/"+e.Name())
		}
	}
	return out, nil
}

// pageNameFromPath 「templates/pages/home.gohtml」→「home」、「templates/auth/login.gohtml」→「auth/login」。
//
// pages 配下はフラット名、auth 配下は `auth/` プレフィックスを付けて衝突を避ける。
func pageNameFromPath(p string) string {
	base := path.Base(p)
	name := strings.TrimSuffix(base, ".gohtml")
	if strings.Contains(p, "/auth/") {
		return "auth/" + name
	}
	return name
}

// templateFuncs html/template に登録する補助関数。
func templateFuncs() template.FuncMap {
	return template.FuncMap{
		// formatBytes 80,000 -> "78.1 KB"。
		"formatBytes": func(b int64) string { return formatBytes(b) },
		// formatTime 表示用の絶対時刻。Asia/Tokyo は将来ユーザ設定で切替。
		"formatTime": func(t time.Time) string {
			if t.IsZero() {
				return ""
			}
			return t.Local().Format("2006-01-02 15:04")
		},
		// isCurrent サイドバー / ナビで aria-current 用。完全一致のみ。
		"isCurrent": func(current, target string) bool { return current == target },
		// pathStartsWith「現在 /trash/123」かつ target「/trash」で true（折りたたみ親メニュー判定）。
		"pathStartsWith": func(current, target string) bool {
			if target == "/" {
				return current == "/"
			}
			return current == target || strings.HasPrefix(current, target+"/")
		},
		// dict template 内で map を作って partial に渡すためのヘルパ。
		"dict": func(values ...any) (map[string]any, error) {
			if len(values)%2 != 0 {
				return nil, errors.New("dict: odd number of arguments")
			}
			out := make(map[string]any, len(values)/2)
			for i := 0; i < len(values); i += 2 {
				k, ok := values[i].(string)
				if !ok {
					return nil, fmt.Errorf("dict: key %d is not string", i)
				}
				out[k] = values[i+1]
			}
			return out, nil
		},
	}
}

// formatBytes バイトを KiB / MiB / GiB 単位で人間可読化する。
func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
