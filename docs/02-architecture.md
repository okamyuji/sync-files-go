# 02. アーキテクチャ

## 1. アーキテクチャ原則

| 原則 | 意味 |
|---|---|
| **Boring is best** | 派手な分散システムは作らない。Postgres と S3 という枯れた基盤に乗る |
| **失敗は静かに、訂正は明示的に** | エラーは握りつぶさず構造化ログに残す。一方で UI はユーザの「明示的な操作」なしに状態を変えない |
| **境界は HTTP** | バックエンドの内部構造は変わっても、HTTP API（HTML 応答含む）の互換性は維持する |
| **データは所有者を 1 箇所に** | ファイル本体は S3 Files、メタデータは RDS。両者は API を通じてのみ協調する |
| **可逆性 > 最適化** | 性能を上げるための最適化が「壊れたら復旧不能」を意味する場合、その最適化は採用しない |

## 2. 全体構成図（C4: Container View）

```
┌──────────────────────────── ユーザ ──────────────────────────────┐
│                                                                  │
│     [Chrome]    [Edge]    [Safari]    [Mobile Safari]            │
│       │           │          │             │                     │
│       └───────────┴──────────┴─────────────┘                     │
│                          │                                       │
│                  HTTPS (TLS 1.3)                                 │
└──────────────────────────│───────────────────────────────────────┘
                           ▼
┌──────────────────────── AWS ap-northeast-1 ────────────────────────┐
│                                                                    │
│  [Route 53]                                                        │
│       │                                                            │
│       ▼                                                            │
│  [ACM (TLS 証明書)]                                                │
│       │                                                            │
│       ▼                                                            │
│  [ALB (Application Load Balancer)]   public subnet                 │
│       │ HTTP/2 (内部)                                              │
│       │                                                            │
│       ▼                                                            │
│  ┌──────────────────────────────────────────────────────────────┐  │
│  │ ECS Fargate Service                            private subnet │  │
│  │  ┌────────────────────────────────────────────────────────┐  │  │
│  │  │ Task (1〜3 タスク)                                      │  │  │
│  │  │  ┌────────────────────────────────────────────────┐    │  │  │
│  │  │  │ Container: sync-files-go (Go アプリ)            │    │  │  │
│  │  │  │  ├── HTTP server (net/http)                     │    │  │  │
│  │  │  │  ├── HTML テンプレート (html/template)          │    │  │  │
│  │  │  │  ├── HTMX を返す                                │    │  │  │
│  │  │  │  └── slog → stdout (JSON)                       │    │  │  │
│  │  │  └────────────────────────────────────────────────┘    │  │  │
│  │  │  Mount points:                                          │  │  │
│  │  │   /var/data → S3 Files (s3files_volume_configuration)   │  │  │
│  │  └────────────────────────────────────────────────────────┘  │  │
│  └──────────────────────────────────────────────────────────────┘  │
│         │                          │                  │            │
│         ▼                          ▼                  ▼            │
│  [RDS for PostgreSQL]    [S3 Files]            [Secrets Manager]   │
│   - メタデータ            - ファイル本体        - DB パスワード    │
│   - 監査ログ              - バージョニング ON   - アプリ AES 鍵    │
│   - セッション            - Object Lock(候補)   - TOTP HMAC 鍵     │
│   - Multi-AZ              - SSE-S3 既定                            │
│                                                                    │
│  [CloudWatch Logs]   [CloudWatch Alarms]   [SNS (アラート通知)]    │
│                                                                    │
└────────────────────────────────────────────────────────────────────┘
```

## 3. コンポーネント詳細

### 3.1 ALB

- 受信: HTTPS (443) のみ。HTTP (80) は HTTPS リダイレクト
- WAF 連携: AWS Managed Rule（Bot Control / SQLi / XSS）。コストとリターンを見て v2 で決定
- Sticky session: 不要（ECS タスクはステートレス。セッションは DB）
- Health check: `/healthz` を 30 秒ごと

### 3.2 ECS Fargate

- 起動タイプ: Fargate
- タスク定義のリソース: 0.5 vCPU / 1024 MB （v1）
- 同時タスク数: desiredCount = 1（通常）。AutoScaling で CPU 70% 超えたら 3 まで
- ヘルスチェック: `/healthz`（liveness）と `/readyz`（readiness）を分ける
- ボリューム: `s3files_volume_configuration` で S3 Files をマウント（[`09-infrastructure-and-deployment.md`](./09-infrastructure-and-deployment.md) §4 参照）
- Secrets Manager 連携: タスク実行ロールが `secretsmanager:GetSecretValue` 権限を持つ

### 3.3 Go アプリケーション

```
sync-files-go/
├── cmd/
│   └── server/                        # main.go
│       └── main.go
├── internal/
│   ├── http/                          # HTTP ハンドラ・ミドルウェア
│   │   ├── middleware/
│   │   │   ├── auth.go                # セッション検証
│   │   │   ├── csrf.go                # double-submit cookie
│   │   │   ├── ratelimit.go           # トークンバケット
│   │   │   ├── logging.go
│   │   │   └── recovery.go
│   │   ├── handler_files.go
│   │   ├── handler_upload.go          # tus.io 互換
│   │   ├── handler_share.go
│   │   ├── handler_auth.go
│   │   ├── handler_sse.go
│   │   └── router.go
│   ├── domain/                        # ピュアな業務ロジック
│   │   ├── file.go
│   │   ├── version.go
│   │   ├── trash.go
│   │   ├── share_link.go
│   │   ├── audit.go
│   │   └── user.go
│   ├── repo/                          # 永続化層（インターフェース + 実装）
│   │   ├── repo.go                    # interface
│   │   ├── postgres/
│   │   │   ├── files.go
│   │   │   ├── users.go
│   │   │   ├── audit.go
│   │   │   └── migrations/
│   │   └── memory/                    # テスト用
│   ├── storage/                       # S3 Files / S3 SDK 抽象
│   │   ├── storage.go                 # interface
│   │   ├── s3files/                   # NFS マウント実装（os パッケージ）
│   │   └── memory/                    # テスト用
│   ├── crypto/                        # AES-GCM ヘルパー、TOTP、Argon2 ラッパ
│   │   ├── aesgcm.go
│   │   ├── totp.go
│   │   └── password.go
│   ├── sync/                          # OCC / コンフリクト / 物理削除バッチ
│   │   ├── occ.go
│   │   ├── conflict.go
│   │   └── garbage_collector.go
│   ├── ui/                            # HTML テンプレート + 静的アセット
│   │   ├── templates/                 # html/template
│   │   ├── static/                    # CSS / JS / アイコン
│   │   └── components.go              # 部分テンプレートのレンダラ
│   ├── config/
│   │   └── config.go                  # 環境変数読み込み
│   └── observability/
│       ├── log.go                     # slog 設定
│       └── metrics.go
├── migrations/                        # リポジトリ層 SQL マイグレーション
├── deploy/
│   ├── Dockerfile
│   ├── docker-compose.yml             # ローカル開発用
│   └── terraform/                     # AWS インフラ定義
└── tests/
    ├── e2e/                           # Playwright
    └── integration/                   # Go 統合テスト
```

### 3.4 RDS for PostgreSQL

- インスタンスクラス: db.t4g.micro (v1) / db.t4g.small (将来)
- ストレージ: gp3 20GB から開始、自動拡張上限 100GB
- マルチ AZ: 有効
- 自動バックアップ: 30 日保持
- パラメータグループ: デフォルト + `max_connections = 100`
- 接続: `database/sql` + `pgx` ドライバ（標準ライブラリ志向だが、`lib/pq` は本家がメンテナンスモードのため `pgx` を採用 — [ADR-002](./adr/ADR-002-rds-postgres-not-self-hosted.md)）
- 接続プール: `*sql.DB` の `SetMaxOpenConns(20)`、`SetMaxIdleConns(5)`、`SetConnMaxLifetime(5 * time.Minute)`

### 3.5 S3 Files

- 内部実装: EFS バックエンド（AWS により提供、ユーザは EFS 自体を意識しない）
- マウント先: `/var/data`
- ディレクトリ構造（[`03-domain-model.md`](./03-domain-model.md) §5 参照）:
  ```
  /var/data/
    ├── owner-{user_id}/
    │   ├── current/
    │   │   ├── {file_uuid}            # 現行版の本体（暗号文）
    │   │   └── ...
    │   ├── trash/
    │   │   └── {file_uuid}            # ソフト削除中
    │   ├── tmp/
    │   │   └── {upload_uuid}.part     # アップロード中
    │   └── versions/
    │       └── {file_uuid}/
    │           ├── v1
    │           ├── v2
    │           └── ...
    └── public/
        └── (将来用)
  ```
- バックエンド S3 バケットは別途バージョニング ON、SSE-S3 既定有効、ライフサイクル設定（90 日後の旧版完全消去）

### 3.6 Secrets Manager

| シークレット名 | 用途 |
|---|---|
| `sync-files-go/db/password` | RDS 接続パスワード |
| `sync-files-go/aes/master-key` | アプリ層 AES-256-GCM のマスタ鍵（鍵階層は [`07-security.md`](./07-security.md) §4） |
| `sync-files-go/totp/hmac-key` | TOTP の HMAC 鍵（共通秘密鍵の暗号化用） |
| `sync-files-go/csrf/key` | CSRF Cookie 署名用 HMAC 鍵 |
| `sync-files-go/session/key` | セッション ID 署名用 HMAC 鍵 |

### 3.7 CloudWatch

- ロググループ: `/ecs/sync-files-go/app`
- メトリクス: `Uploads`, `UploadFailures`, `ConflictDetected`, `TrashRestored`, `PhysicalDeletes`, `SSEConnections`
- アラート: ECS タスクが落ちたら SNS 通知 → メール

## 4. 主要データフロー

### 4.1 アップロード（5MB 以上、tus.io 互換）

```
[Client]                  [ALB]            [ECS Fargate App]                  [S3 Files]                [RDS]
   │                         │                       │                              │                     │
   │ POST /uploads           │                       │                              │                     │
   │ (Tus-Resumable: 1.0.0)  │                       │                              │                     │
   │ (Upload-Length: 12345)  │                       │                              │                     │
   ├────────────────────────>│ ────────────────────> │                              │                     │
   │                         │                       │ INSERT upload_session         │                     │
   │                         │                       │──────────────────────────────────────────────────> │
   │                         │                       │ Create /var/data/.../tmp/{upload_uuid}.part │     │
   │                         │                       │─────────────────────────────>│                     │
   │ 201 Created             │                       │ <─────────────────────────────│                     │
   │ Location: /uploads/{id} │                       │                              │                     │
   │<────────────────────────│<──────────────────────│                              │                     │
   │                         │                       │                              │                     │
   │ HEAD /uploads/{id}      │                       │                              │                     │
   ├────────────────────────>│                       │                              │                     │
   │ Upload-Offset: 0        │                       │                              │                     │
   │<────────────────────────│                       │                              │                     │
   │                         │                       │                              │                     │
   │ PATCH /uploads/{id}     │                       │                              │                     │
   │ Upload-Offset: 0        │                       │                              │                     │
   │ <body chunk 1>          │                       │ append to .part              │                     │
   ├────────────────────────>│ ────────────────────> │─────────────────────────────>│                     │
   │ 204 No Content          │                       │                              │                     │
   │ Upload-Offset: 4096     │                       │                              │                     │
   │<────────────────────────│                       │                              │                     │
   │  ...(繰り返し)...                                                                                      │
   │                         │                       │                              │                     │
   │ PATCH 最後              │                       │                              │                     │
   ├────────────────────────>│ ────────────────────> │ verify SHA-256 / encrypt / fsync                    │
   │                         │                       │ os.Rename → /var/data/.../current/{file_uuid}       │
   │                         │                       │ INSERT files (id, path, version_id, sha256, ...)    │
   │                         │                       │──────────────────────────────────────────────────> │
   │                         │                       │ INSERT audit_log                                    │
   │                         │                       │──────────────────────────────────────────────────> │
   │ 204 No Content          │                       │                              │                     │
   │ X-Sync-File-Id: <uuid>  │                       │                              │                     │
   │ ETag: <version_id>      │                       │                              │                     │
   │<────────────────────────│                       │                              │                     │
```

### 4.2 ダウンロード

```
[Client] ──GET /files/{id}── [App] ──SELECT files── [RDS] ──[App] ──open /var/data/.../current/{uuid}── [S3 Files]
                                                              │
                                                              ▼
                                                       AES-GCM decrypt → response stream
```

ストリーム転送のため、メモリに全量を載せない（`io.Copy` でバッファリング）。

### 4.3 上書き（OCC 成功）

```
[Client]                   [App]                          [RDS]                            [S3 Files]
   │                         │                              │                                  │
   │ PUT /files/{id}         │                              │                                  │
   │ If-Match: <ver_id>      │                              │                                  │
   │ <body>                  │                              │                                  │
   ├────────────────────────>│ SELECT current version_id    │                                  │
   │                         │─────────────────────────────>│                                  │
   │                         │                              │                                  │
   │                         │ if matches:                  │                                  │
   │                         │   write to tmp/, fsync,      │                                  │
   │                         │   atomic rename to versions/ │                                  │
   │                         │─────────────────────────────────────────────────────────────────│
   │                         │   atomic rename current      │                                  │
   │                         │─────────────────────────────────────────────────────────────────│
   │                         │   INSERT file_versions       │                                  │
   │                         │   UPDATE files               │                                  │
   │                         │─────────────────────────────>│                                  │
   │ 204 + ETag: <new_ver>   │                              │                                  │
   │<────────────────────────│                              │                                  │
```

### 4.4 上書き（OCC 衝突）

```
[Client] ──PUT If-Match: <stale>──> [App] ──SELECT── [RDS]
                                       │
                                       ▼
                                  if mismatch:
                                  HTTP 409 Conflict
                                  body = JSON {
                                    "current_version_id": "...",
                                    "current_modified_at": "...",
                                    "current_modified_by": "...",
                                    "options": ["view", "save_as_copy", "force_overwrite"]
                                  }
                                       │
                                       ▼
                                 [Client] HTMX swap → 選択モーダル
```

### 4.5 ソフト削除

```
[Client] ──DELETE /files/{id}── [App] ──UPDATE files SET deleted_at = now()── [RDS]
                                  │
                                  └─> S3 Files は何もしない（INV-1）
```

### 4.6 物理削除（バッチ・ECS Scheduled Task）

```
[EventBridge cron(0 3 * * ? *)] ──RunTask── [ECS gc task] ──SELECT * FROM files WHERE deleted_at < now() - 30d── [RDS]
                                                                │
                                                                ▼
                                                      for each file:
                                                       1. mv /var/data/.../current/{uuid} → /var/data/.../trash-purged/
                                                          (S3 バージョニングが旧版を保持)
                                                       2. UPDATE files SET state = 'purged'
                                                       3. INSERT audit_log (irreversible=true)
```

## 5. ステートとイベント

「ファイル」のライフサイクル（状態遷移）：

```
                       upload
                         │
                         ▼
                  ┌─────────────┐
                  │   draft     │  ← tmp/ に書き込み中
                  └──────┬──────┘
                         │ complete
                         ▼
                  ┌─────────────┐    upload(If-Match ok)
                  │   active    │ ──────────────────────► active (新版)
                  └──────┬──────┘
                         │ delete
                         ▼
                  ┌─────────────┐
                  │   trashed   │  ← deleted_at 設定済み
                  └──────┬──────┘
                         │ 30d 経過 or 明示空にする
                         ▼
                  ┌─────────────┐
                  │   purged    │  ← S3 DeleteMarker 付与済み
                  └──────┬──────┘
                         │ 90d 経過（S3 ライフサイクル）
                         ▼
                  ┌─────────────┐
                  │   gone      │  ← 完全消去（復元不能）
                  └─────────────┘

  trashed → active への戻し（restore）はユーザ操作で可能
  purged → active への戻しは技術的には可能（S3 旧版から）。UI からは不可、運用ツールのみ
  gone → 復元不能
```

## 6. デプロイメントトポロジ

### 6.1 環境

| 環境 | リージョン | 用途 |
|---|---|---|
| local | docker compose | 開発者ローカル |
| dev | ap-northeast-1 | 本番デプロイ前の検証（任意） |
| prod | ap-northeast-1 | 本番 |

v1 では `local` と `prod` のみ運用、`dev` はオプション。

### 6.2 ネットワーク

- VPC: `10.0.0.0/16`
- public subnet: ALB のみ配置
- private subnet: ECS / RDS / S3 Files VPC エンドポイント
- NAT Gateway: なし（VPC エンドポイント経由で AWS API にアクセス）
- IGW: ALB 用

詳細は [`09-infrastructure-and-deployment.md`](./09-infrastructure-and-deployment.md)。

## 7. 単一障害点（SPOF）と多重化

| コンポーネント | 多重化 | 理由 |
|---|---|---|
| ALB | AZ 冗長（標準） | AWS 管理 |
| ECS Fargate | desiredCount = 1〜3 / Multi-AZ | 個人専用なので 1 が常態。AutoScaling は急増対応 |
| RDS | Multi-AZ standby | 自動フェイルオーバ |
| S3 Files | AWS が冗長化 | 同一リージョン内で多重化 |
| Secrets Manager | AWS マネージド | リージョン冗長 |
| **VPC エンドポイント** | 複数 AZ に作成 | これを忘れると AZ 障害でアプリが全滅 |

## 8. 設計の言質（明示的に約束しないこと）

- **マルチリージョン障害には対応しない**：ap-northeast-1 のリージョン全体障害は復旧優先度を下げる（個人用）
- **完全な無停止デプロイは目標としない**：Rolling Update で短時間の同時 2 タスクを許容
- **強整合な分散合意は採らない**：Postgres を単一書き込み点とすることで合意問題を回避

---

次の章: [`03-domain-model.md`](./03-domain-model.md)
