# sync-files-go E2E (Playwright)

Phase 5 のフロントエンド／HTMX UI を Playwright で検証するテストスイート。

## 前提

- Node.js 20+
- Docker Desktop 起動中（テストは `make compose-up` で立ち上がるアプリに対して走る）
- ローカル `localhost:8080` でサーバが応答する

## 初回セットアップ

プロジェクトルートで：

```bash
make compose-up           # アプリ + MySQL を起動
make db-migrate           # スキーマ適用
```

`tests/e2e/` で：

```bash
npm install
npm run install:browsers  # Chromium をダウンロード（初回のみ）
```

## 実行

```bash
# ヘッドレス（デフォルト）
npm test

# 開発時の対話モード
npm run test:ui

# ヘッド付きで手動確認
npm run test:headed

# レポート確認
npm run report
```

特定の spec のみ：

```bash
npx playwright test tests/auth.spec.ts
npx playwright test --grep "save_as_copy"
```

## スペック一覧

| spec | カバー範囲 |
|---|---|
| `auth.spec.ts` | サインアップ／ログイン／ログアウト／バリデーション |
| `upload.spec.ts` | ドロップゾーン表示／実アップロード→一覧反映 |
| `conflict.spec.ts` | OCC 409 → save_as_copy / force_overwrite の API 経由検証 |
| `trash.spec.ts` | 削除 → ゴミ箱 → 復元 |
| `share_links.spec.ts` | 公開リンク発行 → 管理画面表示 → 取り消し |
| `share.spec.ts` | 公開リンクの未認証ランディング表示 |
| `activity.spec.ts` | 監査ログタイムライン |
| `accessibility.spec.ts` | axe-core で Critical/Serious 違反ゼロ |
| `theme.spec.ts` | prefers-color-scheme（light/dark）と prefers-reduced-motion |

## 未実装（Phase 5+ または別 phase）

- `versions.spec.ts` — 過去バージョン UI（Phase 5+）
- `search.spec.ts` — 検索 UI（Phase 5+）
- `accessibility.spec.ts` の TOTP / 設定 詳細（Phase 5+）
- マルチブラウザ（Webkit / Firefox） — Phase 6 のリリースゲート

## CI への組み込み

`.github/workflows/e2e.yml`（Phase 6 で追加）から：

```yaml
- run: make compose-up
- run: make db-migrate
- run: cd tests/e2e && npm ci && npx playwright install --with-deps chromium
- run: cd tests/e2e && npm test
- if: failure()
  uses: actions/upload-artifact@v4
  with: { name: playwright-report, path: tests/e2e/playwright-report }
```

## トラブルシューティング

| 症状 | 対処 |
|---|---|
| `connect ECONNREFUSED localhost:8080` | `make compose-up` を打ち直して `make smoke-test` で 200 確認 |
| 「context deadline exceeded」（テスト中） | `npm run test:headed` で目視確認、必要なら Chromium のみ DB 操作を遅らせる |
| axe-core が 4.5:1 違反を出す | デザイントークン `--color-text-muted` の彩度を上げる |
| signup のメールが衝突する | テストは UUID 風メールで衝突回避済み。失敗時は MySQL で TRUNCATE users; |
