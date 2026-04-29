// Package storage はファイル本体のストレージ抽象。
//
// 本番では S3 Files (NFS マウント) を localfs 実装で扱う。
// テスト用に memory 実装も用意する。
//
// 設計書 03-domain-model.md §5（immutable versions key）を前提：
//   - すべての書き込みは tmp/{upload_uuid}.part に書いてから versions/{file_id}/{version_id} へ os.Rename
//   - 既存キーへの上書きはしない
package storage

import (
	"context"
	"errors"
	"io"
)

// Storage は本体ストレージの最小 interface。
type Storage interface {
	// CreateTemp tmp/{upload_uuid}.part を生成し、書き込み用の Writer を返す。
	// 呼び出し側は Close() で fsync を期待してよい。
	CreateTemp(ctx context.Context, ownerID, uploadUUID string) (TempWriter, error)

	// FinalizeVersion CreateTemp で書いた一時ファイルを versions/{file_id}/{version_id} に
	// 原子的にリネームする。リネーム後の絶対パス（または storage_key）を返す。
	// versions 配下のキーは新規。既存があったらエラー。
	FinalizeVersion(ctx context.Context, ownerID, uploadUUID, fileID, versionID string) (storageKey string, err error)

	// OpenVersion versions/{file_id}/{version_id} を読み取り用に開く。
	OpenVersion(ctx context.Context, ownerID, fileID, versionID string) (io.ReadSeekCloser, error)

	// RemoveVersion versions/{file_id}/{version_id} を削除する（purge 用）。
	RemoveVersion(ctx context.Context, ownerID, fileID, versionID string) error

	// RemoveTemp CreateTemp で開いた一時ファイルを後片付けする（中断時）。
	RemoveTemp(ctx context.Context, ownerID, uploadUUID string) error

	// VersionExists versions/{file_id}/{version_id} が存在するか確認する（補正ジョブ用）。
	VersionExists(ctx context.Context, ownerID, fileID, versionID string) (bool, error)
}

// TempWriter CreateTemp の戻り値。Close 時に fsync 相当を期待する。
type TempWriter interface {
	io.Writer
	io.Closer
	// UploadUUID は内部で生成された一時ファイル ID。FinalizeVersion に渡す。
	UploadUUID() string
}

// ErrAlreadyExists versions 配下のキーがすでに存在するときのセンチネル。
var ErrAlreadyExists = errors.New("storage: target version key already exists")

// ErrNotFound はオブジェクトが存在しない。
var ErrNotFound = errors.New("storage: not found")
