//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/okamyuji/sync-files-go/internal/domain"
	"github.com/okamyuji/sync-files-go/internal/repo/mysql"
)

// TestSchema_ActiveMarkerUNIQUE CR-2: 同名 active 一意性が DB 制約で守られることを確認。
//
// 同一 owner + parent_folder + name で state='active' の 2 行目を INSERT すると、
// MySQL の `uniq_files_active_name` UNIQUE が拒否すること。
func TestSchema_ActiveMarkerUNIQUE(t *testing.T) {
	env := SetupEnv(t)
	u := MakeUser(t, env)
	ctx := context.Background()
	now := time.Now().UTC()

	first := &domain.File{
		ID:        uuid.New(),
		OwnerID:   u.ID,
		Name:      "report.docx",
		Path:      "/report.docx",
		SizeBytes: 100,
		SHA256:    bytesOf32(0xaa),
		State:     domain.FileStateActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := env.Files.Insert(ctx, first); err != nil {
		t.Fatalf("insert first: %v", err)
	}

	dup := &domain.File{
		ID:        uuid.New(), // 別 file_id だが、同名で active
		OwnerID:   u.ID,
		Name:      "report.docx",
		Path:      "/report.docx",
		SizeBytes: 200,
		SHA256:    bytesOf32(0xbb),
		State:     domain.FileStateActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
	err := env.Files.Insert(ctx, dup)
	if err == nil {
		t.Fatal("CR-2: 同名 active の 2 行目が拒否されるべき（UNIQUE 違反期待）")
	}
	if !isDuplicateEntryError(err) {
		t.Fatalf("expected MySQL duplicate entry error, got: %v", err)
	}
	t.Logf("UNIQUE 違反として正しく拒否: %v", err)
}

// TestSchema_TrashedAllowsDuplicate trashed 状態の重複は許される（active_marker が NULL になるため）。
func TestSchema_TrashedAllowsDuplicate(t *testing.T) {
	env := SetupEnv(t)
	u := MakeUser(t, env)
	ctx := context.Background()
	now := time.Now().UTC()

	// 1) trashed の同名ファイルが複数あっても OK
	for i := 0; i < 3; i++ {
		f := &domain.File{
			ID:        uuid.New(),
			OwnerID:   u.ID,
			Name:      "shared-name.txt",
			Path:      "/shared-name.txt",
			SizeBytes: int64(100 * (i + 1)),
			SHA256:    bytesOf32(byte(0xC0 | i)),
			State:     domain.FileStateTrashed,
			CreatedAt: now,
			UpdatedAt: now,
		}
		dt := now
		f.DeletedAt = &dt
		if err := env.Files.Insert(ctx, f); err != nil {
			t.Fatalf("insert trashed #%d: %v", i, err)
		}
	}
	t.Log("trashed 状態の同名重複は UNIQUE 制約から除外される（NULL は重複扱いされない）")
}

// TestSchema_OwnerSeparation 別ユーザの同名 active は OK。
func TestSchema_OwnerSeparation(t *testing.T) {
	env := SetupEnv(t)
	u1 := MakeUser(t, env)
	u2 := MakeUser(t, env)
	ctx := context.Background()
	now := time.Now().UTC()

	for _, owner := range []*mysql.User{u1, u2} {
		f := &domain.File{
			ID:        uuid.New(),
			OwnerID:   owner.ID,
			Name:      "data.csv",
			Path:      "/data.csv",
			SizeBytes: 50,
			SHA256:    bytesOf32(0x77),
			State:     domain.FileStateActive,
			CreatedAt: now,
			UpdatedAt: now,
		}
		if err := env.Files.Insert(ctx, f); err != nil {
			t.Fatalf("insert for owner %s: %v", owner.ID, err)
		}
	}
}

// bytesOf32 同じバイトを 32 個並べた []byte を返す（テストデータ用）。
func bytesOf32(b byte) []byte {
	out := make([]byte, 32)
	for i := range out {
		out[i] = b
	}
	return out
}

// isDuplicateEntryError MySQL の DUP_ENTRY (1062) かどうか文字列で判定。
func isDuplicateEntryError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Duplicate entry") || strings.Contains(msg, "Error 1062")
}
