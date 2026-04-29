# static/js

このディレクトリは Go の `embed.FS` で取り込まれて `/static/js/*` として配信される。

## 配置するファイル

| ファイル | バージョン | 取得元 |
|---|---|---|
| `htmx.min.js` | 2.0.4 | https://unpkg.com/htmx.org@2.0.4/dist/htmx.min.js |
| `htmx-ext-sse.js` | 2.2.2 | https://unpkg.com/htmx-ext-sse@2.2.2/sse.js |
| `app.js` | 自作 | このリポジトリで管理 |

`htmx.min.js` と `htmx-ext-sse.js` はサードパーティ JS のためリポジトリには直接コミットしない方針。
セットアップ時に以下のいずれかで取得する：

```bash
# Makefile から
make ui-vendor

# 手動
curl -sSL -o internal/ui/static/js/htmx.min.js \
  https://unpkg.com/htmx.org@2.0.4/dist/htmx.min.js
curl -sSL -o internal/ui/static/js/htmx-ext-sse.js \
  https://unpkg.com/htmx-ext-sse@2.2.2/sse.js
```

取得後、SHA-256 を verify することを推奨：

```bash
shasum -a 256 internal/ui/static/js/htmx*.js
```

## なぜリポジトリにコミットしないか

- 監査ログを軽くする
- バージョン更新が `make ui-vendor` 一発になる
- `embed.FS` は **存在しないファイルを embed しようとするとビルドエラーになる** ため、`go build` 前に `make ui-vendor` を必ず通す

`.gitignore` に以下が入っていることを確認：

```
internal/ui/static/js/htmx.min.js
internal/ui/static/js/htmx-ext-sse.js
```
