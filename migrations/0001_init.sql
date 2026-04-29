-- sync-files-go 初期スキーマ (MySQL 8.0+)
-- 設計書 docs/03-domain-model.md §4 を正準とする

-- 文字セット強制
SET sql_mode = 'STRICT_ALL_TABLES,NO_ENGINE_SUBSTITUTION,NO_ZERO_DATE,NO_ZERO_IN_DATE';

-- ユーザ
CREATE TABLE IF NOT EXISTS users (
  id_bin              BINARY(16) PRIMARY KEY,
  email               VARCHAR(320) NOT NULL,
  password_hash       VARCHAR(255) NOT NULL,
  totp_secret_enc     VARBINARY(256),
  totp_secret_header  VARBINARY(64),
  totp_enabled        TINYINT(1) NOT NULL DEFAULT 0,
  recovery_codes_hash JSON NOT NULL,
  kek_enc             VARBINARY(80) NOT NULL,
  kek_id_bin          BINARY(16) NOT NULL,
  master_key_version  INT NOT NULL DEFAULT 1,
  created_at          DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  last_login_at       DATETIME(6),
  locked_until        DATETIME(6),
  failed_login_count  INT NOT NULL DEFAULT 0,
  UNIQUE KEY uniq_users_email (email)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- フォルダ
CREATE TABLE IF NOT EXISTS folders (
  id_bin                BINARY(16) PRIMARY KEY,
  owner_id_bin          BINARY(16) NOT NULL,
  parent_folder_id_bin  BINARY(16),
  name                  VARCHAR(255) NOT NULL,
  path                  VARCHAR(2048) NOT NULL,
  path_hash             VARBINARY(32) NOT NULL,
  created_at            DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  deleted_at            DATETIME(6),
  KEY idx_folders_owner_path_hash (owner_id_bin, path_hash),
  KEY idx_folders_parent          (parent_folder_id_bin),
  CONSTRAINT fk_folders_owner FOREIGN KEY (owner_id_bin) REFERENCES users(id_bin)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- ファイル (論理エンティティ)
CREATE TABLE IF NOT EXISTS files (
  id_bin                  BINARY(16) PRIMARY KEY,
  owner_id_bin            BINARY(16) NOT NULL,
  parent_folder_id_bin    BINARY(16),
  name                    VARCHAR(255) NOT NULL,
  path                    VARCHAR(2048) NOT NULL,
  path_hash               VARBINARY(32) NOT NULL,
  current_version_id_bin  BINARY(16),
  size_bytes              BIGINT NOT NULL,
  content_type            VARCHAR(255),
  sha256                  VARBINARY(32) NOT NULL,
  state                   ENUM('draft','active','trashed','purged','gone') NOT NULL,
  created_at              DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  updated_at              DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  deleted_at              DATETIME(6),
  -- CR-2: 同名 active 一意性を生成列 + UNIQUE で守る
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
CREATE TABLE IF NOT EXISTS file_versions (
  id_bin                    BINARY(16) PRIMARY KEY,
  file_id_bin               BINARY(16) NOT NULL,
  version_number            INT NOT NULL,
  size_bytes                BIGINT NOT NULL,
  sha256                    VARBINARY(32) NOT NULL,
  storage_key               VARCHAR(512) NOT NULL,
  s3_version_id             VARCHAR(128),
  dek_enc                   VARBINARY(80) NOT NULL,
  kek_id_bin                BINARY(16) NOT NULL,
  encryption_scheme         VARCHAR(64) NOT NULL,
  encryption_header         VARBINARY(64) NOT NULL,
  created_at                DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  created_by_session_id_bin BINARY(16),
  deleted_by_user           TINYINT(1) NOT NULL DEFAULT 0,
  UNIQUE KEY uniq_file_versions (file_id_bin, version_number),
  KEY idx_file_versions_file (file_id_bin, version_number DESC),
  CONSTRAINT fk_file_versions_file FOREIGN KEY (file_id_bin) REFERENCES files(id_bin) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- タグ
CREATE TABLE IF NOT EXISTS tags (
  id_bin       BINARY(16) PRIMARY KEY,
  owner_id_bin BINARY(16) NOT NULL,
  name         VARCHAR(64) NOT NULL,
  created_at   DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  UNIQUE KEY uniq_tags_owner_name (owner_id_bin, name),
  CONSTRAINT fk_tags_owner FOREIGN KEY (owner_id_bin) REFERENCES users(id_bin)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS file_tags (
  file_id_bin BINARY(16) NOT NULL,
  tag_id_bin  BINARY(16) NOT NULL,
  PRIMARY KEY (file_id_bin, tag_id_bin),
  CONSTRAINT fk_ft_file FOREIGN KEY (file_id_bin) REFERENCES files(id_bin) ON DELETE CASCADE,
  CONSTRAINT fk_ft_tag  FOREIGN KEY (tag_id_bin)  REFERENCES tags(id_bin)  ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- 共有リンク (token は base64url 32 bytes、DB は SHA-256(token))
CREATE TABLE IF NOT EXISTS share_links (
  id_bin           BINARY(16) PRIMARY KEY,
  file_id_bin      BINARY(16) NOT NULL,
  created_by_bin   BINARY(16) NOT NULL,
  token_hash       VARBINARY(32) NOT NULL,
  password_hash    VARCHAR(255),
  expires_at       DATETIME(6) NOT NULL,
  created_at       DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  revoked_at       DATETIME(6),
  view_count       BIGINT NOT NULL DEFAULT 0,
  download_count   BIGINT NOT NULL DEFAULT 0,
  UNIQUE KEY uniq_share_links_token (token_hash),
  KEY idx_share_links_file   (file_id_bin),
  KEY idx_share_links_active (file_id_bin, revoked_at, expires_at),
  CONSTRAINT fk_share_links_file FOREIGN KEY (file_id_bin) REFERENCES files(id_bin) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS share_link_accesses (
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

-- 監査ログ (INSERT only)
CREATE TABLE IF NOT EXISTS audit_logs (
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

-- アップロードセッション (tus.io)
CREATE TABLE IF NOT EXISTS upload_sessions (
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
CREATE TABLE IF NOT EXISTS sessions (
  id_bin       BINARY(16) PRIMARY KEY,
  user_id_bin  BINARY(16) NOT NULL,
  created_at   DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  last_seen_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  expires_at   DATETIME(6) NOT NULL,
  ip_addr      VARBINARY(16),
  user_agent   VARCHAR(512),
  KEY idx_sessions_user   (user_id_bin),
  KEY idx_sessions_expiry (expires_at),
  CONSTRAINT fk_sessions_user FOREIGN KEY (user_id_bin) REFERENCES users(id_bin) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- レート制限
CREATE TABLE IF NOT EXISTS rate_limit_buckets (
  bucket_key   VARCHAR(255) PRIMARY KEY,
  tokens       DOUBLE NOT NULL,
  refilled_at  DATETIME(6) NOT NULL,
  expires_at   DATETIME(6) NOT NULL,
  KEY idx_rate_limit_expiry (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
