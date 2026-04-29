// Package domain はファイル同期システムの中核ドメインモデルを定義する。
//
// このパッケージは外部ライブラリへの依存を持たない（uuid のみ許容）。
// 設計書 03-domain-model.md を正準とする。
package domain

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// FileState はファイルのライフサイクル状態。
//
// 設計書 02-architecture.md §5 のステートマシン：
//
//	draft → active → trashed → purged → gone
type FileState string

const (
	FileStateDraft   FileState = "draft"
	FileStateActive  FileState = "active"
	FileStateTrashed FileState = "trashed"
	FileStatePurged  FileState = "purged"
	FileStateGone    FileState = "gone"
)

// IsValid は state 列の CHECK 制約と等価。
func (s FileState) IsValid() bool {
	switch s {
	case FileStateDraft, FileStateActive, FileStateTrashed, FileStatePurged, FileStateGone:
		return true
	}
	return false
}

// CanTransitionTo は遷移可能性を返す（INV-1 を構造的に守るため）。
func (s FileState) CanTransitionTo(next FileState) bool {
	switch s {
	case FileStateDraft:
		return next == FileStateActive
	case FileStateActive:
		// active → trashed のみ。active → purged は INV-1 違反（直接物理削除禁止）
		return next == FileStateTrashed
	case FileStateTrashed:
		// trashed → active (復元) or trashed → purged (バッチ or 明示 purge)
		return next == FileStateActive || next == FileStatePurged
	case FileStatePurged:
		// purged → gone (S3 ライフサイクル後)
		return next == FileStateGone
	case FileStateGone:
		return false
	}
	return false
}

// File は論理ファイルのドメインモデル。
//
// バイト列は file_versions/{id}/{version_id} に置き、File は「現行版へのポインタ」を持つ論理エンティティ。
type File struct {
	ID               uuid.UUID
	OwnerID          uuid.UUID
	ParentFolderID   *uuid.UUID
	Name             string // NFC 正規化済み
	Path             string
	PathHash         []byte // SHA-256(NFC(Path))
	CurrentVersionID *uuid.UUID
	SizeBytes        int64 // 現行版のサイズ（キャッシュ）
	ContentType      string
	SHA256           []byte // 現行版の sha256（キャッシュ）
	State            FileState
	CreatedAt        time.Time
	UpdatedAt        time.Time
	DeletedAt        *time.Time
}

// ErrInvalidTransition は許されない state 遷移を試みたとき返るセンチネル。
var ErrInvalidTransition = errors.New("invalid file state transition")

// SoftDelete はファイルを active から trashed に遷移させる。
// active 以外からの呼び出しは ErrInvalidTransition を返す。
func (f *File) SoftDelete(now time.Time) error {
	if !f.State.CanTransitionTo(FileStateTrashed) {
		return ErrInvalidTransition
	}
	f.State = FileStateTrashed
	f.DeletedAt = &now
	f.UpdatedAt = now
	return nil
}

// Restore は trashed から active に戻す。
func (f *File) Restore(now time.Time) error {
	if f.State != FileStateTrashed {
		return ErrInvalidTransition
	}
	f.State = FileStateActive
	f.DeletedAt = nil
	f.UpdatedAt = now
	return nil
}

// Purge は trashed から purged に遷移させる（INV-1）。
// active から直接呼ぶことはできない。
func (f *File) Purge(now time.Time) error {
	if !f.State.CanTransitionTo(FileStatePurged) {
		return ErrInvalidTransition
	}
	f.State = FileStatePurged
	f.UpdatedAt = now
	return nil
}

// Finalize は purged から gone に遷移させる（v1 では使わないが、バッチ用に予約）。
func (f *File) Finalize(now time.Time) error {
	if !f.State.CanTransitionTo(FileStateGone) {
		return ErrInvalidTransition
	}
	f.State = FileStateGone
	f.UpdatedAt = now
	return nil
}
