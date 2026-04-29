package repo

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// stubDB は DBRouter のルーティングだけを検証するため、具体的な SQL は実行しないスタブ。
type stubDB struct{ name string }

func (s *stubDB) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
	return nil, nil
}
func (s *stubDB) QueryRowContext(context.Context, string, ...any) *sql.Row { return nil }
func (s *stubDB) ExecContext(context.Context, string, ...any) (sql.Result, error) {
	return nil, nil
}
func (s *stubDB) BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error) { return nil, nil }
func (s *stubDB) PingContext(context.Context) error                       { return nil }

// TestDBRouter_Routing は ADR-008 の中核ルーティング 4 ケースを検証する。
//
// 1. Writer は常に Primary
// 2. 通常時 Reader は Replica
// 3. RAW window 中の Reader は Primary
// 4. Replica 縮退中の Reader は Primary
func TestDBRouter_Routing(t *testing.T) {
	primary := &stubDB{name: "primary"}
	replica := &stubDB{name: "replica"}
	r := NewDBRouter(primary, replica)

	ctx := context.Background()

	// 1. Writer は常に Primary
	if r.Writer(ctx) != primary {
		t.Fatal("Writer should always be Primary")
	}

	// 2. 通常時の Reader は Replica
	if r.Reader(ctx) != replica {
		t.Fatal("Reader should be Replica in normal mode")
	}

	// 3. RAW window 中は Primary に強制
	rawCtx := WithReadAfterWrite(ctx, time.Now().Add(5*time.Second))
	if r.Reader(rawCtx) != primary {
		t.Fatal("Reader during RAW window should be Primary")
	}

	// 期限切れ RAW window では Replica に戻る
	expiredCtx := WithReadAfterWrite(ctx, time.Now().Add(-1*time.Second))
	if r.Reader(expiredCtx) != replica {
		t.Fatal("expired RAW window should fall back to Replica")
	}

	// 4. Replica 縮退中は Primary
	r.SetReplicaDegraded(true)
	if r.Reader(ctx) != primary {
		t.Fatal("Reader during replica degraded should be Primary")
	}
	if !r.IsReplicaDegraded() {
		t.Fatal("IsReplicaDegraded should be true")
	}

	r.SetReplicaDegraded(false)
	if r.Reader(ctx) != replica {
		t.Fatal("after recovery, Reader should be Replica again")
	}
}

// TestReadAfterWriteUntil はミドルウェアが値を取り出せることを確認する。
func TestReadAfterWriteUntil(t *testing.T) {
	ctx := context.Background()
	if _, ok := ReadAfterWriteUntil(ctx); ok {
		t.Fatal("empty ctx should not have RAW until")
	}

	want := time.Now().Add(3 * time.Second)
	ctx = WithReadAfterWrite(ctx, want)
	got, ok := ReadAfterWriteUntil(ctx)
	if !ok || !got.Equal(want) {
		t.Fatalf("want %v ok got %v ok=%v", want, got, ok)
	}
}
