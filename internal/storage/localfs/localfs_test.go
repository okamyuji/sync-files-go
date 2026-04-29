package localfs

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/okamyuji/sync-files-go/internal/storage"
)

// TestRoundTrip_Immutable は CR-1 の中核：
//  1. tmp に書く
//  2. versions/{file_id}/{version_id} へ rename で確定
//  3. 同じキーへの 2 度目の確定は ErrAlreadyExists
func TestRoundTrip_Immutable(t *testing.T) {
	root := t.TempDir()
	st, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	ownerID := uuid.NewString()
	fileID := uuid.NewString()
	versionID := uuid.NewString()

	// 1. CreateTemp + Write + Close
	tw, err := st.CreateTemp(ctx, ownerID, "")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	if _, err := tw.Write([]byte("hello world")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 2. Finalize
	key, err := st.FinalizeVersion(ctx, ownerID, tw.UploadUUID(), fileID, versionID)
	if err != nil {
		t.Fatalf("FinalizeVersion: %v", err)
	}
	want := VersionStorageKey(ownerID, fileID, versionID)
	if key != want {
		t.Fatalf("storage_key want %s got %s", want, key)
	}

	// ファイルが期待パスにあるか
	abs := filepath.Join(root, "owner-"+ownerID, "versions", fileID, versionID)
	data, err := os.ReadFile(abs) // nolint:gosec // テスト
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "hello world" {
		t.Fatalf("contents: %q", string(data))
	}

	// 3. 同じ versions key へ 2 度目の Finalize は失敗
	tw2, err := st.CreateTemp(ctx, ownerID, "")
	if err != nil {
		t.Fatalf("CreateTemp2: %v", err)
	}
	_, _ = tw2.Write([]byte("garbage"))
	_ = tw2.Close()
	if _, err := st.FinalizeVersion(ctx, ownerID, tw2.UploadUUID(), fileID, versionID); !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("immutable key should reject second finalize, got %v", err)
	}
}

// TestOpenVersion_NotFound は存在しないバージョンに対して ErrNotFound を返すことを確認。
func TestOpenVersion_NotFound(t *testing.T) {
	root := t.TempDir()
	st, _ := New(root)
	ctx := context.Background()

	_, err := st.OpenVersion(ctx, "ownerX", "fileX", "vX")
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("want ErrNotFound got %v", err)
	}
}

// TestOpenVersion_RoundTrip は読み取りができることを確認。
func TestOpenVersion_RoundTrip(t *testing.T) {
	root := t.TempDir()
	st, _ := New(root)
	ctx := context.Background()
	ownerID, fileID, versionID := "owner-abc", "file-xyz", "v-001"

	tw, _ := st.CreateTemp(ctx, ownerID, "")
	_, _ = io.WriteString(tw, "payload")
	_ = tw.Close()
	_, err := st.FinalizeVersion(ctx, ownerID, tw.UploadUUID(), fileID, versionID)
	if err != nil {
		t.Fatalf("FinalizeVersion: %v", err)
	}

	rc, err := st.OpenVersion(ctx, ownerID, fileID, versionID)
	if err != nil {
		t.Fatalf("OpenVersion: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "payload" {
		t.Fatalf("payload mismatch: %q", string(got))
	}
}
