package domain

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestResolveOCC は設計書 04-sync-semantics.md §4.2 / §5 の 7 分岐を網羅的にテストする。
func TestResolveOCC(t *testing.T) {
	v1 := uuid.New()
	v2 := uuid.New()
	active := func(v uuid.UUID) *CurrentRef { return &CurrentRef{State: FileStateActive, CurrentVersionID: v} }
	trashed := &CurrentRef{State: FileStateTrashed, CurrentVersionID: v1}
	purged := &CurrentRef{State: FileStatePurged, CurrentVersionID: v1}

	cases := []struct {
		name string
		cur  *CurrentRef
		pre  Precondition
		want OCCOutcome
	}{
		{"no headers → 428", active(v1), Precondition{}, OCCNeedPrecondition},
		{"new with If-None-Match: *", nil, Precondition{IfNoneMatch: "*"}, OCCAccept},
		{"new with If-Match (no target) → 412", nil, Precondition{IfMatch: v1.String()}, OCCPreconditionFailed},
		{"existing + If-None-Match: * → 412", active(v1), Precondition{IfNoneMatch: "*"}, OCCPreconditionFailed},
		{"matching If-Match → accept", active(v1), Precondition{IfMatch: v1.String()}, OCCAccept},
		{"stale If-Match → conflict", active(v2), Precondition{IfMatch: v1.String()}, OCCConflict},
		{"force overwrite (active)", active(v1), Precondition{IfMatch: "*"}, OCCForceAccept},
		{"force overwrite (no target) → 412", nil, Precondition{IfMatch: "*"}, OCCPreconditionFailed},
		{"trashed target with If-Match → gone", trashed, Precondition{IfMatch: v1.String()}, OCCFileGone},
		{"purged target with If-Match: * → gone", purged, Precondition{IfMatch: "*"}, OCCFileGone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveOCC(tc.cur, tc.pre)
			if got != tc.want {
				t.Fatalf("want %s got %s", tc.want, got)
			}
		})
	}
}

func TestConflictCopyName(t *testing.T) {
	when, _ := time.Parse(time.RFC3339, "2026-04-29T14:32:11+09:00")

	cases := []struct {
		original string
		device   string
		want     string
	}{
		{"report.docx", "Pixel", "report (conflict 2026-04-29 14-32 device-Pixel).docx"},
		{"no-extension", "iPhone", "no-extension (conflict 2026-04-29 14-32 device-iPhone)"},
		{"a.b.tar.gz", "", "a.b.tar (conflict 2026-04-29 14-32 device-unknown).gz"}, // 最後の . のみ拡張子
	}
	for _, tc := range cases {
		t.Run(tc.original, func(t *testing.T) {
			got := ConflictCopyName(tc.original, tc.device, when)
			if got != tc.want {
				t.Fatalf("want %q got %q", tc.want, got)
			}
		})
	}
}
