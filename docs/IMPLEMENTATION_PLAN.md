# 実装計画 (Implementation Plan)

> 設計書（[`docs/README.md`](./README.md) の読み順）に沿って sync-files-go を実装する計画。
> 各 Phase は独立コミット単位とし、完了時に `make test && make docker-build` が green になることを最低条件とする。

## ゴール

- v1 機能スコープ（[`01-requirements.md`](./01-requirements.md)）をすべて実装する
- 不変条件 INV-1〜INV-5（[`04-sync-semantics.md`](./04-sync-semantics.md)）を構造的に守る
- ローカル Docker での E2E green → Terraform デプロイ後の実ブラウザ統合テスト green でリリース可

## Phase 一覧

| Phase | 内容 | 主要設計章 | 出力 |
|---|---|---|---|
| **P1** | スケルトン・ローカル開発基盤 | 02 / 09 §13 | go.mod, Makefile, Dockerfile×2, docker-compose, migrations 骨格 |
| **P2** | コアドメイン・永続化層 | 03 / 04 / 06 / 07 §4 | internal/domain, internal/repo (DBRouter), internal/storage, internal/crypto |
| **P3** | HTTP 層・ミドルウェア | 02 §3.4 / 04 / 05 / 07 | internal/http (middleware + handlers), cmd/server |
| **P4** | フロントエンド (HTMX) | 08 | internal/ui/templates, internal/ui/static |
| **P5** | テスト | 11 | tests/unit, tests/integration, tests/e2e |
| **P6** | Terraform IaC | 09 | deploy/terraform/{modules,envs} |

## Phase 1: スケルトン・ローカル開発基盤

### 成果物

```
sync-files-go/
├── go.mod, go.sum                          # Go module
├── Makefile                                # build/test/lint/migrate/run
├── deploy/docker/
│   ├── app.Dockerfile                      # multistage, distroless
│   ├── nginx.Dockerfile                    # alpine + 設定
│   ├── nginx.conf                          # TLS終端 + rate limit + SSE 対応
│   └── docker-compose.yml                  # mysql + app + nginx
├── migrations/
│   └── 0001_init.sql                       # 03 §4 のスキーマ
├── cmd/server/main.go                      # 最小エントリーポイント (起動 → /healthz 返却)
└── internal/config/config.go               # 環境変数読み込み
```

### 受け入れ基準

- [ ] `docker compose up` で mysql + app が起動し、`curl localhost:8080/healthz` が 200 を返す
- [ ] `make test` が（空でも）走る
- [ ] `make docker-build TAG=dev` でイメージが arm64 でビルドできる
- [ ] migrations が MySQL コンテナに対して `make db-migrate` で適用できる

## Phase 2: コアドメイン・永続化層

### 成果物

```
internal/
├── domain/                                 # ピュアな業務ロジック (依存ゼロ)
│   ├── file.go                             # File / FileVersion 構造体 + 状態遷移
│   ├── version.go
│   ├── trash.go
│   ├── share_link.go                       # token 生成・SHA-256
│   ├── audit.go
│   └── user.go
├── crypto/                                 # 暗号プリミティブ
│   ├── aead.go                             # Tink Streaming AEAD ラッパ
│   ├── kek.go                              # AES-Key-Wrap (RFC 3394)
│   ├── totp.go                             # HMAC-SHA1 RFC 6238
│   ├── password.go                         # Argon2id wrapper
│   └── random.go                           # 安全な乱数 (token, nonce など)
├── repo/                                   # 永続化抽象
│   ├── repo.go                             # interface 群 (FilesReader/FilesWriter, etc.)
│   ├── dbrouter.go                         # DBRouter (Reader/Writer/RAW window)
│   ├── mysql/
│   │   ├── files.go
│   │   ├── file_versions.go
│   │   ├── folders.go
│   │   ├── tags.go
│   │   ├── share_links.go
│   │   ├── share_link_accesses.go
│   │   ├── users.go
│   │   ├── sessions.go
│   │   ├── audit.go
│   │   ├── upload_sessions.go
│   │   └── rate_limit.go
│   └── memory/                             # テスト用 in-memory 実装
└── storage/
    ├── storage.go                          # interface (Open/Create/Rename/Remove/Stat)
    ├── localfs/                            # os パッケージで実装 (S3 Files NFS マウントもこれを使う)
    └── memory/                             # テスト用
```

### 受け入れ基準

- [ ] DBRouter が Writer / Reader / RAW window / 縮退運転 の 4 ケースを正しく分岐
- [ ] File 状態遷移 (draft → active → trashed → purged → gone) が enum + 関数で表現される
- [ ] Tink Streaming AEAD で 1MB / 100MB / 2GB のラウンドトリップが通る (テストデータ)
- [ ] active_marker UNIQUE が同名 active 二重 INSERT を拒否する (統合テスト)
- [ ] OCC (`If-Match`) ロジックが §04 §4.2 の 7 分岐を網羅

## Phase 3: HTTP 層・ミドルウェア

### 成果物

```
internal/http/
├── middleware/
│   ├── auth.go                             # セッション cookie 検証
│   ├── csrf.go                             # double-submit cookie
│   ├── ratelimit.go                        # token bucket (DB 由来)
│   ├── raw.go                              # __Host-sync_raw_until cookie 処理 (HIGH 修正)
│   ├── logging.go                          # slog 構造化ログ
│   ├── recovery.go                         # panic → 500
│   └── csp.go                              # CSP nonce 注入
├── handler_auth.go                         # signup, login, logout, totp_setup, recovery
├── handler_files.go                        # GET/POST/PUT/PATCH/DELETE /files/{id}
├── handler_upload.go                       # tus.io 互換 PATCH /uploads/{id}
├── handler_folder.go
├── handler_share.go                        # /files/{id}/share-links と /share/{token}
├── handler_search.go
├── handler_activity.go
├── handler_undo.go
├── handler_sse.go                          # /sse 通知
├── handler_health.go                       # /healthz, /readyz
└── router.go                               # net/http の ServeMux 構築

cmd/server/
└── main.go                                 # 全体結線
```

### 受け入れ基準

- [ ] 共通ガード ([5/file-operations §0]) を全エンドポイントが通過
- [ ] OCC：If-Match なし → 428、不一致 → 409 Conflict + JSON
- [ ] 公開リンク取得は Primary 強制
- [ ] CSP nonce / HSTS / X-Content-Type-Options 等のヘッダが付与される
- [ ] アップロード完了時に Set-Cookie `__Host-sync_raw_until` が出る

## Phase 4: フロントエンド (HTMX)

### 成果物

```
internal/ui/
├── templates/
│   ├── base.gohtml
│   ├── partials/
│   │   ├── header.gohtml
│   │   ├── sidebar.gohtml
│   │   ├── file_row.gohtml
│   │   ├── conflict_modal.gohtml           # OCC 409 用
│   │   ├── notification.gohtml
│   │   └── empty_state.gohtml
│   ├── pages/
│   │   ├── home.gohtml
│   │   ├── search.gohtml
│   │   ├── trash.gohtml
│   │   ├── activity.gohtml
│   │   ├── settings.gohtml
│   │   └── share.gohtml                    # 未認証公開リンク
│   └── auth/
│       ├── login.gohtml
│       ├── signup.gohtml
│       ├── totp_setup.gohtml
│       └── recovery.gohtml
├── static/
│   ├── css/
│   │   ├── tokens.css                      # OKLCH デザイントークン
│   │   ├── reset.css
│   │   ├── layout.css
│   │   └── components.css
│   ├── js/
│   │   ├── htmx.min.js                     # HTMX 4.x
│   │   ├── htmx-ext-sse.js
│   │   └── app.js                          # 競合モーダル動的描画 + 進捗バーのみ (50KB 以下)
│   └── icons/                              # SVG インライン用
└── components.go                           # html/template の Funcs / Render ヘルパ
```

### 受け入れ基準

- [ ] サインアップ → ログイン → アップロード → 一覧 → 削除 → 復元 が手動で操作できる
- [ ] CSP nonce ありで JS が動く（unsafe-inline なし）
- [ ] axe-core で Critical / Serious 違反ゼロ
- [ ] JS 総量 gzip 後 50KB 以下、CSS 20KB 以下
- [ ] ダークモード切替（prefers-color-scheme）

## Phase 5: テスト

### 成果物

```
tests/
├── unit/                                   # internal/* と並置でも可、まず CI で分離
│   └── (各パッケージの _test.go)
├── integration/
│   ├── db_test.go                          # testcontainers-go: mysql Primary+Replica
│   ├── upload_test.go                      # OCC 7 分岐 + コンフリクトコピー
│   ├── delete_test.go                      # ソフト削除 + 物理削除バッチ
│   ├── share_test.go                       # token 検証 + Primary 必須
│   └── helpers.go
└── e2e/
    ├── playwright.config.ts
    ├── tests/
    │   ├── auth.spec.ts
    │   ├── upload.spec.ts
    │   ├── conflict.spec.ts                # 409 モーダル
    │   ├── trash.spec.ts
    │   ├── share.spec.ts
    │   └── activity.spec.ts
    └── fixtures/                           # 小ファイル / 中ファイル
```

### 受け入れ基準

- [ ] 単体カバレッジ >= 80%
- [ ] testcontainers-go で mysql 起動 → 統合テスト全 green（ローカル CI）
- [ ] Playwright E2E（`docker compose up` 後）で主要シナリオ green
- [ ] DBRouter テスト（Reader/Writer/RAW/縮退）が必須項目として組み込まれる

## Phase 6: Terraform IaC

### 成果物

```
deploy/terraform/
├── modules/
│   ├── network/                            # VPC, subnet, IGW, SG, VPC endpoints
│   ├── ecs/                                # cluster, service, task definition (3 コンテナ)
│   ├── rds/                                # MySQL Primary + Replica + parameter group
│   ├── s3files/                            # S3 backend bucket + lifecycle (S3 Files 自体は CLI)
│   ├── ecr/                                # 2 リポジトリ (app, nginx)
│   ├── secrets/                            # 6 シークレット
│   └── observability/                      # CloudWatch logs / alarms
└── envs/
    ├── dev/
    │   ├── main.tf, variables.tf, backend.tf, terraform.tfvars.example
    └── prod/
        ├── main.tf, variables.tf, backend.tf, terraform.tfvars.example
```

### 受け入れ基準

- [ ] `terraform plan` がエラーなし
- [ ] 必要なリソース：VPC, ECS Service (max_capacity=1), RDS Primary+Replica, S3 backend bucket, ECR x2, Secrets x6, CloudWatch
- [ ] S3 Files の作成は CLI 手順を README に明記（Terraform AWS provider 未対応分）
- [ ] Cloudflare Tunnel トークンは Secrets Manager から取得

## ステージング

```
P1 → コミット「Phase 1: スケルトン」
P2 → コミット「Phase 2: ドメイン・永続化」
P3 → コミット「Phase 3: HTTP 層」
P4 → コミット「Phase 4: フロントエンド」
P5 → コミット「Phase 5: テスト」
P6 → コミット「Phase 6: Terraform IaC」
```

各 Phase 完了時に `make test && make docker-build` が green である必要がある。

## リスクと対処

| リスク | 影響 | 対処 |
|---|---|---|
| Tink streaming AEAD の Distroless 互換性 | 暗号化が動かない | Phase 2 早期にラウンドトリップテストを実施。問題があれば age に切替（ADR-005 で許容） |
| testcontainers-go と arm64 の相性 | 統合テストが落ちる | mysql:8.0 公式イメージは arm64 サポート済み。CI で確認 |
| S3 Files の Terraform 未対応 | IaC が部分手動 | CLI 手順を `deploy/scripts/setup-s3files.sh` にスクリプト化 |
| Cloudflare Tunnel の 2GB アップロード | 公開リンクの大容量 DL が止まる | Phase 6 後の本番テストで実測。Free 制限に引っかかれば Tunnel 設定見直し |
| HTMX 4.x の SSE 仕様変更 | 通知が動かない | 公式ドキュメント参照、必要なら 2.x にダウングレード（ADR 追加） |

## 「やらない」こと

- v1 では：マルチテナント、E2E 暗号化（ブラウザ側）、ネイティブクライアント、メールリセット、クロスリージョン DR、AutoScale 解禁、全文検索
- 詳細は `00-overview.md §3` および `12-roadmap.md`

## 完了の定義

- [ ] 全 Phase の受け入れ基準が満たされる
- [ ] CI（lint + test + docker-build + e2e local）が main で green
- [ ] `terraform apply` でリリース可能な状態
- [ ] 設計書 `docs/` がコードと不整合になっていない（リネームや列追加があれば反映）
- [ ] codex に最終レビューさせ GO 判定

## 進め方の方針

- Phase 内の細部は実装中に判断する（ADR が必要なら追加）
- Phase ごとに小さくコミット、何かが壊れたら直前の Phase コミットに戻る
- ローカル Docker で動く状態を常に維持（Phase 3 までで `docker compose up` がエラーなく上がる）
- Terraform は最後（Phase 6）。それまでは AWS リソース不要
