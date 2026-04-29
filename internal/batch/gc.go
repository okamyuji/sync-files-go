// Package batch は日次バッチジョブ群を実装する。
//
// 設計書 05-file-operations-logic-tree.md §7、ADR-004 / CR-5 を正準とする。
package batch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/okamyuji/sync-files-go/internal/repo"
	"github.com/okamyuji/sync-files-go/internal/repo/mysql"
	"github.com/okamyuji/sync-files-go/internal/storage"
)

// GarbageCollector trashed → 30 日経過 → purged のバッチ実行（INV-1）。
type GarbageCollector struct {
	Router        *repo.DBRouter
	Files         *mysql.FilesRepo
	FileVersions  *mysql.FileVersionsRepo
	Audit         *mysql.AuditRepo
	Storage       storage.Storage
	Logger        *slog.Logger
	RetentionDays int // 30
	BatchSize     int // 1 回の実行で扱う files の最大数
}

// Run trashed > RetentionDays のファイルを 1 件ずつ purge する。
//
// `RetentionDays < 0` の場合のみ 30 にデフォルト。0 はテストで「即時 purge」を
// 表現するため意図的に有効。
func (g *GarbageCollector) Run(ctx context.Context) (purged int, err error) {
	if g.RetentionDays < 0 {
		g.RetentionDays = 30
	}
	if g.BatchSize <= 0 {
		g.BatchSize = 1000
	}

	candidates, err := g.findTrashedReady(ctx)
	if err != nil {
		return 0, fmt.Errorf("find candidates: %w", err)
	}
	g.Logger.InfoContext(ctx, "gc: candidates", "count", len(candidates), "retention_days", g.RetentionDays)

	now := time.Now().UTC()
	for _, c := range candidates {
		if err := g.purgeOne(ctx, c, now); err != nil {
			g.Logger.ErrorContext(ctx, "gc: purge failed", "file_id", c.fileID, "err", err)
			continue
		}
		purged++
	}
	g.Logger.InfoContext(ctx, "gc: completed", "purged", purged)
	return purged, nil
}

type trashedCandidate struct {
	fileID  uuid.UUID
	ownerID uuid.UUID
}

// findTrashedReady deleted_at < now - RetentionDays の files を引く。
// 閾値は Go 側で計算して渡す（INTERVAL プレースホルダの解釈ばらつき回避）。
func (g *GarbageCollector) findTrashedReady(ctx context.Context) ([]trashedCandidate, error) {
	threshold := time.Now().UTC().Add(-time.Duration(g.RetentionDays) * 24 * time.Hour)
	const q = `
SELECT id_bin, owner_id_bin
  FROM files
 WHERE state = 'trashed'
   AND deleted_at < ?
 ORDER BY deleted_at ASC
 LIMIT ?`
	rows, err := g.Router.Writer(ctx).QueryContext(ctx, q, threshold, g.BatchSize)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]trashedCandidate, 0, g.BatchSize)
	for rows.Next() {
		var idBin, ownerBin []byte
		if err := rows.Scan(&idBin, &ownerBin); err != nil {
			return nil, err
		}
		var c trashedCandidate
		copy(c.fileID[:], idBin)
		copy(c.ownerID[:], ownerBin)
		out = append(out, c)
	}
	return out, rows.Err()
}

// purgeOne 1 ファイルの全バージョンを S3 Files から削除し、files.state を purged に更新する。
func (g *GarbageCollector) purgeOne(ctx context.Context, c trashedCandidate, now time.Time) error {
	versions, err := g.FileVersions.ListByFile(ctx, c.fileID, 1000)
	if err != nil {
		return fmt.Errorf("list versions: %w", err)
	}
	for _, v := range versions {
		err := g.Storage.RemoveVersion(ctx, c.ownerID.String(), c.fileID.String(), v.ID.String())
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return fmt.Errorf("remove version %s: %w", v.ID, err)
		}
	}
	if err := g.Files.Purge(ctx, c.fileID, now); err != nil {
		return fmt.Errorf("set purged state: %w", err)
	}
	_ = g.Audit.Insert(ctx, nil, &mysql.AuditEntry{
		ActorKind:    mysql.ActorSystem,
		Action:       "file.purge_by_retention",
		TargetKind:   "file",
		TargetID:     &c.fileID,
		Irreversible: true,
		Details: map[string]any{
			"owner_id":         c.ownerID.String(),
			"versions_removed": len(versions),
		},
	})
	return nil
}
