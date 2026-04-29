package batch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/okamyuji/sync-files-go/internal/repo/mysql"
	"github.com/okamyuji/sync-files-go/internal/storage"
)

// OldVersionPruner 90 日経過 + 非 current の file_versions を削除するバッチ（CR-5）。
//
// immutable key 設計では S3 lifecycle の noncurrent_version_expiration が機能しないため、
// アプリ層でこのバッチが唯一の prune 経路となる。
type OldVersionPruner struct {
	FileVersions  *mysql.FileVersionsRepo
	Audit         *mysql.AuditRepo
	Storage       storage.Storage
	Logger        *slog.Logger
	RetentionDays int // 90
	BatchSize     int // 1000
}

// Run 1 回の実行でバッチサイズ分の prune を試みる。
//
// `RetentionDays < 0` の場合のみ 90 にデフォルト。0 はテストで「即時 prune」を
// 表現するため意図的に有効。
func (p *OldVersionPruner) Run(ctx context.Context) (pruned int, err error) {
	if p.RetentionDays < 0 {
		p.RetentionDays = 90
	}
	if p.BatchSize <= 0 {
		p.BatchSize = 1000
	}

	candidates, err := p.FileVersions.FindPrunable(ctx, p.RetentionDays, p.BatchSize)
	if err != nil {
		return 0, fmt.Errorf("find prunable: %w", err)
	}
	p.Logger.InfoContext(ctx, "prune-old-versions: candidates", "count", len(candidates), "retention_days", p.RetentionDays)

	for _, c := range candidates {
		if err := p.pruneOne(ctx, c); err != nil {
			p.Logger.ErrorContext(ctx, "prune-old-versions: failed", "version_id", c.ID, "err", err)
			continue
		}
		pruned++
	}
	p.Logger.InfoContext(ctx, "prune-old-versions: completed", "pruned", pruned)
	return pruned, nil
}

// pruneOne 1 バージョンを S3 から削除して file_versions 行を消す。
func (p *OldVersionPruner) pruneOne(ctx context.Context, c mysql.PrunableVersion) error {
	ownerID, fileID, versionID := storageKeyParts(c.StorageKey)
	if ownerID == "" || fileID == "" || versionID == "" {
		return fmt.Errorf("malformed storage_key: %s", c.StorageKey)
	}
	if err := p.Storage.RemoveVersion(ctx, ownerID, fileID, versionID); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return fmt.Errorf("remove version: %w", err)
	}
	if err := p.FileVersions.Delete(ctx, c.ID); err != nil {
		return fmt.Errorf("delete row: %w", err)
	}
	_ = p.Audit.Insert(ctx, nil, &mysql.AuditEntry{
		ActorKind:    mysql.ActorSystem,
		Action:       "file_version.prune_by_age",
		TargetKind:   "file_version",
		TargetID:     &c.ID,
		Irreversible: true,
		Details: map[string]any{
			"file_id":     c.FileID.String(),
			"storage_key": c.StorageKey,
			"pruned_at":   time.Now().UTC().Format(time.RFC3339Nano),
		},
	})
	return nil
}

// storageKeyParts "owner-{owner}/versions/{file}/{version}" から要素を取り出す。
func storageKeyParts(key string) (owner, file, version string) {
	// 期待フォーマット: owner-{uuid}/versions/{uuid}/{uuid}
	parts := strings.Split(key, "/")
	if len(parts) != 4 || !strings.HasPrefix(parts[0], "owner-") || parts[1] != "versions" {
		return "", "", ""
	}
	return strings.TrimPrefix(parts[0], "owner-"), parts[2], parts[3]
}
