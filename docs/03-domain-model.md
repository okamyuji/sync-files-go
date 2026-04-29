# 03. ドメインモデルとデータモデル

## 1. ドメインの考え方

「ドメインは現実世界の写像」という素朴な原則を取る。本システムの中心概念は次の 4 つ：

```
                              [User]  (オーナー本人)
                                │
                                │ owns
                                ▼
            ┌───────────────────────────────────────┐
            │              [File]                    │ ── (logical entity)
            │                                        │
            │  has many: FileVersion (履歴)          │
            │  has at most one: ShareLink (公開リンク) │
            │  has many: Tag                          │
            │  may be soft-deleted: deleted_at        │
            └────────────────┬───────────────────────┘
                             │
                             │ tracked by
                             ▼
                       [AuditLogEntry]  (誰がいつ何をしたか)
```

設計の出発点として、**「ファイル本体（バイト列）」と「ファイルの存在（メタデータ）」を分離する** ことを徹底する。理由：

- バイト列は S3 Files に置く（耐久性・容量）
- メタデータは MySQL Primary に置く（書き込み・read-after-write）と Read Replica（一覧・検索・履歴）
- 両者は **論理的に同期するが、原子的トランザクションは張れない**（分散の壁）。ゆえに後述の冪等処理と再同期ジョブが必要

## 2. エンティティ詳細

### 2.1 User

```
User {
  id_bin        BINARY(16) (主キー、UUID v4 を BIN(16) で保管)
  email         VARCHAR(320) UNIQUE  (NFC 正規化済み)
  password_hash VARCHAR(255)         (Argon2id)
  totp_secret_enc VARBINARY(128)     (AES-GCM で暗号化済み)
  totp_enabled  TINYINT(1)
  recovery_codes_hash JSON            (Argon2id 文字列の配列)
  created_at    DATETIME(6)
  last_login_at DATETIME(6) nullable
  locked_until  DATETIME(6) nullable
  failed_login_count INT default 0
}
```

個人専用のため通常 1 行のみだが、テスト・将来拡張のためテーブル化。

### 2.2 File

```
File {
  id_bin             BINARY(16) (主キー)
  owner_id_bin       BINARY(16)
  parent_folder_id_bin BINARY(16) nullable
  name               VARCHAR(255)  (NFC 正規化済み)
  path               VARCHAR(2048) (フルパス)
  current_version_id_bin BINARY(16) nullable
  size_bytes         BIGINT
  content_type       VARCHAR(255)
  storage_key        VARCHAR(512)  (S3 Files 上のキー)
  created_at         DATETIME(6)
  updated_at         DATETIME(6)
  deleted_at         DATETIME(6) nullable
  state              ENUM('draft','active','trashed','purged','gone')   -- 採択: ENUM か CHECK 制約
  sha256             VARBINARY(32)
  encryption_key_id_bin BINARY(16)
}
```

不変条件：
- `state = 'active'` ⇒ `deleted_at IS NULL`
- `state = 'trashed'` ⇒ `deleted_at IS NOT NULL` AND `deleted_at > NOW() - INTERVAL 30 DAY`
- `state = 'purged'` ⇒ `deleted_at < NOW() - INTERVAL 30 DAY`
- `state = 'draft'` のレコードは `current_version_id_bin IS NULL` を許す

MySQL の制約事項：
- 部分インデックス（PostgreSQL の `WHERE` 付き）が無いため、「同名 active ファイルの一意性」は **アプリ層で `SELECT FOR UPDATE` + INSERT で担保** する（`(owner_id_bin, path)` の UNIQUE は不採用）
- `DEFERRABLE` 外部キーが無いため、`current_version_id_bin` は外部キー制約を張らず、アプリ層で整合性を保つ（あるいは「先に file 行を INSERT し、後で UPDATE」する手順）

### 2.3 FileVersion

```
FileVersion {
  id_bin             BINARY(16) (主キー)
  file_id_bin        BINARY(16) FOREIGN KEY → files.id_bin
  version_number     INT
  size_bytes         BIGINT
  sha256             VARBINARY(32)
  storage_key        VARCHAR(512)
  s3_version_id      VARCHAR(128) nullable
  encryption_key_id_bin BINARY(16)
  created_at         DATETIME(6)
  created_by_session_id_bin BINARY(16) nullable
  deleted_by_user    TINYINT(1) default 0
}
UNIQUE (file_id_bin, version_number)
```

### 2.4 Folder

```
Folder {
  id_bin               BINARY(16)
  owner_id_bin         BINARY(16)
  parent_folder_id_bin BINARY(16) nullable
  name                 VARCHAR(255)
  path                 VARCHAR(2048)
  created_at           DATETIME(6)
  deleted_at           DATETIME(6) nullable
}
```

ルートは `parent_folder_id_bin IS NULL` AND `path = ''`。

### 2.5 Tag / FileTag

```
Tag {
  id_bin       BINARY(16)
  owner_id_bin BINARY(16)
  name         VARCHAR(64)
  created_at   DATETIME(6)
  UNIQUE (owner_id_bin, name)
}

FileTag {
  file_id_bin BINARY(16)
  tag_id_bin  BINARY(16)
  PRIMARY KEY (file_id_bin, tag_id_bin)
}
```

### 2.6 ShareLink

```
ShareLink {
  id_bin           BINARY(16)
  file_id_bin      BINARY(16) FOREIGN KEY → files.id_bin
  created_by_bin   BINARY(16) FOREIGN KEY → users.id_bin
  password_hash    VARCHAR(255) nullable  (Argon2id)
  expires_at       DATETIME(6) nullable
  created_at       DATETIME(6)
  revoked_at       DATETIME(6) nullable
  view_count       BIGINT default 0
  download_count   BIGINT default 0
}
```

### 2.7 ShareLinkAccess

```
ShareLinkAccess {
  id_bin            BINARY(16)
  share_link_id_bin BINARY(16) FOREIGN KEY → share_links.id_bin
  ip_addr           VARBINARY(16)   (IPv4/IPv6 を 16 bytes で統一保管)
  user_agent        VARCHAR(512)
  accessed_at       DATETIME(6)
  action            ENUM('view','download','password_failure')
  http_status       INT
}
```

### 2.8 AuditLog

```
AuditLog {
  id_bin       BINARY(16)
  occurred_at  DATETIME(6)
  actor_id_bin BINARY(16) nullable
  actor_kind   ENUM('user','public_viewer','system')
  action       VARCHAR(64)        -- 'file.upload', 'file.update', etc.
  target_kind  VARCHAR(32)
  target_id_bin BINARY(16) nullable
  details_json JSON
  ip_addr      VARBINARY(16) nullable
  user_agent   VARCHAR(512) nullable
  irreversible TINYINT(1) default 0
}
```

INSERT のみ。UPDATE / DELETE はアプリケーションロールに付与しない。

### 2.9 UploadSession（tus.io レジューム用）

```
UploadSession {
  id_bin               BINARY(16)
  owner_id_bin         BINARY(16)
  parent_folder_id_bin BINARY(16) nullable
  filename             VARCHAR(255)
  size_total           BIGINT
  size_received        BIGINT default 0
  storage_key          VARCHAR(512)
  if_match             VARCHAR(64) nullable   -- 上書き対象の version_id (UUID 文字列)
  if_none_match        VARCHAR(8)  nullable   -- '*'
  created_at           DATETIME(6)
  expires_at           DATETIME(6)            -- 7 日
  completed_at         DATETIME(6) nullable
}
```

### 2.10 Session

```
Session {
  id_bin       BINARY(16)
  user_id_bin  BINARY(16)
  created_at   DATETIME(6)
  last_seen_at DATETIME(6)
  expires_at   DATETIME(6)
  ip_addr      VARBINARY(16)
  user_agent   VARCHAR(512)
}
```

## 3. ER 図（簡略）

```
       ┌─────────┐
       │  User   │
       └────┬────┘
       1    │    1
            │
       ┌────┴────┐                 ┌─────────────┐
       │  File   │ 1 ────── n ───> │ FileVersion │
       └────┬────┘                 └─────────────┘
            │ 1                    
            │                      
       ┌────┴────┐ n ── 1 ┌────────┐
       │ Folder  │────────│ Folder │ (自己参照ツリー)
       └─────────┘        └────────┘
            │
            │ 1
            │
            │ n
       ┌────┴────┐                  ┌──────────────────┐
       │  Tag    │ n ── n ─ FileTag │   AuditLog       │
       └─────────┘                  └──────────────────┘

       ┌──────────────┐
       │  ShareLink   │ n ── 1 ── File
       └──────┬───────┘
              │ 1
              │
              ▼ n
       ┌──────────────────┐
       │ ShareLinkAccess  │
       └──────────────────┘
```

## 4. MySQL スキーマ（正準）

最終的なマイグレーション SQL は `migrations/` に置くが、設計時点での参照スキーマを以下に示す。

```sql
SET sql_mode = 'STRICT_ALL_TABLES,NO_ENGINE_SUBSTITUTION';

-- ユーザ
CREATE TABLE users (
  id_bin              BINARY(16) PRIMARY KEY,
  email               VARCHAR(320) NOT NULL,
  password_hash       VARCHAR(255) NOT NULL,
  totp_secret_enc     VARBINARY(256),
  totp_enabled        TINYINT(1) NOT NULL DEFAULT 0,
  recovery_codes_hash JSON NOT NULL,
  created_at          DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  last_login_at       DATETIME(6),
  locked_until        DATETIME(6),
  failed_login_count  INT NOT NULL DEFAULT 0,
  UNIQUE KEY uniq_users_email (email)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- フォルダ
CREATE TABLE folders (
  id_bin                BINARY(16) PRIMARY KEY,
  owner_id_bin          BINARY(16) NOT NULL,
  parent_folder_id_bin  BINARY(16),
  name                  VARCHAR(255) NOT NULL,
  path                  VARCHAR(2048) NOT NULL,
  created_at            DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  deleted_at            DATETIME(6),
  KEY idx_folders_owner_path (owner_id_bin, path(255)),
  KEY idx_folders_parent     (parent_folder_id_bin),
  CONSTRAINT fk_folders_owner FOREIGN KEY (owner_id_bin) REFERENCES users(id_bin)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- ファイル
CREATE TABLE files (
  id_bin                  BINARY(16) PRIMARY KEY,
  owner_id_bin            BINARY(16) NOT NULL,
  parent_folder_id_bin    BINARY(16),
  name                    VARCHAR(255) NOT NULL,
  path                    VARCHAR(2048) NOT NULL,
  current_version_id_bin  BINARY(16),
  size_bytes              BIGINT NOT NULL,
  content_type            VARCHAR(255),
  storage_key             VARCHAR(512) NOT NULL,
  sha256                  VARBINARY(32) NOT NULL,
  encryption_key_id_bin   BINARY(16) NOT NULL,
  state                   ENUM('draft','active','trashed','purged','gone') NOT NULL,
  created_at              DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  updated_at              DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  deleted_at              DATETIME(6),
  KEY idx_files_owner_path  (owner_id_bin, path(255)),
  KEY idx_files_owner_state (owner_id_bin, state, updated_at DESC),
  KEY idx_files_deleted_at  (state, deleted_at),
  FULLTEXT KEY ft_files_name (name) WITH PARSER ngram,
  CONSTRAINT fk_files_owner FOREIGN KEY (owner_id_bin) REFERENCES users(id_bin)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- ファイルバージョン
CREATE TABLE file_versions (
  id_bin                  BINARY(16) PRIMARY KEY,
  file_id_bin             BINARY(16) NOT NULL,
  version_number          INT NOT NULL,
  size_bytes              BIGINT NOT NULL,
  sha256                  VARBINARY(32) NOT NULL,
  storage_key             VARCHAR(512) NOT NULL,
  s3_version_id           VARCHAR(128),
  encryption_key_id_bin   BINARY(16) NOT NULL,
  created_at              DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  created_by_session_id_bin BINARY(16),
  deleted_by_user         TINYINT(1) NOT NULL DEFAULT 0,
  UNIQUE KEY uniq_file_versions (file_id_bin, version_number),
  KEY idx_file_versions_file (file_id_bin, version_number DESC),
  CONSTRAINT fk_file_versions_file FOREIGN KEY (file_id_bin) REFERENCES files(id_bin) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- タグ
CREATE TABLE tags (
  id_bin       BINARY(16) PRIMARY KEY,
  owner_id_bin BINARY(16) NOT NULL,
  name         VARCHAR(64) NOT NULL,
  created_at   DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  UNIQUE KEY uniq_tags_owner_name (owner_id_bin, name),
  CONSTRAINT fk_tags_owner FOREIGN KEY (owner_id_bin) REFERENCES users(id_bin)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE file_tags (
  file_id_bin BINARY(16) NOT NULL,
  tag_id_bin  BINARY(16) NOT NULL,
  PRIMARY KEY (file_id_bin, tag_id_bin),
  CONSTRAINT fk_ft_file FOREIGN KEY (file_id_bin) REFERENCES files(id_bin) ON DELETE CASCADE,
  CONSTRAINT fk_ft_tag  FOREIGN KEY (tag_id_bin)  REFERENCES tags(id_bin)  ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- 共有リンク
CREATE TABLE share_links (
  id_bin           BINARY(16) PRIMARY KEY,
  file_id_bin      BINARY(16) NOT NULL,
  created_by_bin   BINARY(16) NOT NULL,
  password_hash    VARCHAR(255),
  expires_at       DATETIME(6),
  created_at       DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  revoked_at       DATETIME(6),
  view_count       BIGINT NOT NULL DEFAULT 0,
  download_count   BIGINT NOT NULL DEFAULT 0,
  KEY idx_share_links_file (file_id_bin),
  KEY idx_share_links_active (file_id_bin, revoked_at, expires_at),
  CONSTRAINT fk_share_links_file FOREIGN KEY (file_id_bin) REFERENCES files(id_bin) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE share_link_accesses (
  id_bin            BINARY(16) PRIMARY KEY,
  share_link_id_bin BINARY(16) NOT NULL,
  ip_addr           VARBINARY(16) NOT NULL,
  user_agent        VARCHAR(512),
  accessed_at       DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  action            ENUM('view','download','password_failure') NOT NULL,
  http_status       INT,
  KEY idx_share_link_accesses_link (share_link_id_bin, accessed_at DESC),
  CONSTRAINT fk_sla_link FOREIGN KEY (share_link_id_bin) REFERENCES share_links(id_bin) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- 監査ログ（INSERT only）
CREATE TABLE audit_logs (
  id_bin        BINARY(16) PRIMARY KEY,
  occurred_at   DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  actor_id_bin  BINARY(16),
  actor_kind    ENUM('user','public_viewer','system') NOT NULL,
  action        VARCHAR(64) NOT NULL,
  target_kind   VARCHAR(32) NOT NULL,
  target_id_bin BINARY(16),
  details_json  JSON NOT NULL,
  ip_addr       VARBINARY(16),
  user_agent    VARCHAR(512),
  irreversible  TINYINT(1) NOT NULL DEFAULT 0,
  KEY idx_audit_logs_actor_time (actor_id_bin, occurred_at DESC),
  KEY idx_audit_logs_target     (target_kind, target_id_bin, occurred_at DESC)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- アップロードセッション
CREATE TABLE upload_sessions (
  id_bin               BINARY(16) PRIMARY KEY,
  owner_id_bin         BINARY(16) NOT NULL,
  parent_folder_id_bin BINARY(16),
  filename             VARCHAR(255) NOT NULL,
  size_total           BIGINT NOT NULL,
  size_received        BIGINT NOT NULL DEFAULT 0,
  storage_key          VARCHAR(512) NOT NULL,
  if_match             VARCHAR(64),
  if_none_match        VARCHAR(8),
  created_at           DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  expires_at           DATETIME(6) NOT NULL,
  completed_at         DATETIME(6),
  KEY idx_upload_sessions_expiry (expires_at, completed_at),
  CONSTRAINT fk_us_owner FOREIGN KEY (owner_id_bin) REFERENCES users(id_bin)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- セッション
CREATE TABLE sessions (
  id_bin       BINARY(16) PRIMARY KEY,
  user_id_bin  BINARY(16) NOT NULL,
  created_at   DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  last_seen_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  expires_at   DATETIME(6) NOT NULL,
  ip_addr      VARBINARY(16),
  user_agent   VARCHAR(512),
  KEY idx_sessions_user (user_id_bin),
  KEY idx_sessions_expiry (expires_at),
  CONSTRAINT fk_sessions_user FOREIGN KEY (user_id_bin) REFERENCES users(id_bin) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- レート制限（IP / アカウント単位）
CREATE TABLE rate_limit_buckets (
  bucket_key   VARCHAR(255) PRIMARY KEY,
  tokens       DOUBLE NOT NULL,
  refilled_at  DATETIME(6) NOT NULL,
  expires_at   DATETIME(6) NOT NULL,
  KEY idx_rate_limit_expiry (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
```

### 4.1 同名 active 一意性

PostgreSQL の部分インデックスが無いため、`(owner_id_bin, path)` への UNIQUE は使わず：

```sql
-- アプリ層の擬似コード:
BEGIN;
SELECT id_bin FROM files
 WHERE owner_id_bin = ? AND path = ? AND state = 'active'
 FOR UPDATE;
-- なければ INSERT、あれば UPDATE（OCC）
COMMIT;
```

ロックは `FOR UPDATE` で行ベースロック。Replica では `FOR UPDATE` が許可されないため、必ず Writer (Primary) で実行。

### 4.2 DB ロール分離

```sql
-- アプリロール（DML 専用）
CREATE USER 'sync_app'@'%' IDENTIFIED BY '<from secrets manager>' REQUIRE SSL;
GRANT SELECT, INSERT, UPDATE, DELETE ON sync.* TO 'sync_app'@'%';
-- 監査ログは INSERT/SELECT のみ
REVOKE UPDATE, DELETE ON sync.audit_logs FROM 'sync_app'@'%';

-- マイグレーションロール
CREATE USER 'sync_migrate'@'%' IDENTIFIED BY '<from secrets manager>' REQUIRE SSL;
GRANT ALL PRIVILEGES ON sync.* TO 'sync_migrate'@'%';
```

Read Replica にはアプリ用の `sync_app` 同名・同 password でレプリケーション。Reader 接続でも `INSERT/UPDATE/DELETE` を呼んでも MySQL Replica は `--read-only` を持つため拒否される（保険）。

### 4.3 文字セット

`utf8mb4`（4 バイト UTF-8、絵文字対応）。照合は `utf8mb4_0900_ai_ci`（MySQL 8 デフォルト、UCA 9.0 ベース）。NFC 正規化はアプリ層で行う。

## 5. S3 Files 上のファイル配置規則

```
/var/data/                                      ← S3 Files マウントポイント
├── owner-{user_uuid}/
│   ├── current/{file_uuid}                     ← 現行版の暗号文
│   ├── tmp/{upload_session_uuid}.part          ← アップロード中の一時
│   └── versions/{file_uuid}/v{N}               ← 旧版
└── _system/
    ├── kek/                                    ← 鍵階層 (07-security 参照)
    └── healthcheck.txt
```

設計ポイント：

- **ファイル名は UUID 固定**：リネームを O(1) で
- **ソフト削除中もファイル本体は `current/` に残す**：`deleted_at` を立てるだけ
- **物理削除（purge）時のみ S3 オブジェクトに DeleteMarker を付与**
- **バージョンは `versions/{file_uuid}/v{N}` に明示的に保存**
- 注意: NFS v4.1+ の `os.Rename` は同 FS 内アトミックの規定。AWS 実装の挙動は実機検証（[`13`](./13-risks-and-open-questions.md) §3）

## 6. メタデータと S3 Files の整合性

ファイル本体（S3 Files）とメタデータ（MySQL）は別リソース。原子的トランザクションは張れない。本設計では以下のルールで整合性を保つ：

### 6.1 書き込み順（commit-after-write）

```
1. Acquire OCC (Primary 上で SELECT FOR UPDATE current version + check If-Match)
2. Write encrypted bytes to /var/data/.../tmp/{upload_uuid}
3. fsync(2)  -- NFS では best-effort、後段の SHA-256 検証で補強
4. os.Rename(tmp → current/{file_uuid})  -- 同 FS 内、原子的
5. BEGIN; INSERT file_versions; UPDATE files SET current_version_id_bin = new; INSERT audit_log; COMMIT;
6. （DB COMMIT 失敗時）孤児ファイル補正ジョブが /_orphan に隔離
7. レスポンスを返す前に context.WithReadAfterWrite(ctx) を仕込み、
   直後の取得は Primary を読む（Replica 遅延の影響を回避）
```

### 6.2 読み取り

```
1. SELECT files (DBRouter.Reader = 通常は Replica、RAW window 中は Primary)
2. open(/var/data/.../current/{file_uuid})
3. AES-GCM 復号して stream
```

### 6.3 補正ジョブ（reconciliation）

- **孤児ファイル検出**: 1 日 1 回、`/var/data/owner-*/current/` を走査し、対応する `files` レコードがないものを `/_orphan/` へ移動
- **メタデータ孤児検出**: `files` で `state = 'active'` だが S3 Files 上のオブジェクトが存在しないものを検出してアラート
- **`upload_sessions.expires_at < now()` のクリーンアップ**: 1 時間ごとに走査し、`tmp/*.part` を削除

## 7. ストアドプロシージャ vs アプリ層

**ストアドプロシージャは使わない**。理由は変更管理・テスト・移植性。例外として、`audit_logs` のトリガでの強制 INSERT は採用しない（アプリが必ず INSERT する責務を負い、テストでカバー）。

## 8. 不変条件の DB 制約への落とし込み

| 不変条件 | DB 制約・補強 |
|---|---|
| INV-1 物理削除は二段階 | `state` ENUM + アプリ側の遷移ロジック |
| INV-2 書き込みは累積 | `file_versions` への INSERT を強制（アプリ層） + S3 バージョニング ON |
| INV-4 未完了は本番に反映しない | `state = 'draft'` 状態を経由。`upload_sessions` が COMMIT 後にのみ `files` を更新 |
| INV-5 破壊的操作の確認 | UI 側 + サーバ側の二段確認 |

## 8.1 設計書中の SQL 表記の約束

設計書（特に [`04-sync-semantics.md`](./04-sync-semantics.md) と [`05-file-operations-logic-tree.md`](./05-file-operations-logic-tree.md)）の SQL 例では、可読性のため次の **擬似 SQL 表記** を採る：

| 設計書での書き方 | 実装上の対応 |
|---|---|
| `$1`, `$2` プレースホルダ | go-sql-driver/mysql は `?` を使う |
| `owner_id`, `file_id` のような短い列名 | 実装は `owner_id_bin`, `file_id_bin`（BINARY(16)） |
| `now()` | MySQL `NOW()` または `CURRENT_TIMESTAMP(6)` |
| `INTERVAL 30 DAY` | MySQL 構文そのまま |
| `FOR UPDATE` | MySQL の InnoDB ロック（同等） |

実装時はこの対応に従って読み替える。テンプレ的に書いた SQL を本物のクエリに変換するのはリポジトリ層の責務。

## 9. 命名規則

- テーブル名は複数形 snake_case (`files`, `file_versions`)
- 列名は snake_case
- 主キーは `id_bin`（BINARY(16) = UUID v4）。アプリ側で `uuid.UUID` として扱う
- 外部キーは `<entity>_id_bin`
- タイムスタンプは `DATETIME(6)`、サーバ DB は UTC で保存（タイムゾーン管理は app の責務）
- `ENUM` を採用（値の追加に弱いが、実用上 v1 で増えない見込み。増えるなら CHECK + VARCHAR に切替）

## 10. リポジトリ層と DBRouter

```go
// internal/repo/repo.go
type FilesReader interface {
    GetByID(ctx context.Context, id uuid.UUID) (*domain.File, error)
    ListByOwner(ctx context.Context, ownerID uuid.UUID, opts ListOpts) ([]*domain.File, error)
    Search(ctx context.Context, ownerID uuid.UUID, q string) ([]*domain.File, error)
}

type FilesWriter interface {
    Insert(ctx context.Context, tx *sql.Tx, f *domain.File) error
    UpdateVersion(ctx context.Context, tx *sql.Tx, fileID, newVersionID uuid.UUID) error
    SoftDelete(ctx context.Context, tx *sql.Tx, fileID uuid.UUID) error
    Purge(ctx context.Context, tx *sql.Tx, fileID uuid.UUID) error
    SelectForUpdate(ctx context.Context, tx *sql.Tx, ownerID uuid.UUID, path string) (*domain.File, error)
}

type FilesRepo struct {
    router *DBRouter
}

func (r *FilesRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.File, error) {
    db := r.router.Reader(ctx) // RAW window 中は Primary
    return queryFile(ctx, db, id)
}

func (r *FilesRepo) BeginWrite(ctx context.Context) (*sql.Tx, error) {
    return r.router.Writer(ctx).BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
}
```

ハンドラ：

```go
func uploadHandler(repo *FilesRepo) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        ctx := r.Context()
        tx, _ := repo.BeginWrite(ctx)
        defer tx.Rollback()

        cur, _ := repo.SelectForUpdate(ctx, tx, ownerID, path)
        // OCC ロジック...
        repo.Insert(ctx, tx, newFile)
        repo.UpdateVersion(ctx, tx, fileID, newVersionID)
        tx.Commit()

        // 直後のレスポンス用 read を Primary に向ける
        ctx = repo.router.WithReadAfterWrite(ctx)
        f, _ := repo.GetByID(ctx, fileID)
        renderFileRow(w, f)
    }
}
```

これにより、ハンドラは「どの DB を使うか」を細かく知らずに済む。

---

次の章: [`04-sync-semantics.md`](./04-sync-semantics.md)
