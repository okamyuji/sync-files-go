package domain

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// OCCOutcome はアップロード時の Precondition 評価結果。
//
// 設計書 04-sync-semantics.md §4.2 / 05-file-operations-logic-tree.md §1 に対応。
type OCCOutcome int

const (
	OCCAccept              OCCOutcome = iota // 通常の上書き、または新規作成
	OCCForceAccept                            // If-Match: * （ユーザが強制上書きを選択）
	OCCConflict                               // If-Match のバージョンが古い
	OCCPreconditionFailed                     // 412：新規作成と矛盾、または対象が存在しない
	OCCNeedPrecondition                       // 428：ヘッダなし
	OCCFileGone                               // 410：対象が trashed/purged/gone
)

func (o OCCOutcome) String() string {
	switch o {
	case OCCAccept:
		return "accept"
	case OCCForceAccept:
		return "force_accept"
	case OCCConflict:
		return "conflict"
	case OCCPreconditionFailed:
		return "precondition_failed"
	case OCCNeedPrecondition:
		return "need_precondition"
	case OCCFileGone:
		return "file_gone"
	}
	return fmt.Sprintf("unknown(%d)", o)
}

// Precondition は HTTP の If-Match / If-None-Match を解釈した結果。
type Precondition struct {
	IfMatch     string // 値の例: "<version_uuid>" / "*" / ""
	IfNoneMatch string // 値の例: "*" / ""
}

// CurrentRef はサーバ側で観測した現行の状態（OCC 評価のためだけに必要な最小集合）。
// nil の場合は「対象ファイルが（active として）存在しない」を意味する。
type CurrentRef struct {
	State            FileState
	CurrentVersionID uuid.UUID
}

// ResolveOCC は Precondition と現行参照を比較して OCC の判定を返す。
//
// 純関数。DB アクセス・I/O・時計依存なし。Phase 2 単体テストの中心。
func ResolveOCC(cur *CurrentRef, pre Precondition) OCCOutcome {
	hasIfMatch := pre.IfMatch != ""
	hasIfNoneMatch := pre.IfNoneMatch != ""

	switch {
	case !hasIfMatch && !hasIfNoneMatch:
		// 設計書 04 §4.1: ヘッダなしのアップロードは 428 で拒否
		return OCCNeedPrecondition

	case hasIfNoneMatch && pre.IfNoneMatch == "*":
		// 新規作成のみを意図
		if cur == nil {
			return OCCAccept
		}
		// 既存があれば 412
		return OCCPreconditionFailed

	case hasIfMatch && pre.IfMatch == "*":
		// 強制上書き：対象が無い場合は 412
		if cur == nil {
			return OCCPreconditionFailed
		}
		if cur.State != FileStateActive {
			return OCCFileGone
		}
		return OCCForceAccept

	case hasIfMatch:
		// 通常の OCC
		if cur == nil {
			return OCCPreconditionFailed
		}
		if cur.State != FileStateActive {
			return OCCFileGone
		}
		if pre.IfMatch == cur.CurrentVersionID.String() {
			return OCCAccept
		}
		return OCCConflict
	}
	return OCCNeedPrecondition
}

// ConflictCopyName はコンフリクトコピーの命名規則を実装する。
//
// 例:
//
//	"Q2 Report.docx" + "Pixel" + 2026-04-29 14:32 →
//	  "Q2 Report (conflict 2026-04-29 14-32 device-Pixel).docx"
func ConflictCopyName(originalName, deviceLabel string, when time.Time) string {
	base, ext := splitExt(originalName)
	stamp := when.Format("2006-01-02 15-04")
	if deviceLabel == "" {
		deviceLabel = "unknown"
	}
	return fmt.Sprintf("%s (conflict %s device-%s)%s", base, stamp, deviceLabel, ext)
}

// splitExt は最後のドット以降を拡張子として返す（先頭ドットのドットファイルは拡張子なし扱い）。
func splitExt(name string) (base, ext string) {
	for i := len(name) - 1; i > 0; i-- {
		if name[i] == '.' {
			return name[:i], name[i:]
		}
	}
	return name, ""
}
