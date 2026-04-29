package domain

import (
	"errors"
	"testing"
	"time"
)

func TestFileState_CanTransitionTo(t *testing.T) {
	cases := []struct {
		from FileState
		to   FileState
		want bool
	}{
		{FileStateDraft, FileStateActive, true},
		{FileStateDraft, FileStateTrashed, false},
		// INV-1: active → purged は禁止（直接物理削除禁止）
		{FileStateActive, FileStateTrashed, true},
		{FileStateActive, FileStatePurged, false},
		{FileStateActive, FileStateGone, false},

		{FileStateTrashed, FileStateActive, true}, // 復元
		{FileStateTrashed, FileStatePurged, true}, // 30 日経過 or 明示 purge
		{FileStateTrashed, FileStateGone, false},  // 直接 gone は不可

		{FileStatePurged, FileStateGone, true},
		{FileStatePurged, FileStateActive, false},

		{FileStateGone, FileStateActive, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.from)+"→"+string(tc.to), func(t *testing.T) {
			if got := tc.from.CanTransitionTo(tc.to); got != tc.want {
				t.Fatalf("want %v got %v", tc.want, got)
			}
		})
	}
}

// TestFile_SoftDelete_INV1 active からの即時 purge を禁止する INV-1 を構造的に確認する。
func TestFile_SoftDelete_INV1(t *testing.T) {
	now := time.Now()
	f := &File{State: FileStateActive}

	if err := f.Purge(now); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("active からの直接 Purge は禁止されるべき: %v", err)
	}

	if err := f.SoftDelete(now); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	if f.State != FileStateTrashed {
		t.Fatalf("after SoftDelete: state=%s", f.State)
	}
	if f.DeletedAt == nil {
		t.Fatal("DeletedAt should be set")
	}

	// trashed → purged は OK
	if err := f.Purge(now); err != nil {
		t.Fatalf("trashed → Purge should succeed: %v", err)
	}
	if f.State != FileStatePurged {
		t.Fatalf("after Purge: state=%s", f.State)
	}
}

// TestFile_Restore trashed → active を確認する。
func TestFile_Restore(t *testing.T) {
	now := time.Now()
	deletedAt := now.Add(-1 * time.Hour)
	f := &File{State: FileStateTrashed, DeletedAt: &deletedAt}

	if err := f.Restore(now); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if f.State != FileStateActive {
		t.Fatalf("after Restore: state=%s", f.State)
	}
	if f.DeletedAt != nil {
		t.Fatalf("DeletedAt should be cleared on Restore")
	}
}
