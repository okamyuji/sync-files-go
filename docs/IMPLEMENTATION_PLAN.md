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
| **P2** | コアドメイン・永続化層 + 単体テスト | 03 / 04 / 06 / 07 §4 / 11 | internal/domain, internal/repo (DBRouter), internal/storage, internal/crypto + 各 _test.go |
| **P3** | HTTP 層・ミドルウェア + 統合テスト | 02 §3.4 / 04 / 05 / 07 / 11 | internal/http (middleware + handlers), cmd/server, tests/integration |
| **P4** | バッチ・運用 CLI・補正ジョブ | 03 §6.3 / 05 §7 / 10 §4.3 / ADR-009 | cmd/batch (gc / prune-old-versions / reconcile), cmd/sync-files-admin |
| **P5** | フロントエンド (HTMX) + axe + Playwright E2E | 08 / 11 §4 | internal/ui/templates, internal/ui/static, tests/e2e |
| **P6** | Terraform IaC + EventBridge + 本番疎通テスト | 09 / 10 / 11 §12.2 | deploy/terraform/{modules,envs}, スモーク + 実ブラウザ E2E ゲート |

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

## Phase 2: コアドメイン・永続化層 + 単体テスト

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

- [ ] DBRouter が Writer / Reader / RAW window / 縮退運転 の 4 ケースを正しく分岐（単体テスト、in-memory DB stub）
- [ ] File 状態遷移 (draft → active → trashed → purged → gone) が enum + 関数で表現される（単体テスト）
- [ ] Tink Streaming AEAD で 1MB / 100MB のラウンドトリップが通る（純関数テスト、2GB は Phase 5 の E2E に回す）
- [ ] OCC (`If-Match`) ロジックが §04 §4.2 の 7 分岐を網羅（単体テスト、in-memory repo を使う）
- [ ] 単体カバレッジ >= 80%（go test -cover、CI で「下げない」をチェック）

**注**: `active_marker` UNIQUE のように **実 MySQL 接続が必要な確認** は Phase 2 では行わず、Phase 3 の統合テスト（testcontainers-go）で検証する（責務分離）。

## Phase 3: HTTP 層・ミドルウェア + 統合テスト

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

tests/integration/                          # testcontainers-go ベース
├── helpers.go                              # mysql Primary+Replica 起動
├── schema_test.go                          # active_marker UNIQUE が同名 active 二重 INSERT を拒否することを検証（Phase 2 から移動）
├── upload_test.go                          # OCC 7 分岐 + コンフリクトコピー
├── delete_test.go                          # ソフト削除 + ゴミ箱経由 + 即時 purge は不可
├── share_test.go                           # token 検証 + Primary 必須
├── raw_window_test.go                      # __Host-sync_raw_until cookie の HTTP 跨ぎ伝播
└── auth_test.go                            # session, TOTP, lockout
```

### 受け入れ基準

- [ ] 共通ガード ([5/file-operations §0]) を全エンドポイントが通過
- [ ] OCC：If-Match なし → 428、不一致 → 409 Conflict + JSON
- [ ] 公開リンク取得は Primary 強制（DBRouter の Replica routing と分けて検証）
- [ ] CSP nonce / HSTS / X-Content-Type-Options 等のヘッダが付与される
- [ ] アップロード完了時に Set-Cookie `__Host-sync_raw_until` が出る
- [ ] **Phase 3 内で統合テストが green**（testcontainers-go の MySQL 8.0 Primary + Replica）
- [ ] アップロード→削除→復元のラウンドトリップ統合テストで `versions/{file_uuid}/{version_uuid}` キーが残ること

## Phase 4: バッチ・運用 CLI・補正ジョブ

設計書（03 §6.3、05 §7、10 §4.3、ADR-009）で言及されている **HTTP に出さない** 機能群を実装する。Phase 5（フロントエンド）の前に行うのは、これらが運用の安全機構であり、後回しにしてリリース直前に詰まると事故になるため。

### 成果物

```
cmd/
├── batch/
│   └── main.go                             # サブコマンド分岐: gc / prune-old-versions / reconcile
└── sync-files-admin/
    └── main.go                             # 運用 CLI（ADR-009 含む）

internal/batch/
├── garbage_collector.go                    # state='trashed' AND deleted_at < now()-30d を物理削除
├── prune_old_versions.go                   # CR-5 対応：90 日経過 + 非 current の file_versions を削除
├── reconcile.go                            # 補正ジョブ: orphan ファイル / メタデータ片側欠落の検出と隔離
├── upload_session_cleanup.go               # tmp/ の 7 日 TTL 掃除
└── metrics.go                              # CloudWatch EMF メトリクス

internal/admin/                             # 運用 CLI が呼ぶサブコマンド実装
├── restore_file.go                         # 過去版から復元
├── restore_purged.go                       # purged 状態からの復元（運用ツール）
├── reconcile_orphans.go                    # 補正ジョブの dry-run / 実行
├── export_user_data.go
├── rotate_aes_key.go                       # KEK 再ラップ（マスタ鍵ローテーション）
└── force_purge.go                          # ADR-009: INV-1 例外。UI からは到達不能、CLI のみ
```

### 受け入れ基準

- [ ] `cmd/batch gc` が trashed > 30 日のファイルを物理削除し、対応する `versions/{file_uuid}/{version_uuid}` を全削除する
- [ ] `cmd/batch prune-old-versions` が 90 日経過＋非 current の file_versions を削除する。current や `deleted_by_user=1` には触れない（テストで検証）
- [ ] `cmd/batch reconcile --dry-run` が orphan ファイルと孤児メタデータを正しく検出する
- [ ] `cmd/sync-files-admin force-purge` が確認プロンプト + パスワード再入力なしには実行されない
- [ ] HTTP ルータに `force-purge` を含む admin エンドポイントが **存在しない** ことをテストで検証（アンチパターンテスト）
- [ ] 各バッチコマンドは exit code を持ち、CloudWatch アラート用にメトリクスを EMF 形式で stdout に出力
- [ ] バッチの統合テストで「時計を 31 日 / 91 日進める」シナリオが通る（`CLOCK_OFFSET_SECONDS` 利用）

## Phase 5: フロントエンド (HTMX) + axe + Playwright E2E

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

UI 操作の検証はすべて Playwright E2E スクリプトで自動化する（「手動で操作できる」では受け入れない）：

```
tests/e2e/
├── playwright.config.ts
├── tests/
│   ├── auth.spec.ts                        # signup / login / TOTP / recovery
│   ├── upload.spec.ts                      # 単純 + tus.io レジューム
│   ├── conflict.spec.ts                    # 409 + 選択モーダル + 各オプション
│   ├── versions.spec.ts                    # バージョン一覧 + 過去版復元
│   ├── trash.spec.ts                       # 削除 → ゴミ箱 → 復元 → 31 日後 purge
│   ├── share.spec.ts                       # 期限・パスワード・取り消し
│   ├── share_links.spec.ts                 # 発行済み公開リンク管理画面
│   ├── activity.spec.ts                    # アクティビティタイムライン
│   ├── search.spec.ts                      # ファイル名 / タグ検索
│   ├── accessibility.spec.ts               # axe-core 全画面スキャン
│   └── theme.spec.ts                       # light / dark
└── fixtures/
```

- [ ] 上記 11 spec が `docker compose up` 起動の環境で green
- [ ] axe-core で Critical / Serious 違反ゼロ（各ページで実行）
- [ ] CSP nonce ありで JS が動く（unsafe-inline なし）
- [ ] JS 総量 gzip 後 50KB 以下、CSS 20KB 以下
- [ ] ダークモード切替（prefers-color-scheme）

## Phase 6: Terraform IaC + EventBridge + 本番疎通テスト

### 成果物

```
deploy/terraform/
├── modules/
│   ├── network/                            # VPC, subnet (Public 1〜2 AZ), IGW, SG, VPC endpoints
│   ├── ecs/                                # cluster, service (max_capacity=1), task definition (3 コンテナ)
│   ├── rds/                                # MySQL 8.0 Primary Multi-AZ + Read Replica × 1 + parameter group
│   ├── s3files/                            # S3 backend bucket + lifecycle (DeleteMarker 配下のみ noncurrent expire)
│   ├── ecr/                                # 2 リポジトリ (app, nginx) + lifecycle policy
│   ├── secrets/                            # 6 シークレット (db_password, aes_master, totp_hmac, csrf, session, cloudflared_token)
│   ├── batch/                              # ECS Scheduled Task + EventBridge Schedule (gc / prune / reconcile)
│   │   ├── eventbridge.tf                  # cron(0 18 * * ? *) で gc, cron(0 19 ...) で prune
│   │   ├── ecs_runtask.tf                  # RunTask permission, IAM
│   │   └── alarms.tf                       # 「24h バッチ実行 0」アラート
│   └── observability/                      # CloudWatch logs / alarms / SNS
└── envs/
    ├── dev/
    │   ├── main.tf, variables.tf, backend.tf, terraform.tfvars.example
    └── prod/
        ├── main.tf, variables.tf, backend.tf, terraform.tfvars.example

deploy/scripts/
├── setup-s3files.sh                        # S3 Files file system + access point の CLI 作成
└── seed-secrets.sh                         # 初期シークレット投入

tests/e2e/release/                          # 本番/staging への terraform apply 後に走らせる E2E
├── smoke.spec.ts                           # /healthz, /readyz, ログイン
└── critical-path.spec.ts                   # 損失防止クリティカルシナリオ（OCC、削除→復元、公開リンク）
```

### 受け入れ基準

- [ ] `terraform plan` がエラーなし、必要なリソース（VPC, ECS Service max_capacity=1, RDS Primary+Replica, S3 backend bucket, ECR x2, Secrets x6, CloudWatch, EventBridge × 3 ジョブ）が出力される
- [ ] EventBridge Schedule で `gc` (ゴミ箱物理削除)、`prune-old-versions`、`reconcile` の 3 ジョブが日次起動する設定
- [ ] S3 Files の作成は `deploy/scripts/setup-s3files.sh` を README に明記（Terraform AWS provider 未対応分）
- [ ] Cloudflare Tunnel トークンは Secrets Manager から取得
- [ ] **staging で `terraform apply` を実行し、`tests/e2e/release` の smoke と critical-path が Playwright headed で green**（自動リリースゲート）
- [ ] **加えて、人間オペレータが実ブラウザ（Chrome / Safari / Mobile Safari の 3 ブラウザ）で `11 §12.2` の必須シナリオを手動実行し、UX を確認する**（手動リリースゲート、必須）
- [ ] 上記 2 つが両方 green で初めて prod へ apply 可

## ステージング

```
P1 → コミット「Phase 1: スケルトン」
P2 → コミット「Phase 2: ドメイン・永続化 + 単体テスト」
P3 → コミット「Phase 3: HTTP 層 + 統合テスト」
P4 → コミット「Phase 4: バッチ・運用 CLI・補正ジョブ」
P5 → コミット「Phase 5: フロントエンド + Playwright E2E」
P6 → コミット「Phase 6: Terraform + EventBridge + 本番疎通」
```

各 Phase 完了時に `make test && make docker-build` が green である必要がある（P1 は `make build` のみで可）。

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
- v2 候補（v1 では実装しない運用面）：
  - `gone` 状態への自動 finalization バッチ（v1 は purged で止める。`05 §7.3`）
  - ステータスページ
  - OS ネイティブ自動同期クライアント
  - Email パスワードリセット（SES 本番ドメイン認証完了後）

逆に **v1 に含む**（運用面のため見落としがち）：
- `cmd/sync-files-admin` 運用 CLI（restore / reconcile / export / rotate / force-purge / prune）
- `cmd/batch` バッチ（gc / prune-old-versions / reconcile / upload_session_cleanup）
- EventBridge Schedule で日次起動
- 補正ジョブ（DB / S3 divergence の検出と隔離）

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
