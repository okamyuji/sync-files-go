# ADR-008: MySQL の読み書き分離（Primary Write / Read Replica）と DBRouter 設計

## ステータス

採択 (2026-04-29)

## コンテキスト

ユーザの Zenn 記事「OpenAI 規模でなくても RDB 設計は Primary を守る考え方でだいたい対応できる」（[参考](../../articles_reference/mysql-read-replica-write-ahead-htmx-go.md)）の方針に従い、本システムでも：

- Primary は書き込み + read-after-write が必要な読み取りに集中させる
- Read Replica は古い読み取りを許容できる処理（一覧、検索、過去版表示、共有リンク履歴など）を受け持つ
- アプリ層の `DBRouter` で接続先を判断し、`forcePrimary(ctx)` で個別制御

を採用する。

## 決定

### Replica へ寄せる読み取り

| 画面 / 処理 | 接続先 | 理由 |
|---|---|---|
| ファイル一覧 | Replica | 数秒古くて問題ない |
| ファイル検索 | Replica | 最新性より応答速度優先 |
| アクティビティタイムライン | Replica | 過去ログのみで read-after-write 不要 |
| ゴミ箱一覧 | Replica | 同上 |
| 過去版一覧 | Replica | 同上 |
| 公開リンクのアクセス履歴 | Replica | 同上 |
| 公開リンク経由のファイル取得 | Replica | 公開後に多少古くても可 |

### Primary に残す読み取り

| 画面 / 処理 | 接続先 | 理由 |
|---|---|---|
| アップロード時の OCC チェック | Primary | 強整合性が必要 |
| ダウンロード（自分のアップロード直後） | Primary（read-after-write window） | 自分の書き込みを直後に見るユースケース |
| 認証セッション照合 | Primary | 整合性最優先 |
| 公開リンク作成 / 取り消し | Primary | 書き込みフロー |

### 書き込み

すべて Primary。これは絶対。

### read-after-write window

- 書き込み完了後、context に `withReadAfterWrite(ctx)` で 5 秒の有効期限を埋め込む
- この期間内は `forcePrimary(ctx) = true` とし、Reader 接続でも Primary に向かわせる
- 5 秒は Replica 遅延の通常値から決定（実測で見直し）

### DBRouter の Go コード骨子

```go
type DBRouter struct {
    primary *sql.DB
    replica *sql.DB
    // Replica 不調時の縮退運転フラグ
    replicaDegraded atomic.Bool
}

type readAfterWriteUntilKey struct{}

func (r *DBRouter) Writer(ctx context.Context) *sql.DB {
    return r.primary
}

func (r *DBRouter) Reader(ctx context.Context) *sql.DB {
    if r.forcePrimary(ctx) || r.replicaDegraded.Load() {
        return r.primary
    }
    return r.replica
}

func (r *DBRouter) forcePrimary(ctx context.Context) bool {
    until, ok := ctx.Value(readAfterWriteUntilKey{}).(time.Time)
    return ok && time.Now().Before(until)
}

func WithReadAfterWrite(ctx context.Context) context.Context {
    return context.WithValue(ctx, readAfterWriteUntilKey{}, time.Now().Add(5*time.Second))
}
```

### 接続プール

```go
func configureDB(db *sql.DB) {
    db.SetMaxOpenConns(50)
    db.SetMaxIdleConns(25)
    db.SetConnMaxLifetime(30 * time.Minute)
    db.SetConnMaxIdleTime(5 * time.Minute)
}
```

Primary と Replica で別 `*sql.DB`。それぞれ上記設定。

### 縮退運転

- Replica の遅延が 10 秒を超えたら、`replicaDegraded` を true に設定
- すべての読み取りを Primary に寄せる（一時的）
- アクセスが多い管理画面・全文検索のような重い読み取りはレート制限・タイムアウトで Primary を保護
- 平時に戻ったら自動復帰（遅延 < 1 秒）

### キャッシュ戦略

「Redis を入れる前に MySQL でできることをやる」という記事の方針に従い、v1 では：

- アプリ内メモリキャッシュ：タグ一覧、ユーザ設定、ETag マッチ用ヘッダのみ
- 短 TTL（30 秒）+ singleflight 風の cache stampede 対策
- 外部キャッシュは v2 以降で検討

### 集計テーブル

「ファイル数」「総容量」のような集計は v1 では SQL 都度集計。アクセス増・遅延増があれば日次バッチで集計テーブル化（v2）。

## 帰結

- 03-domain-model.md：MySQL 構文に書き換え + DBRouter / Reader-Writer 設計を追加
- 02-architecture.md：構成図に Read Replica を追加
- 09-infrastructure-and-deployment.md：RDS Multi-AZ + Read Replica の Terraform を追加
- 10-operations.md：Replica 遅延の監視と縮退運転の Runbook を追加
- 11-testing-strategy.md：DBRouter のルーティングテストを必須化
- 06-data-loss-prevention.md：「Replica 遅延中の OCC 検査は Primary」を明示

## リンク

- [Primary を守る考え方（参考記事）](../../articles_reference/mysql-read-replica-write-ahead-htmx-go.md)
- [`03-domain-model.md`](../03-domain-model.md)
- [OpenAI Scaling PostgreSQL](https://openai.com/index/scaling-postgresql/)
