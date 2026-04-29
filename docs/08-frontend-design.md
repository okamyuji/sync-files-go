# 08. フロントエンド設計（HTMX + 質素モダン）

## 1. 設計の核

| 原則 | 意味 |
|---|---|
| **サーバ・レンダー優先** | ページ遷移・部分更新ともに HTML を返す。JSON API はバックエンド内部用に限定 |
| **HTMX で十分** | SPA フレームワーク（React / Vue 等）を採用しない。DOM 同期は `hx-swap` の流儀で |
| **質素であって退屈ではない** | デフォルトテンプレ感を排し、タイポ・余白・コントラストで質を出す |
| **アクセシビリティを後付けにしない** | キーボード操作・スクリーンリーダー・`prefers-reduced-motion` を最初から |
| **進捗的強化** | JavaScript が無効でもアップロード・ダウンロード・閲覧の主要フローは動く |

## 2. 技術スタック

| 層 | 採用 | 不採用（理由） |
|---|---|---|
| サーバテンプレート | `html/template`（Go 標準） | Pug / Mustache 系（標準で十分） |
| クライアント | HTMX 4.x + `htmx-ext-sse` | React / Vue / Svelte（NG-3 / NG-7） |
| CSS | Vanilla CSS（Custom Properties） | Tailwind / Bootstrap（量を最小化、テンプレ感回避） |
| アイコン | SVG インライン（自前で 12 種類程度） | Material Icons 等（外部依存削減） |
| フォント | システムフォントスタック中心 + 1 つだけ Web フォント（`Geist`, `Inter`, `IBM Plex Sans` 等から最終選択） | 過剰な Web フォント |
| アップロード補助 | tus-js-client（必要時のみ） | uppy（重い） |

## 3. 画面構成

```
                                ┌── 認証画面
                                │   ├── /login
                                │   ├── /signup
                                │   ├── /totp/setup
                                │   └── /password-reset
                                │
                                │── 主要画面
                                │   ├── /                   ホーム（ファイル一覧）
                                │   ├── /folders/{path}     フォルダビュー
                                │   ├── /search             検索結果
                                │   ├── /trash              ゴミ箱
                                │   ├── /activity           アクティビティタイムライン
                                │   └── /share-links        発行済み公開リンク管理
                                │
                                │── ファイル操作
                                │   ├── /files/{id}             プレビュー
                                │   ├── /files/{id}/download    ダウンロード
                                │   └── /files/{id}/versions    バージョン履歴
                                │
                                │── 設定
                                │   ├── /settings               一般
                                │   ├── /settings/security      パスワード / 2FA
                                │   └── /settings/sessions      アクティブセッション
                                │
                                │── 公開リンク（未認証）
                                │   ├── /share/{id}                プレビュー
                                │   ├── /share/{id}/password       パスワード入力
                                │   └── /share/{id}/download       ダウンロード
                                │
                                └── システム
                                    ├── /healthz
                                    └── /readyz
```

## 4. レイアウト構造（ホーム例）

```
┌──────────────────────────────────────────────────────────────────────┐
│  [ロゴ sync-files-go]      パンくず        [検索...]   [+] [SSEバッジ] │  ← ヘッダ (sticky)
├──────────────────────────────────────────────────────────────────────┤
│                              │                                        │
│  サイドナビ（折りたたみ可）  │  メイン                                │
│  ┌────────────────────────┐  │  ┌──────────────────────────────────┐ │
│  │ ▾ ファイル              │  │  │ 並び替え | フィルタ | 表示切替    │ │
│  │   ▸ ホーム              │  │  ├──────────────────────────────────┤ │
│  │   ▸ お気に入り (v2)     │  │  │ 名前         サイズ  更新日 操作  │ │
│  │ ▸ 共有リンク             │  │  │ ───────────────────────────────  │ │
│  │ ▸ ゴミ箱                 │  │  │ 📄 Q2.docx   84KB    14:32  ⋯  │ │
│  │ ▾ 設定                   │  │  │ 📄 photo.jpg  2.1MB  09:11  ⋯  │ │
│  │   ▸ アカウント           │  │  │ ...                              │ │
│  │   ▸ セキュリティ         │  │  └──────────────────────────────────┘ │
│  └────────────────────────┘  │                                        │
│                              │  ドラッグ&ドロップアップロード ZONE     │
│                              │  （透過オーバーレイ）                   │
└──────────────────────────────────────────────────────────────────────┘
```

## 5. 質素モダン デザイン方針

### 5.1 デザイン原則

1. **タイポグラフィを主役に**：見出しと本文のスケール差で階層を作る（`clamp(1rem, 0.92rem + 0.4vw, 1.125rem)` スタイルの流体タイポ）
2. **余白で語る**：要素を詰め込まず、息継ぎをする
3. **2 色 + ニュートラルだけで構成**：アクセント 1 + 警告 1 + 5 段階のニュートラルグレー
4. **影は 1 種類のみ**：使い分けると安っぽくなる
5. **モーション控えめ**：ホバー / フォーカスのみ、トランジション 200ms 以下

### 5.2 デザイントークン（CSS Custom Properties）

```css
:root {
  /* 色（OKLCH を採用、知覚均等で扱いやすい） */
  --color-bg:           oklch(98% 0 0);
  --color-surface:      oklch(100% 0 0);
  --color-surface-2:    oklch(96% 0 0);
  --color-text:         oklch(20% 0 0);
  --color-text-muted:   oklch(45% 0 0);
  --color-border:       oklch(90% 0 0);
  --color-accent:       oklch(60% 0.18 250);   /* 落ち着いた青 */
  --color-accent-soft:  oklch(95% 0.04 250);
  --color-warn:         oklch(60% 0.20 30);    /* 控えめな赤 */
  --color-success:      oklch(65% 0.15 150);

  /* タイポ */
  --font-sans: ui-sans-serif, system-ui, -apple-system, "Segoe UI",
               "Hiragino Sans", "Noto Sans JP", sans-serif;
  --font-mono: ui-monospace, "SF Mono", "JetBrains Mono", monospace;

  --text-xs:   clamp(0.75rem, 0.7rem + 0.1vw, 0.8125rem);
  --text-sm:   clamp(0.85rem, 0.8rem + 0.2vw, 0.9375rem);
  --text-base: clamp(1rem,  0.95rem + 0.2vw, 1.05rem);
  --text-lg:   clamp(1.15rem, 1.05rem + 0.4vw, 1.35rem);
  --text-xl:   clamp(1.5rem,  1.2rem + 1vw,   2rem);

  /* スペーシング */
  --space-1: 0.25rem;
  --space-2: 0.5rem;
  --space-3: 0.75rem;
  --space-4: 1rem;
  --space-6: 1.5rem;
  --space-8: 2rem;
  --space-12: 3rem;

  /* 影 */
  --shadow-1: 0 1px 0 rgba(0,0,0,0.04), 0 8px 24px -12px rgba(0,0,0,0.08);

  /* モーション */
  --duration-fast:   150ms;
  --duration-normal: 200ms;
  --ease: cubic-bezier(0.2, 0, 0, 1);
}

@media (prefers-color-scheme: dark) {
  :root {
    --color-bg:        oklch(15% 0 0);
    --color-surface:   oklch(18% 0 0);
    --color-surface-2: oklch(22% 0 0);
    --color-text:      oklch(96% 0 0);
    --color-text-muted: oklch(72% 0 0);
    --color-border:    oklch(28% 0 0);
    --color-accent:    oklch(70% 0.16 250);
    --color-accent-soft: oklch(28% 0.06 250);
  }
}

@media (prefers-reduced-motion: reduce) {
  :root { --duration-fast: 0ms; --duration-normal: 0ms; }
  *, *::before, *::after { animation-duration: 0ms !important; transition-duration: 0ms !important; }
}
```

### 5.3 ベースリセット

```css
*, *::before, *::after { box-sizing: border-box; }
html { color-scheme: light dark; }
body {
  margin: 0;
  font-family: var(--font-sans);
  color: var(--color-text);
  background: var(--color-bg);
  font-size: var(--text-base);
  line-height: 1.55;
  -webkit-font-smoothing: antialiased;
}
:focus-visible {
  outline: 2px solid var(--color-accent);
  outline-offset: 2px;
}
```

## 6. HTMX の使い方

### 6.1 ファイル一覧の差し替え（HTMX swap）

```html
<form
  hx-get="/search"
  hx-trigger="input changed delay:200ms, search"
  hx-target="#file-list"
  hx-swap="outerHTML"
  hx-push-url="true">
  <input type="search" name="q" placeholder="検索..." aria-label="ファイル検索" />
</form>

<section id="file-list">
  <!-- サーバが返す部分テンプレート -->
</section>
```

### 6.2 アップロード進捗

```html
<form
  hx-encoding="multipart/form-data"
  hx-post="/files"
  hx-headers='{"If-None-Match": "*"}'
  hx-target="#upload-result"
  hx-on:htmx:xhr:progress="updateProgress(event.detail.loaded, event.detail.total)"
  hx-on:htmx:after-request="resetProgress()">
  <input type="file" name="file" required />
  <button type="submit">アップロード</button>
  <progress id="upload-progress" max="100" value="0"></progress>
</form>

<script nonce="{{.CspNonce}}">
function updateProgress(loaded, total) {
  const p = document.getElementById('upload-progress');
  p.value = (loaded / total) * 100;
}
function resetProgress() {
  document.getElementById('upload-progress').value = 0;
}
</script>
```

### 6.3 SSE 通知

```html
<div id="notifications"
     hx-ext="sse"
     sse-connect="/sse"
     sse-swap="file_changed,file_deleted,share_created"
     hx-target="#notif-area"
     hx-swap="afterbegin">
</div>

<aside id="notif-area" aria-live="polite"></aside>
```

サーバ側では Content-Type: text/event-stream で JSON ではなく **HTML フラグメント** を返す（HTMX 4.x の流儀）。例：

```
event: file_changed
data: <div class="notif" data-file-id="...">変更があります</div>
```

### 6.4 競合モーダル（HX-Trigger）

サーバが 409 を返すとき、`HX-Trigger: openConflictModal` ヘッダで JS イベントを発火し、JSON 内容で動的描画：

```html
<dialog id="conflict-modal">
  <!-- HTMX で innerHTML が差し替えられる -->
</dialog>

<script nonce="{{.CspNonce}}">
document.body.addEventListener('openConflictModal', (e) => {
  const data = e.detail; // 409 のレスポンス JSON
  // 動的に dialog の中を組み立てる（テンプレートエンジンとしての小さな関数）
  document.getElementById('conflict-modal').showModal();
});
</script>
```

可能なら **dialog 自身をサーバが部分テンプレートとして返す** 方がシンプル。設計判断は実装時。

## 7. テンプレート構成

```
internal/ui/templates/
├── base.gohtml                  -- 共通レイアウト
├── partials/
│   ├── header.gohtml
│   ├── sidebar.gohtml
│   ├── file_row.gohtml
│   ├── conflict_modal.gohtml
│   ├── notification.gohtml
│   └── empty_state.gohtml
├── pages/
│   ├── home.gohtml
│   ├── search.gohtml
│   ├── trash.gohtml
│   ├── activity.gohtml
│   ├── settings.gohtml
│   └── share.gohtml             -- 公開リンクの未認証ページ
└── auth/
    ├── login.gohtml
    ├── signup.gohtml
    └── totp_setup.gohtml
```

部分テンプレートはそれぞれ独立して HTMX swap できるよう設計。

## 8. アクセシビリティ

| 観点 | 対策 |
|---|---|
| キーボード | すべての操作が Tab だけで到達可能。フォーカスインジケーター明示 |
| スクリーンリーダー | `aria-label`, `aria-live="polite"`, `aria-current="page"`, ランドマーク (`<header>`, `<nav>`, `<main>`) |
| コントラスト | 4.5:1 以上 (axe-core で CI 検証) |
| `prefers-reduced-motion` | アニメーション 0ms |
| `prefers-color-scheme` | ライト/ダーク両方をデザイン |
| フォーム | `<label>` を必ず関連付け、エラーは `aria-describedby` |
| モーダル | `<dialog>` ネイティブ要素を採用（`showModal()`）、ESC で閉じる、フォーカストラップ |

## 9. パフォーマンス目標

| 指標 | 目標 |
|---|---|
| LCP (Largest Contentful Paint) | < 2.5s (p75) |
| INP (Interaction to Next Paint) | < 200ms |
| CLS | < 0.1 |
| JavaScript 総量 (gzip) | < 50KB（HTMX + tus-js-client + 自作 5KB 程度） |
| CSS 総量 (gzip) | < 20KB |

施策：
- HTML / CSS / JS を Brotli で圧縮配信
- 静的アセットに `Cache-Control: public, max-age=31536000, immutable`（バージョニング URL）
- フォントは subset + `font-display: swap`
- 画像プレビューは AVIF / WebP の自動選択（サーバ側変換は v2、v1 は元ファイル直接）

## 10. デザインのアンチパターン回避

「テンプレ感」を生む典型を避ける：

| 禁忌 | 代替 |
|---|---|
| 中央配置のヒーロー + グラデブロブ | 左寄せ非対称、編集中ファイルの実物を並べる |
| 4 段の機能カードを横並び | 不揃い高さの bento レイアウトで階層を出す |
| 当て字のスタックフォント | 1 つだけ意図のあるフォント |
| パディング均一の Card | コンテンツに応じた可変パディング |
| カラフルすぎるアクセント | 1 色 + ニュートラルで強弱 |
| 不要な装飾アイコン | 必要な場所だけアイコン |

[共通ガイドライン: web/design-quality.md](https://example.invalid/) に準拠（自分の rules ディレクトリの `~/.claude/rules/web/design-quality.md` の方針）。

## 11. 国際化

- テンプレートは `{{ T "key.name" }}` ヘルパーで管理（v1 は日本語のみ）
- 文字列表は `internal/ui/i18n/ja.json` / `en.json`
- 日付フォーマット: `time.Format("2006-01-02 15:04 MST")` をサーバ側で変換
- タイムゾーン: ユーザ設定（デフォルト Asia/Tokyo）

## 12. テスト方針

- HTML スナップショットテスト（`html/template` のレンダリング結果）
- Playwright で主要な UI フロー（[`11`](./11-testing-strategy.md) §3）
- axe-core でアクセシビリティ自動検証
- Lighthouse でパフォーマンス検証（CI に組み込まない、ローカル手動）

## 13. 「派手にしない」ことの設計上の意味

- アニメーションを抑える → 端末性能の差で UX が劣化しない
- 色数を絞る → ダークモード対応が容易
- JS 量が少ない → 古いブラウザでも動く・初回表示が速い
- HTMX 中心 → SPA の状態管理バグ群（ルータ・ストア・キャッシュ）を持たない

「シンプル」は手抜きの言い訳ではなく、信頼性の上流対策である。

---

次の章: [`09-infrastructure-and-deployment.md`](./09-infrastructure-and-deployment.md)
