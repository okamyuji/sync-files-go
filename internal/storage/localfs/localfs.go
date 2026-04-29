// Package localfs はローカルファイルシステム（または S3 Files NFS マウント）を裏に持つ Storage 実装。
//
// CR-1 修正：versions/{file_id}/{version_id} は immutable key。
// アップロードは tmp/{upload_uuid}.part に書いて os.Rename で確定。
package localfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/okamyuji/sync-files-go/internal/storage"
)

// Storage は os パッケージで実装した storage.Storage。
type Storage struct {
	root string // /var/data 想定
}

// New は root ディレクトリを指定する。存在しなければ MkdirAll する。
func New(root string) (*Storage, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir root %s: %w", root, err)
	}
	return &Storage{root: root}, nil
}

func (s *Storage) ownerDir(ownerID string) string {
	return filepath.Join(s.root, "owner-"+ownerID)
}

func (s *Storage) tmpPath(ownerID, uploadUUID string) string {
	return filepath.Join(s.ownerDir(ownerID), "tmp", uploadUUID+".part")
}

// VersionStorageKey はキーの正準形式を組み立てる（DB の storage_key 列に保存する値）。
func VersionStorageKey(ownerID, fileID, versionID string) string {
	return fmt.Sprintf("owner-%s/versions/%s/%s", ownerID, fileID, versionID)
}

func (s *Storage) versionPath(ownerID, fileID, versionID string) string {
	return filepath.Join(s.ownerDir(ownerID), "versions", fileID, versionID)
}

// CreateTemp は tmp/{upload_uuid}.part を作成して Writer を返す。
func (s *Storage) CreateTemp(ctx context.Context, ownerID, uploadUUID string) (storage.TempWriter, error) {
	if uploadUUID == "" {
		uploadUUID = uuid.NewString()
	}
	path := s.tmpPath(ownerID, uploadUUID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir tmp: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open temp: %w", err)
	}
	return &tempWriter{f: f, uploadUUID: uploadUUID, path: path}, nil
}

// FinalizeVersion は tmp → versions/{file_id}/{version_id} に原子的に rename する。
// 既存があれば ErrAlreadyExists を返す（immutable key の保証）。
func (s *Storage) FinalizeVersion(ctx context.Context, ownerID, uploadUUID, fileID, versionID string) (string, error) {
	src := s.tmpPath(ownerID, uploadUUID)
	dst := s.versionPath(ownerID, fileID, versionID)

	if _, err := os.Stat(dst); err == nil {
		return "", storage.ErrAlreadyExists
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", fmt.Errorf("mkdir versions: %w", err)
	}
	if err := os.Rename(src, dst); err != nil {
		return "", fmt.Errorf("rename tmp→version: %w", err)
	}
	return VersionStorageKey(ownerID, fileID, versionID), nil
}

// OpenVersion は versions/{file_id}/{version_id} を読み取り用に開く。
func (s *Storage) OpenVersion(ctx context.Context, ownerID, fileID, versionID string) (io.ReadSeekCloser, error) {
	path := s.versionPath(ownerID, fileID, versionID)
	f, err := os.Open(path) // nolint:gosec // path はサーバ側で組み立てた固定パス
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, storage.ErrNotFound
		}
		return nil, fmt.Errorf("open version: %w", err)
	}
	return f, nil
}

// RemoveVersion は purge バッチから呼ばれる。
func (s *Storage) RemoveVersion(ctx context.Context, ownerID, fileID, versionID string) error {
	path := s.versionPath(ownerID, fileID, versionID)
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return storage.ErrNotFound
		}
		return fmt.Errorf("remove version: %w", err)
	}
	return nil
}

// RemoveTemp は中断時の後片付け。存在しなければエラーにしない（冪等）。
func (s *Storage) RemoveTemp(ctx context.Context, ownerID, uploadUUID string) error {
	path := s.tmpPath(ownerID, uploadUUID)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove temp: %w", err)
	}
	return nil
}

// VersionExists は補正ジョブが orphan 検出に使う。
func (s *Storage) VersionExists(ctx context.Context, ownerID, fileID, versionID string) (bool, error) {
	_, err := os.Stat(s.versionPath(ownerID, fileID, versionID))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

type tempWriter struct {
	f          *os.File
	uploadUUID string
	path       string
	closed     atomic.Bool
}

func (w *tempWriter) Write(p []byte) (int, error) { return w.f.Write(p) }

func (w *tempWriter) Close() error {
	if !w.closed.CompareAndSwap(false, true) {
		return nil
	}
	// fsync (NFS では best-effort、設計書 03 §6.1 参照)
	if err := w.f.Sync(); err != nil {
		_ = w.f.Close()
		return fmt.Errorf("sync: %w", err)
	}
	return w.f.Close()
}

func (w *tempWriter) UploadUUID() string { return w.uploadUUID }
