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
- メタデータは PostgreSQL に置く（検索・トランザクション・整合性）
- 両者は **論理的に同期するが、原子的トランザクションは張れない**（分散の壁）。ゆえに後述の冪等処理と再同期ジョブが必要

## 2. エンティティ詳細

### 2.1 User

```
User {
  id            UUID (主キー)
  email         string (unique, lowercased, NFC)
  password_hash string (Argon2id)
  totp_secret   bytes (AES-256-GCM で暗号化済み)
  totp_enabled  bool
  recovery_codes_hash text[]   (Argon2id)
  created_at    timestamptz
  last_login_at timestamptz
  locked_until  timestamptz nullable  (ログイン失敗による一時ロック)
  failed_login_count int default 0
}
```

個人専用のため通常 1 行のみだが、テスト・将来拡張のためテーブル化。

### 2.2 File

```
File {
  id            UUID (主キー)
  owner_id      UUID (User.id)
  parent_folder_id UUID nullable (Folder.id)
  name          string  (ファイル名のみ。NFC 正規化済み)
  path          string  (フルパス。冗長だが検索高速化のため)
  current_version_id UUID  (FileVersion.id への参照)
  size_bytes    bigint
  content_type  string
  storage_key   string  (S3 Files 上のキー = 'owner-{uuid}/current/{file_uuid}')
  created_at    timestamptz
  updated_at    timestamptz
  deleted_at    timestamptz nullable    -- ソフト削除
  state         enum('draft','active','trashed','purged','gone')
  sha256        bytes  (32 bytes)  -- 暗号化前の本体に対するハッシュ
  encryption_key_id UUID  (内部の鍵管理。鍵階層は 07-security)
}
```

不変条件：
- `state = 'active'` ⇒ `deleted_at IS NULL`
- `state = 'trashed'` ⇒ `deleted_at IS NOT NULL` AND `deleted_at > now() - interval '30 days'`
- `state = 'purged'` ⇒ `deleted_at < now() - interval '30 days'`
- `state = 'draft'` のレコードは `current_version_id IS NULL` を許す

### 2.3 FileVersion

```
FileVersion {
  id              UUID (主キー)
  file_id         UUID (File.id)
  version_number  int  (1, 2, 3, ...)
  size_bytes      bigint
  sha256          bytes
  storage_key     string  (S3 Files 上のキー = 'owner-{uuid}/versions/{file_uuid}/v{N}')
  s3_version_id   string  (S3 バケットバージョニングの version-id)
  encryption_key_id UUID
  created_at      timestamptz
  created_by_session_id UUID nullable  (どのセッション/端末から作成されたか・OCC で利用)
  deleted_by_user bool default false   (ユーザが個別削除した版か)
}
```

`(file_id, version_number)` に UNIQUE 制約。

### 2.4 Folder

```
Folder {
  id            UUID (主キー)
  owner_id      UUID
  parent_folder_id UUID nullable
  name          string (NFC 正規化済み)
  path          string (フルパス、末尾 / なし)
  created_at    timestamptz
  deleted_at    timestamptz nullable
}
```

ルートは `parent_folder_id IS NULL` AND `path = ''`。

### 2.5 Tag / FileTag

```
Tag {
  id        UUID
  owner_id  UUID
  name      string (NFC 正規化済み、unique per owner)
  created_at timestamptz
}

FileTag {
  file_id UUID
  tag_id  UUID
  PRIMARY KEY (file_id, tag_id)
}
```

### 2.6 ShareLink

```
ShareLink {
  id            UUID (主キー、URL に露出する)
  file_id       UUID (File.id)
  created_by    UUID (User.id)
  password_hash string nullable  (Argon2id)
  expires_at    timestamptz nullable
  created_at    timestamptz
  revoked_at    timestamptz nullable
  view_count    bigint default 0
  download_count bigint default 0
}
```

### 2.7 ShareLinkAccess

```
ShareLinkAccess {
  id            UUID
  share_link_id UUID
  ip_addr       inet  -- 監査・濫用検出
  user_agent    string
  accessed_at   timestamptz
  action        enum('view','download','password_failure')
  http_status   int
}
```

### 2.8 AuditLog

```
AuditLog {
  id            UUID
  occurred_at   timestamptz
  actor_id      UUID nullable    -- nullable は公開リンク経由のため
  actor_kind    enum('user','public_viewer','system')
  action        text  -- 'file.upload', 'file.update', 'file.delete', 'file.restore', 'file.purge', 'file.rename', 'file.move', 'share.create', 'share.revoke', 'auth.login', 'auth.logout', 'auth.password_change', etc.
  target_kind   text  -- 'file', 'folder', 'share_link', 'user'
  target_id     UUID nullable
  details_json  jsonb -- 操作固有の追加情報（before / after）
  ip_addr       inet nullable
  user_agent    string nullable
  irreversible  bool default false
}
```

INSERT のみ。UPDATE / DELETE はアプリケーションロールに付与しない。

### 2.9 UploadSession（tus.io レジューム用）

```
UploadSession {
  id            UUID  (URL に露出)
  owner_id      UUID
  parent_folder_id UUID nullable
  filename      string
  size_total    bigint
  size_received bigint default 0
  storage_key   string  -- /var/data/.../tmp/{upload_uuid}.part
  if_match      string nullable  -- 上書き対象の version_id
  if_none_match string nullable  -- 新規時は '*'
  created_at    timestamptz
  expires_at    timestamptz  -- 7 日
  completed_at  timestamptz nullable
}
```

### 2.10 Session

```
Session {
  id            UUID  (Cookie に格納する値の元、HMAC 署名済み）
  user_id       UUID
  created_at    timestamptz
  last_seen_at  timestamptz
  expires_at    timestamptz
  ip_addr       inet
  user_agent    string
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

## 4. PostgreSQL スキーマ（正準）

最終的なマイグレーション SQL は `migrations/` に置くが、設計時点での参照スキーマを以下に示す。

```sql
-- 拡張
CREATE EXTENSION IF NOT EXISTS pgcrypto;     -- gen_random_uuid()
CREATE EXTENSION IF NOT EXISTS pg_trgm;      -- 検索

-- ユーザ
CREATE TABLE users (
  id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  email               TEXT NOT NULL UNIQUE,
  password_hash       TEXT NOT NULL,
  totp_secret_enc     BYTEA,                  -- AES-GCM で暗号化済み
  totp_enabled        BOOLEAN NOT NULL DEFAULT false,
  recovery_codes_hash TEXT[]  NOT NULL DEFAULT '{}',
  created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_login_at       TIMESTAMPTZ,
  locked_until        TIMESTAMPTZ,
  failed_login_count  INT NOT NULL DEFAULT 0
);

-- フォルダ
CREATE TABLE folders (
  id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  owner_id         UUID NOT NULL REFERENCES users(id),
  parent_folder_id UUID REFERENCES folders(id) ON DELETE RESTRICT,
  name             TEXT NOT NULL,
  path             TEXT NOT NULL,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  deleted_at       TIMESTAMPTZ,
  UNIQUE (owner_id, path)
);
CREATE INDEX idx_folders_owner_path ON folders (owner_id, path);
CREATE INDEX idx_folders_parent ON folders (parent_folder_id);

-- ファイル
CREATE TABLE files (
  id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  owner_id           UUID NOT NULL REFERENCES users(id),
  parent_folder_id   UUID REFERENCES folders(id) ON DELETE RESTRICT,
  name               TEXT NOT NULL,
  path               TEXT NOT NULL,
  current_version_id UUID,                     -- file_versions(id) 後で FK
  size_bytes         BIGINT NOT NULL,
  content_type       TEXT,
  storage_key        TEXT NOT NULL,
  sha256             BYTEA NOT NULL,           -- 32 bytes
  encryption_key_id  UUID NOT NULL,
  state              TEXT NOT NULL CHECK (state IN ('draft','active','trashed','purged','gone')),
  created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
  deleted_at         TIMESTAMPTZ,
  UNIQUE (owner_id, path)
    DEFERRABLE INITIALLY IMMEDIATE
);
CREATE INDEX idx_files_owner_path ON files (owner_id, path);
CREATE INDEX idx_files_owner_state ON files (owner_id, state);
CREATE INDEX idx_files_deleted_at ON files (deleted_at) WHERE state = 'trashed';
CREATE INDEX idx_files_name_trgm ON files USING gin (name gin_trgm_ops);

-- ファイルバージョン
CREATE TABLE file_versions (
  id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  file_id            UUID NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  version_number     INT  NOT NULL,
  size_bytes         BIGINT NOT NULL,
  sha256             BYTEA NOT NULL,
  storage_key        TEXT NOT NULL,
  s3_version_id      TEXT,
  encryption_key_id  UUID NOT NULL,
  created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
  created_by_session_id UUID,
  deleted_by_user    BOOLEAN NOT NULL DEFAULT false,
  UNIQUE (file_id, version_number)
);
CREATE INDEX idx_file_versions_file ON file_versions (file_id, version_number DESC);

ALTER TABLE files
  ADD CONSTRAINT fk_files_current_version
  FOREIGN KEY (current_version_id) REFERENCES file_versions(id)
  DEFERRABLE INITIALLY DEFERRED;

-- タグ
CREATE TABLE tags (
  id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  owner_id   UUID NOT NULL REFERENCES users(id),
  name       TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (owner_id, name)
);

CREATE TABLE file_tags (
  file_id UUID NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  tag_id  UUID NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
  PRIMARY KEY (file_id, tag_id)
);

-- 共有リンク
CREATE TABLE share_links (
  id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  file_id          UUID NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  created_by       UUID NOT NULL REFERENCES users(id),
  password_hash    TEXT,
  expires_at       TIMESTAMPTZ,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  revoked_at       TIMESTAMPTZ,
  view_count       BIGINT NOT NULL DEFAULT 0,
  download_count   BIGINT NOT NULL DEFAULT 0
);
CREATE INDEX idx_share_links_file ON share_links (file_id);
CREATE INDEX idx_share_links_active ON share_links (file_id) WHERE revoked_at IS NULL;

CREATE TABLE share_link_accesses (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  share_link_id UUID NOT NULL REFERENCES share_links(id) ON DELETE CASCADE,
  ip_addr       INET NOT NULL,
  user_agent    TEXT,
  accessed_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  action        TEXT NOT NULL CHECK (action IN ('view','download','password_failure')),
  http_status   INT
);
CREATE INDEX idx_share_link_accesses_link ON share_link_accesses (share_link_id, accessed_at DESC);

-- 監査ログ（INSERT only）
CREATE TABLE audit_logs (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  occurred_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  actor_id      UUID,
  actor_kind    TEXT NOT NULL CHECK (actor_kind IN ('user','public_viewer','system')),
  action        TEXT NOT NULL,
  target_kind   TEXT NOT NULL,
  target_id     UUID,
  details_json  JSONB NOT NULL DEFAULT '{}',
  ip_addr       INET,
  user_agent    TEXT,
  irreversible  BOOLEAN NOT NULL DEFAULT false
);
CREATE INDEX idx_audit_logs_actor_time ON audit_logs (actor_id, occurred_at DESC);
CREATE INDEX idx_audit_logs_target ON audit_logs (target_kind, target_id, occurred_at DESC);

-- 権限分離（マイグレーションで実行）
REVOKE UPDATE, DELETE ON audit_logs FROM PUBLIC;
-- アプリ用ロール（後述）に INSERT / SELECT のみ付与

-- アップロードセッション
CREATE TABLE upload_sessions (
  id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  owner_id         UUID NOT NULL REFERENCES users(id),
  parent_folder_id UUID REFERENCES folders(id),
  filename         TEXT NOT NULL,
  size_total       BIGINT NOT NULL,
  size_received    BIGINT NOT NULL DEFAULT 0,
  storage_key      TEXT NOT NULL,
  if_match         TEXT,
  if_none_match    TEXT,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at       TIMESTAMPTZ NOT NULL,
  completed_at     TIMESTAMPTZ
);
CREATE INDEX idx_upload_sessions_expiry ON upload_sessions (expires_at) WHERE completed_at IS NULL;

-- セッション
CREATE TABLE sessions (
  id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at   TIMESTAMPTZ NOT NULL,
  ip_addr      INET,
  user_agent   TEXT
);
CREATE INDEX idx_sessions_user ON sessions (user_id);
CREATE INDEX idx_sessions_expiry ON sessions (expires_at);

-- レート制限（IP / アカウント単位）
CREATE TABLE rate_limit_buckets (
  bucket_key   TEXT PRIMARY KEY,         -- 例 'login:ip:203.0.113.1' or 'login:user:abc'
  tokens       DOUBLE PRECISION NOT NULL,
  refilled_at  TIMESTAMPTZ NOT NULL,
  expires_at   TIMESTAMPTZ NOT NULL
);
```

### 4.1 DB ロール分離

```sql
-- 通常アプリロール
CREATE ROLE sync_app LOGIN PASSWORD '<from secrets manager>';
GRANT CONNECT ON DATABASE sync TO sync_app;
GRANT USAGE ON SCHEMA public TO sync_app;
GRANT SELECT, INSERT, UPDATE, DELETE
  ON ALL TABLES IN SCHEMA public TO sync_app;
-- ただし audit_logs は INSERT/SELECT のみ
REVOKE UPDATE, DELETE ON audit_logs FROM sync_app;

-- マイグレーションロール（DDL 専用）
CREATE ROLE sync_migrate LOGIN PASSWORD '<from secrets manager>';
GRANT CREATE ON SCHEMA public TO sync_migrate;
GRANT ALL ON ALL TABLES IN SCHEMA public TO sync_migrate;
```

## 5. S3 Files 上のファイル配置規則

```
/var/data/                                      ← S3 Files マウントポイント
├── owner-{user_uuid}/
│   ├── current/
│   │   └── {file_uuid}                         ← 現行版の暗号文
│   ├── trash/
│   │   └── {file_uuid}                         ← 旧設計の名残（v1 では使用しない）
│   ├── tmp/
│   │   └── {upload_session_uuid}.part          ← アップロード中の一時
│   └── versions/
│       └── {file_uuid}/
│           ├── v1
│           ├── v2
│           └── ...
└── _system/
    ├── kek/                                    ← 鍵階層 (07-security 参照)
    └── healthcheck.txt                          ← S3 Files 動作確認用
```

設計ポイント：

- **ファイル名は UUID 固定**：リネームを O(1) で（DB 上の `path` と `name` だけ更新、S3 のキーは不変）
- **ソフト削除中もファイル本体は `current/` に置いたまま**：`deleted_at` を立てるだけ
- **物理削除（purge）時のみ S3 オブジェクトに DeleteMarker を付与**
- **バージョンは `versions/{file_uuid}/v{N}` に明示的に保存**：S3 バケットバージョニングと二重管理になるが、明示的なほうが障害解析時に追いやすい
- 注意: S3 Files は POSIX ライクなので `os.Rename` は同一ファイルシステム内で原子的に動作するはず（NFS v4.1+ 規定）。ただしクラッシュ時の挙動は要検証（[`13-risks-and-open-questions.md`](./13-risks-and-open-questions.md) §3 参照）

## 6. メタデータと S3 Files の整合性

ファイル本体（S3 Files）とメタデータ（RDS）は **2 種類のリソース** で、Postgres トランザクションでは原子的に変更できない。本設計では以下のルールで整合性を保つ：

### 6.1 書き込み順（commit-after-write）

```
1. Acquire OCC (SELECT current version + check If-Match)
2. Write encrypted bytes to /var/data/.../tmp/{upload_uuid}
3. fsync(2) ※ NFS v4.1+ では best-effort であることに注意
4. os.Rename(tmp → current/{file_uuid})  -- 原子的、同 FS 内
5. BEGIN; INSERT file_versions; UPDATE files SET current_version_id = new; INSERT audit_log; COMMIT;
6. （DB COMMIT 失敗時）孤児ファイルを掃除する補正ジョブ（後述 §6.3）
```

### 6.2 読み取り

```
1. SELECT files WHERE id = ? AND state = 'active'
2. open(/var/data/.../current/{file_uuid})  -- AES-GCM 復号して stream
```

### 6.3 補正ジョブ（reconciliation）

- **孤児ファイル検出**: 1 日 1 回、`/var/data/owner-*/current/` を走査し、対応する `files` レコードがないものを `_orphan/` へ移動（即削除はしない）
- **メタデータ孤児検出**: `files` テーブルで `state = 'active'` だが S3 Files 上のオブジェクトが存在しないものを検出してアラート
- **`upload_sessions.expires_at < now()` のクリーンアップ**: 1 時間ごとに走査し、`tmp/*.part` を削除

## 7. ストアドプロシージャ vs アプリ層

設計判断：**ストアドプロシージャは使わない**。理由：

- Go ですべてのロジックを書ける
- DB 移植性を維持（Postgres ロックインだが、極端なベンダー機能には依存しない）
- 監査・テスト容易性

例外として、`audit_logs` への INSERT を `BEFORE` トリガで強制する案は **不採用**。アプリ側で必ず INSERT する責務を負う（テストでカバー）。

## 8. 不変条件の DB 制約への落とし込み

| 不変条件 | DB 制約 |
|---|---|
| INV-1 物理削除は二段階 | `state` の CHECK 制約 + アプリ側の遷移ロジック |
| INV-2 書き込みは累積 | `file_versions` への INSERT を強制（アプリ層） + S3 バージョニング ON |
| INV-4 未完了は本番に反映しない | `state = 'draft'` 状態を経由する。`upload_sessions` が COMMIT 後にのみ `files` レコードを作る |
| INV-5 破壊的操作の確認 | UI 側 + サーバ側の二段確認（強制上書き等） |

## 9. 命名規則

- テーブル名は複数形 snake_case (`files`, `file_versions`)
- 列名は snake_case
- 主キーは `id`（UUID v4）
- 外部キーは `<entity>_id`
- タイムスタンプは `timestamptz`、サーバ DB は UTC で保存
- ENUM 風の列は CHECK 制約 + TEXT で表現（PostgreSQL の ENUM 型は移行しづらいため不採用）

---

次の章: [`04-sync-semantics.md`](./04-sync-semantics.md)
