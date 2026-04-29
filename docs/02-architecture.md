# 02. アーキテクチャ

## 1. アーキテクチャ原則

| 原則 | 意味 |
|---|---|
| **Boring is best** | 派手な分散システムは作らない。MySQL Primary + Read Replica と S3 Files という枯れた基盤に乗る |
| **失敗は静かに、訂正は明示的に** | エラーは握りつぶさず構造化ログに残す。一方で UI はユーザの「明示的な操作」なしに状態を変えない |
| **境界は HTTP** | バックエンドの内部構造は変わっても、HTTP API（HTML 応答含む）の互換性は維持する |
| **データは所有者を 1 箇所に** | ファイル本体は S3 Files、メタデータは RDS MySQL Primary。両者は API を通じてのみ協調する |
| **Primary を守る** | 書き込みと read-after-write は Primary に集約、それ以外の read は Replica に逃がす（[ADR-008](./adr/ADR-008-mysql-read-replica-write-ahead.md)） |
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
                ┌──────────────────────┐
                │   Cloudflare Edge    │
                │  - DNS               │
                │  - TLS 終端          │
                │  - DDoS 緩和         │
                └──────────┬───────────┘
                           │ Cloudflare Tunnel (cloudflared が outbound 接続)
                           │
┌──────────────────────── AWS ap-northeast-1 ────────────────────────┐
│                                                                    │
│  VPC (10.0.0.0/16)                                                 │
│                                                                    │
│  ┌──────────────────────────────────────────────────────────────┐  │
│  │ ECS Fargate Task (app-subnet, AZ-a)                           │  │
│  │  ┌──────────┐   ┌────────┐   ┌─────────────────────────────┐  │  │
│  │  │cloudflared│ ─>│ nginx  │ ─>│ Container: sync-files-go     │  │  │
│  │  │(tunnel)   │   │(8443)  │   │  - HTTP server (net/http)    │  │  │
│  │  │           │   │ TLS    │   │  - HTML テンプレート         │  │  │
│  │  └──────────┘   │ 終端   │   │  - DBRouter (Reader/Writer)  │  │  │
│  │       │         │ +rate  │   │  - slog → stdout (JSON)      │  │  │
│  │       │ outbound│ limit  │   └─────────────────────────────┘  │  │
│  │       └─────────┴────────┘                                    │  │
│  │  Mount: /var/data → S3 Files                                  │  │
│  └──────────────────────────────────────────────────────────────┘  │
│         │                          │                                │
│         ▼                          ▼                                │
│  ┌────────────────┐         ┌────────────────────┐                  │
│  │ RDS MySQL      │ binlog  │ S3 Files           │                  │
│  │ Primary        │ ─────►  │ - ファイル本体     │                  │
│  │ (Write+RAW)    │         │ - バージョニング ON│                  │
│  └───────┬────────┘         │ - SSE-S3 既定      │                  │
│          │ replication      └────────────────────┘                  │
│          ▼                                                           │
│  ┌────────────────┐                                                 │
│  │ RDS MySQL      │                                                 │
│  │ Read Replica   │                                                 │
│  │ (Read-heavy)   │                                                 │
│  └────────────────┘                                                 │
│                                                                    │
│  [Secrets Manager]   [CloudWatch]   [VPC Endpoints (ECR/SM/S3)]    │
│                                                                    │
└────────────────────────────────────────────────────────────────────┘
```

決定の根拠は [ADR-007](./adr/ADR-007-cloudflare-tunnel-not-alb.md)（Cloudflare Tunnel）と [ADR-008](./adr/ADR-008-mysql-read-replica-write-ahead.md)（MySQL + Read Replica）。

## 3. コンポーネント詳細

### 3.1 Cloudflare（外部公開層）

- DNS：`sync.example.com` を Cloudflare Hosted Zone で管理
- TLS：Cloudflare が TLS 1.3 で終端、Cloudflare ↔ cloudflared 間はトンネル内で再暗号化
- DDoS：Cloudflare の標準保護
- 課金：Free プラン（個人用）。月額固定の追加費用は **ゼロ**

### 3.2 cloudflared（サイドカー）

- ECS タスク内で動く Tunnel コネクタ
- AWS から Cloudflare へ outbound 接続のみ。Inbound ポートは開けない
- 認証トークンは Secrets Manager から取得
- 設定：`localhost:8443` (nginx) を `sync.example.com` に紐づけ

### 3.3 nginx（サイドカー）

- TLS 終端（cloudflared から受けた接続を 8443 で待ち受け）
- 補助レート制限（`limit_req_zone`）
- 大容量 body の取扱：`client_max_body_size 2200m`
- SSE のために `proxy_buffering off` + `chunked_transfer_encoding off`
- ロギング：JSON 形式で stdout に
- Cloudflare の IP レンジを `set_real_ip_from` で信頼し、`CF-Connecting-IP` を真の IP として使う

### 3.4 Go アプリケーション

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
│   │   │   ├── ratelimit.go           # トークンバケット (DB 由来)
│   │   │   ├── logging.go
│   │   │   └── recovery.go
│   │   ├── handler_files.go
│   │   ├── handler_upload.go          # tus.io 互換
│   │   ├── handler_share.go
│   │   ├── handler_auth.go
│   │   ├── handler_sse.go
│   │   └── router.go
│   ├── domain/
│   │   ├── file.go
│   │   ├── version.go
│   │   ├── trash.go
│   │   ├── share_link.go
│   │   ├── audit.go
│   │   └── user.go
│   ├── repo/
│   │   ├── repo.go                    # Reader / Writer の interface
│   │   ├── dbrouter.go                # Reader/Writer 切替（DBRouter）
│   │   ├── mysql/
│   │   │   ├── files.go
│   │   │   ├── users.go
│   │   │   ├── audit.go
│   │   │   └── migrations/
│   │   └── memory/                    # テスト用
│   ├── storage/
│   │   ├── storage.go                 # interface
│   │   ├── s3files/                   # NFS マウント実装（os パッケージ）
│   │   └── memory/                    # テスト用
│   ├── crypto/
│   │   ├── aesgcm.go
│   │   ├── totp.go
│   │   └── password.go
│   ├── sync/
│   │   ├── occ.go
│   │   ├── conflict.go
│   │   └── garbage_collector.go
│   ├── ui/
│   │   ├── templates/                 # html/template
│   │   ├── static/                    # CSS / JS / アイコン
│   │   └── components.go
│   ├── config/
│   │   └── config.go
│   └── observability/
│       ├── log.go
│       └── metrics.go
├── migrations/                        # SQL（MySQL 8 構文）
├── deploy/
│   ├── docker/
│   │   ├── app.Dockerfile
│   │   ├── nginx.Dockerfile
│   │   ├── nginx.conf
│   │   └── docker-compose.yml         # ローカル開発
│   └── terraform/
└── tests/
    ├── e2e/                           # Playwright
    └── integration/                   # Go 統合テスト
```

### 3.5 DBRouter（読み書き分離）

設計の中心となる小さなコンポーネント：

```go
// internal/repo/dbrouter.go
type DBRouter struct {
    primary         *sql.DB
    replica         *sql.DB
    replicaDegraded atomic.Bool
}

func (r *DBRouter) Writer(ctx context.Context) *sql.DB {
    return r.primary
}

func (r *DBRouter) Reader(ctx context.Context) *sql.DB {
    if r.forcePrimary(ctx) || r.replicaDegraded.Load() {
        return r.primary
    }
    return r.replica
}

// HIGH 修正: ctx だけでは HTTP リクエストを跨いで RAW window を伝播できない。
// Cookie 経由で「次の数秒は Primary 読み」を指示する。
type readAfterWriteUntilKey struct{}

func (r *DBRouter) forcePrimary(ctx context.Context) bool {
    until, ok := ctx.Value(readAfterWriteUntilKey{}).(time.Time)
    return ok && time.Now().Before(until)
}
```

ミドルウェアと書き込み完了処理：

```go
// internal/http/middleware/raw.go
const rawCookieName = "__Host-sync_raw_until"

// 書き込み直後のレスポンスで cookie を発行
func SetRawCookie(w http.ResponseWriter, sessionID uuid.UUID, until time.Time, signKey []byte) {
    payload := fmt.Sprintf("%d", until.Unix())
    sig := hmacSHA256(signKey, sessionID[:], []byte(payload))
    http.SetCookie(w, &http.Cookie{
        Name:     rawCookieName,
        Value:    payload + "." + base64url(sig),
        Path:     "/",
        HttpOnly: true,
        Secure:   true,
        SameSite: http.SameSiteLaxMode,
        Expires:  until.Add(time.Second),
    })
}

// 受信時：cookie を検証して ctx に焼き付ける
func RAWMiddleware(signKey []byte) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            if c, err := r.Cookie(rawCookieName); err == nil {
                if until, ok := verifyRaw(c.Value, currentSession(r).ID, signKey); ok && time.Now().Before(until) {
                    ctx := context.WithValue(r.Context(), readAfterWriteUntilKey{}, until)
                    r = r.WithContext(ctx)
                }
            }
            next.ServeHTTP(w, r)
        })
    }
}
```

これにより、書き込み完了 → リダイレクト先のリクエスト → さらに次の HTMX 部分更新まで、5 秒間は `DBRouter.Reader` が Primary を返す。詳細は [ADR-008](./adr/ADR-008-mysql-read-replica-write-ahead.md)。

### 3.6 RDS for MySQL

| 項目 | 値 |
|---|---|
| エンジン | MySQL 8.0.x |
| Primary | db.t4g.micro (v1) / Multi-AZ |
| Read Replica | db.t4g.micro × 1（v1） |
| ストレージ | gp3 20GB から開始、自動拡張上限 100GB |
| 自動バックアップ | 30 日保持 |
| パラメータグループ | デフォルト + `max_connections=100`、`innodb_buffer_pool_size` を実機で調整 |
| ドライバ | go-sql-driver/mysql + database/sql |
| 接続プール | Primary / Replica それぞれ `SetMaxOpenConns(50) / SetMaxIdleConns(25) / SetConnMaxLifetime(30m)` |

### 3.7 S3 Files

- 内部実装: EFS バックエンド（AWS により提供）
- マウント先: `/var/data`
- ディレクトリ構造（[`03-domain-model.md`](./03-domain-model.md) §5 参照）：
  ```
  /var/data/
    ├── owner-{user_id}/
    │   ├── versions/{file_uuid}/{version_uuid}    ← 全バージョンを immutable に保存（CR-1）
    │   └── tmp/{upload_uuid}.part                  ← アップロード途中
    └── _system/...
  ```
- バックエンド S3 バケット：バージョニング ON、SSE-S3 既定有効、ライフサイクル設定（90 日後の旧版完全消去）

### 3.8 Secrets Manager

| シークレット名 | 用途 |
|---|---|
| `sync-files-go/db/password` | RDS 接続パスワード（Primary・Replica で同一ユーザ） |
| `sync-files-go/aes/master-key` | アプリ層 AES-256-GCM のマスタ鍵 |
| `sync-files-go/totp/hmac-key` | TOTP の HMAC 鍵 |
| `sync-files-go/csrf/key` | CSRF Cookie 署名用 |
| `sync-files-go/session/key` | セッション ID 署名用 |
| `sync-files-go/cloudflared/token` | Cloudflare Tunnel 認証トークン |

### 3.9 CloudWatch

- ロググループ: `/ecs/sync-files-go/{app,nginx,cloudflared}`
- メトリクス: `Uploads`, `UploadFailures`, `ConflictDetected`, `TrashRestored`, `PhysicalDeletes`, `SSEConnections`, `ReplicaLagSeconds`, `DBPoolUtilization`
- アラート: ECS タスクが落ちたら SNS 通知 → メール

## 4. 主要データフロー

### 4.1 アップロード（5MB 以上、tus.io 互換）

```
[Client] ──HTTPS──> [Cloudflare] ──Tunnel──> [cloudflared] ──:8443──> [nginx] ──:8080──> [app]
                                                                                              │
                                                              SELECT FOR UPDATE (Primary) ──> [MySQL Primary]
                                                                                              │
                                                              tmp 書き込み, encrypt, fsync ──> [S3 Files]
                                                                                              │
                                                              os.Rename → versions/{file_uuid}/{version_uuid} ─> [S3 Files]
                                                                                              │
                                                              INSERT file_versions, audit ──> [MySQL Primary]
                                                                                              │
                                                              ETag 返却 (Cloudflare 経由)
```

### 4.2 ダウンロード（最近の自分のアップロードを表示）

```
[Client] ─GET─> [Cloudflare/Tunnel/nginx] ─> [app]
                                              │
                                              if WithReadAfterWrite(ctx) → DBRouter.Reader = Primary
                                              else                          → DBRouter.Reader = Replica
                                              │
                                              SELECT files ────────────> [MySQL]
                                              │
                                              JOIN file_versions ON current_version_id_bin
                                              open(/var/data/.../versions/{file_uuid}/{version_uuid}) ──> [S3 Files]
                                              │
                                              Streaming AEAD decrypt → stream
```

### 4.3 上書き（OCC）

[`04-sync-semantics.md`](./04-sync-semantics.md) §4 参照。Primary に対して `SELECT FOR UPDATE` でレースを防ぐ。

### 4.4 ファイル一覧（Replica）

```
[Client] ─GET / ─> [app] ──DBRouter.Reader=Replica──> [MySQL Replica]
                    SELECT files WHERE owner=? AND state='active' ORDER BY updated_at DESC LIMIT 50
```

平時は Replica。`WithReadAfterWrite` 中なら Primary。

### 4.5 ソフト削除

```
[app] ──Writer──> [MySQL Primary]
       UPDATE files SET state='trashed', deleted_at=now() WHERE id=?
       UPDATE share_links SET revoked_at=now() WHERE file_id=? AND revoked_at IS NULL
       INSERT INTO audit_logs ...
```

S3 Files 上のファイルには手をつけない（INV-1）。

### 4.6 物理削除（バッチ・ECS Scheduled Task）

```
[EventBridge cron(0 18 * * ? *)] ──RunTask──> [ECS gc task] ──Reader=Primary──> [MySQL]
   SELECT * FROM files WHERE state='trashed' AND deleted_at < now() - INTERVAL 30 DAY
   for each:
     for each fv in file_versions WHERE file_id_bin = files.id_bin:
       os.Remove(/var/data/.../versions/{file_uuid}/{fv.id_bin})  # S3 DeleteMarker 付与
     UPDATE files SET state='purged' (Primary)
     INSERT audit_logs (irreversible=true)
```

## 5. ステートとイベント

```
                       upload
                         │
                         ▼
                  ┌─────────────┐
                  │   draft     │
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
                         │ 30d 経過 or 明示「ゴミ箱を空にする」
                         ▼
                  ┌─────────────┐
                  │   purged    │  ← S3 DeleteMarker 付与済み
                  └──────┬──────┘
                         │ 90d 経過（S3 ライフサイクル）
                         ▼
                  ┌─────────────┐
                  │   gone      │
                  └─────────────┘
```

## 6. デプロイメントトポロジ

### 6.1 環境

| 環境 | リージョン | 用途 |
|---|---|---|
| local | docker compose | 開発者ローカル |
| dev | ap-northeast-1 | 検証（任意） |
| prod | ap-northeast-1 | 本番 |

### 6.2 ネットワーク（HIGH 修正：Public/Private を整合）

| 用途 | サブネット種別 | CIDR (例) | 配置 |
|---|---|---|---|
| ECS Fargate タスク | **Public Subnet**（IGW ルート） | 10.0.10.0/24 (AZ-a), 10.0.11.0/24 (AZ-c) | タスクには `assign_public_ip = true`。Inbound は SG で全 deny。Outbound のみ IGW 経由で Cloudflare へ |
| RDS Primary / Standby / Read Replica | Private Subnet（IGW ルートなし） | 10.0.20.0/24 (AZ-a), 10.0.21.0/24 (AZ-c) | 外部到達不可 |
| VPC エンドポイント（interface） | Private Subnet | 上記 RDS 用と共用 | ECS から AWS API へは VPC エンドポイント経由 |

- 外部公開：Cloudflare Tunnel が outbound のみ。受信ポートは一切開けない
- Public IP を ECS タスクに付与する必要があるのは、Cloudflare Tunnel のために IGW へ outbound 接続するため（Fargate は public subnet + public IP + IGW route が外向き接続の前提。NAT Gateway を入れない代替案）
- セキュリティはタスクの SG（Inbound 全 deny）と Cloudflare 側 + nginx 側の二段で担保する
- VPC エンドポイント（gateway / interface）：
  - S3 (gateway)
  - S3 Files (interface)
  - ECR API & DKR (interface)
  - Secrets Manager (interface)
  - CloudWatch Logs (interface)

### 6.3 セキュリティグループ

| SG | Inbound | Outbound |
|---|---|---|
| `ecs` | **すべて拒否**（Cloudflare Tunnel は outbound のみ） | 3306→`rds`、2049→S3 Files VPCE、443→VPCエンドポイント、443→Cloudflare（IGW 経由） |
| `rds` | 3306 from `ecs` | none |
| `vpce-*` | 443 from `ecs` | none |

詳細は [`09-infrastructure-and-deployment.md`](./09-infrastructure-and-deployment.md)。

## 7. 単一障害点（SPOF）と多重化

| コンポーネント | 多重化 | 理由 |
|---|---|---|
| Cloudflare Edge | Cloudflare Anycast | 通常運用で意識不要。障害時は ALB 一時起動の Runbook あり |
| ECS Fargate | **v1: desiredCount = 1 / max_capacity = 1**（NFS open-while-delete 等の実機検証完了まで AutoScaling は無効） | 個人専用 + 安全寄せ。検証完了後 v2 で 1〜3 |
| RDS Primary | Multi-AZ standby | 自動フェイルオーバ |
| RDS Read Replica | 1 個（v1） | Replica 不調時は縮退運転で Primary 一本化 |
| S3 Files | AWS が冗長化 | 同一リージョン内で多重化 |
| Secrets Manager | AWS マネージド | リージョン冗長 |
| **VPC エンドポイント** | 複数 AZ | これを忘れると AZ 障害でアプリが全滅 |

## 8. 設計の言質（明示的に約束しないこと）

- **マルチリージョン障害には対応しない**：ap-northeast-1 のリージョン全体障害は復旧優先度を下げる（個人用）
- **完全な無停止デプロイは目標としない**：Rolling Update で短時間の同時 2 タスクを許容
- **強整合な分散合意は採らない**：Primary を単一書き込み点とすることで合意問題を回避
- **Cloudflare 障害時のサービス停止は受容**：障害時は Runbook で ALB を緊急起動するが、即時自動切替はしない

---

次の章: [`03-domain-model.md`](./03-domain-model.md)
