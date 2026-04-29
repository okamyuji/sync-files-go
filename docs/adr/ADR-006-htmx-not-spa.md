# ADR-006: フロントエンドは HTMX。SPA フレームワークを不採用

## ステータス

採択 (2026-04-29)

## コンテキスト

UI 層に何を採用するか。

## 検討した選択肢

### A. React / Vue / Svelte ベースの SPA

利点：
- リッチな UX
- コンポーネント再利用
- エコシステム豊富

欠点：
- バンドルサイズ
- 状態管理の複雑さ（ストア・キャッシュ・ルータ）
- SPA 特有のバグ（ハイドレーション・履歴・ブラウザバック）
- SSR / SSG で同じことを得るのに別のフレームワーク（Next.js 等）が必要

### B. HTMX + サーバ側 HTML テンプレート ← **採択**

利点：
- HTML がそのままレスポンス。バックエンドの `html/template` だけで完結
- 必要に応じて部分テンプレートを返して `hx-swap`
- JS 量が極小（HTMX 4.x で gzip 後 20KB 程度）
- 進捗的強化：JS 無効でも基本機能が動く
- SSE 拡張で「軽いリアルタイム性」も得られる

欠点：
- 高度なクライアント状態（オフラインモード等）には不向き → v1 のスコープでは不要

### C. Vanilla JS + サーバ側 HTML

利点：依存ゼロ

欠点：HTMX が解決してくれる典型的なボイラープレート（部分更新・SSE 連携）を毎回書くことになる

## 決定

**選択肢 B（HTMX）** を採択する。具体的には：

- HTMX 4.x を採用
- `htmx-ext-sse` を SSE 通知に使用
- 必要な箇所のみ vanilla JS（数十行）：競合モーダル動的描画、進捗表示、CSP nonce 連携
- CSS は Vanilla（Tailwind 等の大型 CSS フレームワークも不採用）
- JavaScript の総量を gzip 後 50KB 以下に抑える

## 帰結

- 08-frontend-design.md にデザインとテンプレート構成
- v1 では SPA 系ライブラリを依存に入れない
- v3 候補：本格的なクライアント側暗号化を入れる場合は WebCrypto 用の最小 JS を追加

## リンク

- [`08-frontend-design.md`](../08-frontend-design.md)
- [HTMX 公式](https://htmx.org/)
- [HTMX SSE Extension](https://htmx.org/extensions/sse/)
