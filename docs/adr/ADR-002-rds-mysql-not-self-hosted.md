# ADR-002: メタデータ DB は RDS for MySQL（Primary + Read Replica）。自前構築・SQLite を不採用

## ステータス

採択 (2026-04-29)。当初は PostgreSQL を採択していたが、ユーザ指示により MySQL に変更。

## コンテキスト

メタデータ（ファイル一覧、バージョン、監査ログ、セッション、共有リンク等）の永続化先として、4 通りを検討：

1. RDS for PostgreSQL（Multi-AZ）
2. **RDS for MySQL 8.x（Primary + Read Replica）** ← 採択
3. SQLite を S3 Files (NFS) 上に保存
4. PostgreSQL コンテナを ECS 上で同居起動 + S3 Files 上にデータ

## 検討内容

### 選択肢 A: RDS for PostgreSQL

最初の検討で採択していた。Go の `database/sql` + `pgx` ドライバ、`pg_trgm` でのファイル名検索、`gen_random_uuid()`、`JSONB`、配列型などが扱いやすい。

### 選択肢 B: RDS for MySQL 8.x + Read Replica ← **採択**

利点：
- 「Primary を守る」設計（[参考記事](../../articles_reference/mysql-read-replica-write-ahead-htmx-go.md)）と直結
- Primary は書き込みと read-after-write、Replica は read-heavy 操作（一覧、検索、アクティビティ）に分離
- アプリ層で `DBRouter` を実装し、`forcePrimary(ctx)` でリクエスト単位に切替
- MySQL 8.0 で `CHECK` 制約・`JSON`・`UUID()` 関数・FULLTEXT INDEX などが揃っている
- Go の `database/sql` + 標準的な MySQL ドライバ（go-sql-driver/mysql）で完結

欠点：
- PostgreSQL に比べて：
  - 配列型が無い（`text[]` 等を使えない → JSON で代替）
  - `pg_trgm` 相当はないが、FULLTEXT INDEX やプレフィックス前方一致で代替可能
  - 部分インデックス（条件付き UNIQUE）が無い → 別カラム + アプリ側ロジックで担保
- DEFERRABLE 制約が無い → トランザクション設計を見直す
- TIMESTAMPTZ が無い → `DATETIME` + アプリ側で UTC 統一

### 選択肢 C: SQLite on NFS

利点：シンプル

欠点：NFS 上の SQLite はロック不整合・耐久性問題が知られており、本設計の不変条件と矛盾。

### 選択肢 D: PostgreSQL/MySQL コンテナを ECS 上で同居 + S3 Files 上にデータ

利点：コスト最小

欠点：DB の `fsync` 信頼性が NFS 上では弱く、データ破損リスク。「ファイル損失防止」要件と本質的に矛盾。

## 決定

**RDS for MySQL 8.x（Primary db.t4g.micro Multi-AZ + Read Replica db.t4g.micro × 1）** を採択。

- アプリ層に `DBRouter` を実装し、Reader / Writer を明示的に分離
- `forcePrimary(ctx)` で read-after-write 用途に Primary を強制
- 接続プール: `database/sql` の `SetMaxOpenConns(50)`、`SetMaxIdleConns(25)` を最低ライン
- 重い管理画面 SQL は `context.WithTimeout` で短く制限
- アプリ内メモリキャッシュは「カテゴリ・タグ等の更新頻度低めの値」のみに限定

## 帰結

- 03-domain-model.md のスキーマを MySQL 構文に書き直し
- `gen_random_uuid()` → `UUID_TO_BIN(UUID())` または アプリ側で UUID v4 生成
- `BYTEA` → `VARBINARY(N)` / `BLOB`
- `INET` → `VARBINARY(16)`（IPv6 互換）
- `JSONB` → `JSON`
- `TIMESTAMPTZ` → `DATETIME(6)` + アプリ側で UTC 統一（タイムゾーン管理は app の責務）
- 部分 UNIQUE INDEX（`WHERE state='active'`）→ 「アクティブ専用テーブル」または「アクティブフラグ列」+ アプリ側で実装
- バージョン履歴は v1 では「同一ファイル名の active 重複は禁止」をアプリ側で担保

## リンク

- [`03-domain-model.md`](../03-domain-model.md)
- [Primary を守る考え方（参考記事）](../../articles_reference/mysql-read-replica-write-ahead-htmx-go.md)
