// Package repo は永続化層の interface と DBRouter を提供する。
//
// DBRouter は設計書 ADR-008 / 02-architecture.md §3.5 を実装する：
//   - Writer は常に Primary
//   - Reader は通常 Replica、ただし以下のケースは Primary に切り替える:
//   - RAW (Read-After-Write) window 内 (5 秒、HTTP リクエストを跨ぐ。HMAC 署名 Cookie で伝播)
//   - Replica 縮退運転中 (replicaDegraded フラグ)
package repo

import (
	"context"
	"database/sql"
	"sync/atomic"
	"time"
)

// DB database/sql の *sql.DB を抽象化する小さい interface。
// Reader/Writer どちらでも実装できる必要のあるメソッドだけ定義する。
type DB interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
	PingContext(ctx context.Context) error
}

// DBRouter は読み書き分離のためのルーティングコンポーネント。
type DBRouter struct {
	primary DB
	replica DB
	// Replica 遅延が閾値超過したときに true。アトミックに切替可能。
	replicaDegraded atomic.Bool
}

// NewDBRouter Primary/Replica を受け取って DBRouter を構築する。
func NewDBRouter(primary, replica DB) *DBRouter {
	return &DBRouter{primary: primary, replica: replica}
}

// Writer は常に Primary を返す。
func (r *DBRouter) Writer(ctx context.Context) DB {
	return r.primary
}

// Reader は通常 Replica、RAW window 中または Replica 縮退中は Primary を返す。
func (r *DBRouter) Reader(ctx context.Context) DB {
	if r.forcePrimary(ctx) || r.replicaDegraded.Load() {
		return r.primary
	}
	return r.replica
}

// SetReplicaDegraded Replica 不調時に true、復旧時に false。
// Phase 3 で監視ループから呼ばれる。
func (r *DBRouter) SetReplicaDegraded(degraded bool) {
	r.replicaDegraded.Store(degraded)
}

// IsReplicaDegraded は現在の縮退状態を返す（メトリクス用）。
func (r *DBRouter) IsReplicaDegraded() bool {
	return r.replicaDegraded.Load()
}

// readAfterWriteUntilKey RAW window の context キー。
type readAfterWriteUntilKey struct{}

// WithReadAfterWrite ctx に「until 時刻まで Reader を Primary に強制する」マーカーを埋める。
// HTTP リクエスト間で伝播させるには、加えて HMAC 署名 Cookie が必要（HIGH 修正）。
// 設計書 ADR-008 を参照。
func WithReadAfterWrite(ctx context.Context, until time.Time) context.Context {
	return context.WithValue(ctx, readAfterWriteUntilKey{}, until)
}

// ReadAfterWriteUntil ctx に埋まった until 時刻を取り出す（middleware が cookie 検証で使う）。
func ReadAfterWriteUntil(ctx context.Context) (time.Time, bool) {
	t, ok := ctx.Value(readAfterWriteUntilKey{}).(time.Time)
	return t, ok
}

func (r *DBRouter) forcePrimary(ctx context.Context) bool {
	until, ok := ctx.Value(readAfterWriteUntilKey{}).(time.Time)
	return ok && time.Now().Before(until)
}
