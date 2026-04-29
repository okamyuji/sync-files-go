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
  id_bin              BINARY(16) (主キー、UUID v4 を BIN(16) で保管)
  email               VARCHAR(320) UNIQUE  (NFC 正規化済み)
  password_hash       VARCHAR(255)         (Argon2id)
  totp_secret_enc     VARBINARY(256)        (AES-GCM 暗号文)
  totp_secret_header  VARBINARY(64)         (AES-GCM の nonce + tag)
  totp_enabled        TINYINT(1)
  recovery_codes_hash JSON                  (Argon2id 文字列の配列)
  -- 鍵階層
  kek_enc             VARBINARY(80)         (KEK を Master Key で AES-Key-Wrap)
  kek_id_bin          BINARY(16)            (KEK の論理 ID)
  master_key_version  INT                   (どの世代の Master Key で wrap したか)
  created_at          DATETIME(6)
  last_login_at       DATETIME(6) nullable
  locked_until        DATETIME(6) nullable
  failed_login_count  INT default 0
}
```

個人専用のため通常 1 行のみだが、テスト・将来拡張のためテーブル化。

### 2.2 File

```
File {
  id_bin             BINARY(16) (主キー、論理ファイル ID = file_uuid)
  owner_id_bin       BINARY(16)
  parent_folder_id_bin BINARY(16) nullable
  name               VARCHAR(255)  (NFC 正規化済み)
  path               VARCHAR(2048) (フルパス)
  path_hash          VARBINARY(32) (SHA-256(NFC(path))、検索・索引用)
  current_version_id_bin BINARY(16) nullable (file_versions.id_bin への論理参照)
  size_bytes         BIGINT      (current_version の size のキャッシュ)
  content_type       VARCHAR(255)
  sha256             VARBINARY(32) (current_version の sha256 のキャッシュ)
  state              ENUM('draft','active','trashed','purged','gone')
  active_marker      VARBINARY(32) GENERATED  (state='active' のときだけハッシュ、CR-2 用)
  created_at         DATETIME(6)
  updated_at         DATETIME(6)
  deleted_at         DATETIME(6) nullable
}
```

**File は論理エンティティ**。ファイル本体（バイト列）は `file_versions` 経由でしか参照しない。`storage_key` も `encryption_key_id_bin` も File テーブルには持たない（CR-1 修正により versions/ 配下の immutable key で管理するため）。

不変条件：
- `state = 'active'` ⇒ `deleted_at IS NULL`
- `state = 'trashed'` ⇒ `deleted_at IS NOT NULL` AND `deleted_at > NOW() - INTERVAL 30 DAY`
- `state = 'purged'` ⇒ `deleted_at < NOW() - INTERVAL 30 DAY`
- `state = 'draft'` のレコードは `current_version_id_bin IS NULL` を許す

MySQL での「同名 active 一意性」の表現（CR-2 修正）：

「同一オーナー・同一フォルダ配下で、`state='active'` のファイル名が一意」を **DB 制約で守る**。MySQL は条件付きインデックスがないので、生成列でこれを表現する：

```sql
-- files テーブルに次の生成列と UNIQUE を追加（後述の §4 スキーマで反映済み）
active_marker VARBINARY(32) AS (
  CASE WHEN state = 'active'
       THEN UNHEX(SHA2(CONCAT_WS(':', HEX(owner_id_bin),
                                       COALESCE(HEX(parent_folder_id_bin),''),
                                       name), 256))
       ELSE NULL
  END
) STORED,
UNIQUE KEY uniq_files_active_name (active_marker)
```

- 同一オーナー・同フォルダ・同名で `state='active'` の行は同じハッシュ値を持ち、UNIQUE 制約でDB側が二重 INSERT を拒否する
- `state` が `trashed` / `purged` / `draft` / `gone` の行は NULL となり、UNIQUE 制約から除外される（MySQL は NULL を一意性検査で重複として扱わない）
- アプリ側のロックは並列パフォーマンスのために残すが、DB 制約が最終防衛線

`current_version_id_bin` は外部キー制約を張らず（`DEFERRABLE` 不在のため）、アプリ層で整合性を保つ：「`file_versions` に行を INSERT してから `files.current_version_id_bin` を UPDATE する」順序を厳守。

### 2.3 FileVersion

```
FileVersion {
  id_bin             BINARY(16) (主キー、= version_uuid)
  file_id_bin        BINARY(16) FOREIGN KEY → files.id_bin
  version_number     INT
  size_bytes         BIGINT
  sha256             VARBINARY(32)
  storage_key        VARCHAR(512)             ('owner-{user}/versions/{file_uuid}/{version_uuid}', immutable)
  s3_version_id      VARCHAR(128) nullable     (S3 バケットバージョニング ID。レイヤ分離のため記録)
  -- 鍵関連
  dek_enc            VARBINARY(80)             (DEK を該当ユーザの KEK で AES-Key-Wrap)
  kek_id_bin         BINARY(16)                (どの KEK で wrap したか)
  encryption_scheme  VARCHAR(32)               (例: 'tink-aead-streaming-aes-256-gcm-hkdf-1mb')
  encryption_header  VARBINARY(64)             (スキーム固有のヘッダ。base nonce / salt / version 等)
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

### 2.6 ShareLink（HIGH 修正：URL token を内部 ID と分離）

```
ShareLink {
  id_bin           BINARY(16)                  -- 内部 ID。外部 URL には出さない
  file_id_bin      BINARY(16) FOREIGN KEY → files.id_bin
  created_by_bin   BINARY(16) FOREIGN KEY → users.id_bin
  token_hash       VARBINARY(32) UNIQUE        -- SHA-256(URL token)。URL に出す token そのものは保管しない
  password_hash    VARCHAR(255) nullable       (Argon2id)
  expires_at       DATETIME(6) NOT NULL        -- v1 で「期限なし」を許さない (HIGH 修正)
  created_at       DATETIME(6)
  revoked_at       DATETIME(6) nullable
  view_count       BIGINT default 0
  download_count   BIGINT default 0
}
```

URL は `/share/<base64url-32-bytes-random>`。サーバは受信した token を SHA-256 してから DB の `token_hash` で索引引き。これにより：

- DB 内部 ID と URL を分離（漏洩時の影響範囲を限定）
- `token_hash` のみ保管なので DB 漏洩でも token 自体は復元できない
- v1 では「期限なし」リンクを禁止（最大 7 日に制限）

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
  totp_secret_enc     VARBINARY(256),                      -- TOTP secret を AES-GCM で暗号化保管
  totp_secret_header  VARBINARY(64),                       -- TOTP secret の AES-GCM nonce + tag (header)
  totp_enabled        TINYINT(1) NOT NULL DEFAULT 0,
  recovery_codes_hash JSON NOT NULL,
  -- CR-3: 鍵階層
  kek_enc             VARBINARY(80) NOT NULL,              -- KEK を Master Key で AES-Key-Wrap (RFC 3394)
  kek_id_bin          BINARY(16) NOT NULL,                 -- このユーザの KEK の論理 ID
  master_key_version  INT NOT NULL DEFAULT 1,              -- KEK が暗号化された Master Key の世代
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
  path_hash               VARBINARY(32) NOT NULL,                 -- SHA-256(NFC(path))、検索・索引用 (MEDIUM 修正)
  current_version_id_bin  BINARY(16),
  size_bytes              BIGINT NOT NULL,
  content_type            VARCHAR(255),
  sha256                  VARBINARY(32) NOT NULL,
  state                   ENUM('draft','active','trashed','purged','gone') NOT NULL,
  created_at              DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  updated_at              DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  deleted_at              DATETIME(6),
  -- CR-2: state='active' の同名重複を DB 制約で防ぐ生成列
  active_marker VARBINARY(32) AS (
    CASE WHEN state = 'active' THEN
      UNHEX(SHA2(CONCAT_WS(':', HEX(owner_id_bin),
                                  COALESCE(HEX(parent_folder_id_bin),''),
                                  name), 256))
    ELSE NULL END
  ) STORED,
  UNIQUE KEY uniq_files_active_name (active_marker),
  KEY idx_files_owner_path_hash (owner_id_bin, path_hash),
  KEY idx_files_owner_state     (owner_id_bin, state, updated_at DESC),
  KEY idx_files_deleted_at      (state, deleted_at),
  FULLTEXT KEY ft_files_name (name) WITH PARSER ngram,
  CONSTRAINT fk_files_owner FOREIGN KEY (owner_id_bin) REFERENCES users(id_bin)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- ファイルバージョン (immutable)
CREATE TABLE file_versions (
  id_bin                  BINARY(16) PRIMARY KEY,        -- = version_uuid、storage_key の末尾と一致
  file_id_bin             BINARY(16) NOT NULL,
  version_number          INT NOT NULL,
  size_bytes              BIGINT NOT NULL,
  sha256                  VARBINARY(32) NOT NULL,
  storage_key             VARCHAR(512) NOT NULL,         -- 'owner-{user}/versions/{file_uuid}/{version_uuid}'
  s3_version_id           VARCHAR(128),
  -- CR-3: DEK の保管 (file_version 単位で 1 個)
  dek_enc                 VARBINARY(80) NOT NULL,        -- DEK を該当ユーザの KEK で AES-Key-Wrap
  kek_id_bin              BINARY(16) NOT NULL,           -- どの KEK で wrap されたか (鍵ローテーション履歴)
  -- CR-4: AES-GCM ストリーム暗号化のヘッダ（nonce 戦略・チャンク長などをバージョン化）
  encryption_scheme       VARCHAR(32) NOT NULL,          -- 'tink-aead-streaming-aes-256-gcm-hkdf-1mb' 等
  encryption_header       VARBINARY(64) NOT NULL,        -- スキーム固有のヘッダ（base nonce / salt / version 等）
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

-- 共有リンク (HIGH 修正: token を内部 ID と分離)
CREATE TABLE share_links (
  id_bin           BINARY(16) PRIMARY KEY,
  file_id_bin      BINARY(16) NOT NULL,
  created_by_bin   BINARY(16) NOT NULL,
  token_hash       VARBINARY(32) NOT NULL,        -- SHA-256(URL に露出する 32 bytes random token)
  password_hash    VARCHAR(255),
  expires_at       DATETIME(6) NOT NULL,           -- v1 では NOT NULL (期限なしを禁止)
  created_at       DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  revoked_at       DATETIME(6),
  view_count       BIGINT NOT NULL DEFAULT 0,
  download_count   BIGINT NOT NULL DEFAULT 0,
  UNIQUE KEY uniq_share_links_token (token_hash),
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

## 5. S3 Files 上のファイル配置規則（**immutable versions only**）

```
/var/data/                                      ← S3 Files マウントポイント
├── owner-{user_uuid}/
│   ├── versions/{file_uuid}/{version_uuid}     ← すべての版を immutable に保存（書いたら触らない）
│   └── tmp/{upload_session_uuid}.part          ← アップロード中の一時
└── _system/
    ├── kek/                                    ← 鍵階層 (07-security 参照)
    └── healthcheck.txt
```

> **設計の根拠（immutable versions only）**: すべての版を `versions/{file_uuid}/{version_uuid}` という **immutable key** に書き、「現行版の指し示し」は **DB の `files.current_version_id_bin` 列だけ**で表現する。これにより：
> - DB COMMIT が成功して初めて新版が「現行」になる
> - DB COMMIT 失敗時、旧版の参照は変わらず、新版オブジェクトはどの DB 行からも参照されない無参照状態 → 補正ジョブで容易に検出可能（`versions/*/*` を走査し、どの `file_versions` 行からも参照されていないキーを `/_orphan/` へ移動）
> - 可変キー（同じパスへの上書き）は一切持たない。ダウンロードは常に `SELECT current_version_id_bin FROM files` → `open(versions/{file_uuid}/{version_uuid})` の二段で行う

設計ポイント：

- **ファイル名は UUID v4**：`file_uuid` はファイルの論理 ID、`version_uuid` はバージョンの ID（共に Primary Key）
- **リネームは O(1)**：DB の `path` / `name` / `parent_folder_id_bin` のみ更新。S3 キーは不変
- **ソフト削除中もファイル本体は `versions/` に残す**：`files.deleted_at` を立てるだけ
- **物理削除（purge）時のみ**`versions/{file_uuid}/*` を `os.Remove` で削除（S3 DeleteMarker が付与される。バージョニングはバケット側で別途持つ）
- 「現行版」「旧版」という物理的な区別は S3 上には存在しない。すべては DB の `files.current_version_id_bin` 経由

参考: NFS v4.1+ の `os.Rename` は同 FS 内アトミックだが、本設計では tmp → versions/ への単一の rename 後はそのキーを上書きしないため、rename のアトミック性に対する依存度が下がる（[`13`](./13-risks-and-open-questions.md) §3）。

## 6. メタデータと S3 Files の整合性

ファイル本体（S3 Files）とメタデータ（MySQL）は別リソース。原子的トランザクションは張れない。本設計では以下のルールで整合性を保つ：

### 6.1 書き込み順（commit-after-write、immutable versions）

```
1. version_uuid := uuid.New()
2. Acquire OCC (Primary 上で SELECT FOR UPDATE files... + check If-Match)
3. Write encrypted bytes to /var/data/owner-X/tmp/{upload_uuid}
4. fsync(2)  -- NFS では best-effort、後段の SHA-256 検証で補強
5. os.Rename(tmp/{upload_uuid} → versions/{file_uuid}/{version_uuid})
   -- 同 FS 内、原子的、かつ versions/ 配下は以後不変（書き換えない）
6. BEGIN
     INSERT INTO file_versions (id_bin=version_uuid, file_id_bin=file_uuid, ...);
     UPDATE files SET current_version_id_bin = version_uuid, updated_at = NOW(6), ... WHERE id_bin = file_uuid;
     INSERT INTO audit_logs ...;
   COMMIT;
7. （DB COMMIT 失敗時）versions/{file_uuid}/{version_uuid} は無参照のまま残る。
   補正ジョブが「file_versions に対応行がない versions/* キー」を検出して /_orphan/ へ隔離。
   この時点でも files.current_version_id_bin は旧版のままなので、復元は不要 (旧版が引き続き「現行」)。
8. レスポンスを返す際に Set-Cookie で raw_until=now+5s（ハッシュ署名つき）を発行。
   後続リクエストはこの cookie を見て forcePrimary を判定する（HIGH 修正、§DBRouter）。
```

**重要**: ステップ 5 は `versions/{file_uuid}/{version_uuid}` という **新規キー** への書き込みであり、既存キーを上書きしない。これにより、DB COMMIT に対する S3 側の状態は常に「新キーが追加された／何もされていない」のいずれかであり、混線が起きない。

### 6.2 読み取り

```
1. SELECT files.current_version_id_bin (DBRouter.Reader = 通常は Replica、RAW window 中は Primary)
2. SELECT file_versions.storage_key WHERE id_bin = current_version_id_bin
3. open(/var/data/owner-X/versions/{file_uuid}/{version_uuid})
4. AES-GCM 復号して stream
```

ステップ 1 と 2 を 1 SQL の JOIN で行ってもよい。「現行版」を物理的に表すのは DB の参照のみ。

### 6.3 補正ジョブ（reconciliation）

- **孤児ファイル検出（version レベル）**: 1 日 1 回、`/var/data/owner-*/versions/*/*` を走査し、対応する `file_versions` レコードがないキーを `/_orphan/` へ移動（DB COMMIT 失敗の取り残し）
- **メタデータ孤児検出**: `file_versions` の各行に対応する S3 Files 上のオブジェクトが存在しない場合を検出してアラート
- **古いバージョンの prune（90 日経過）**: アプリ層日次バッチ `prune-old-versions` が `file_versions.created_at < NOW() - INTERVAL 90 DAY` かつ `id_bin <> files.current_version_id_bin` のものを `os.Remove(versions/{file_uuid}/{version_uuid})` + `DELETE FROM file_versions` で削除。詳細は [`05-file-operations-logic-tree.md`](./05-file-operations-logic-tree.md) §7.2。S3 lifecycle の `noncurrent_version_expiration` は immutable key 設計では機能しないため、このバッチが唯一の prune 経路。
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

設計書中の SQL 例は **MySQL 構文を正準** とし、プレースホルダは `?` で統一する：

| 表記 | 意味 |
|---|---|
| `?` | go-sql-driver/mysql のプレースホルダ。Go 側は `db.Exec(ctx, sql, args...)` で渡す |
| `owner_id`, `file_id` のような短い列名 | 実装は `owner_id_bin`, `file_id_bin`（BINARY(16)）。短縮表記は文脈が明らかな疑似コードのみ |
| `now()` / `NOW()` | MySQL の `NOW()` または `CURRENT_TIMESTAMP(6)` |
| `INTERVAL 30 DAY` | MySQL 構文そのまま |
| `FOR UPDATE` | InnoDB の行ロック |

旧設計初版の `$1` / `$2` プレースホルダ（PostgreSQL 風）は誤りで、すべて `?` に揃えること。リポジトリ層（`internal/repo/mysql/*.go`）が実 SQL を保持する。

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
