package batch

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/okamyuji/sync-files-go/internal/repo"
	"github.com/okamyuji/sync-files-go/internal/repo/mysql"
	"github.com/okamyuji/sync-files-go/internal/storage"
)

// Reconciler 補正ジョブ。設計書 03-domain-model.md §6.3。
//
// 機能：
//   - 期限切れ upload_session の tmp/{uuid}.part を掃除（7 日経過）
//   - file_versions に対応する S3 オブジェクトが存在しないものを検出してアラート
//   - S3 上の versions/*/* で対応する file_versions 行が無いものを /_orphan/ に隔離
type Reconciler struct {
	Router       *repo.DBRouter
	FileVersions *mysql.FileVersionsRepo
	Audit        *mysql.AuditRepo
	Storage      storage.Storage
	Logger       *slog.Logger
}

// CleanupExpiredUploads upload_sessions.expires_at < now の tmp ファイルを削除。
// バッチで毎時実行。
func (r *Reconciler) CleanupExpiredUploads(ctx context.Context) (cleaned int, err error) {
	const q = `
SELECT id_bin, owner_id_bin, storage_key
  FROM upload_sessions
 WHERE completed_at IS NULL
   AND expires_at < NOW(6)
 LIMIT 1000`
	rows, err := r.Router.Writer(ctx).QueryContext(ctx, q)
	if err != nil {
		return 0, fmt.Errorf("query expired: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var batch []expiredUpload
	for rows.Next() {
		var idBin, ownerBin []byte
		var key string
		if err := rows.Scan(&idBin, &ownerBin, &key); err != nil {
			return 0, err
		}
		var u expiredUpload
		copy(u.uploadID[:], idBin)
		copy(u.ownerID[:], ownerBin)
		u.storageKey = key
		batch = append(batch, u)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for _, u := range batch {
		// storage_key には tmp/{upload_uuid}.part が入る前提
		uploadStr := u.uploadID.String()
		if err := r.Storage.RemoveTemp(ctx, u.ownerID.String(), uploadStr); err != nil {
			r.Logger.WarnContext(ctx, "remove tmp failed", "upload_id", u.uploadID, "err", err)
			continue
		}
		_, _ = r.Router.Writer(ctx).ExecContext(ctx,
			`DELETE FROM upload_sessions WHERE id_bin = ?`, u.uploadID[:])
		cleaned++
	}
	r.Logger.InfoContext(ctx, "reconcile: cleaned expired uploads", "count", cleaned)
	return cleaned, nil
}

type expiredUpload struct {
	uploadID   uuid.UUID
	ownerID    uuid.UUID
	storageKey string
}

// MetadataOrphans file_versions に対応する物理オブジェクトが無い行を検出。
// 真の障害なのでメトリクス + アラート。物理削除はしない（誤削除リスク）。
func (r *Reconciler) MetadataOrphans(ctx context.Context, batchSize int) (count int, err error) {
	if batchSize <= 0 {
		batchSize = 100
	}
	const q = `
SELECT fv.id_bin, fv.file_id_bin, fv.storage_key, f.owner_id_bin
  FROM file_versions fv
  JOIN files f ON f.id_bin = fv.file_id_bin
 WHERE f.state IN ('active', 'trashed')
 ORDER BY fv.created_at DESC
 LIMIT ?`
	rows, err := r.Router.Writer(ctx).QueryContext(ctx, q, batchSize)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var verBin, fileBin, ownerBin []byte
		var key string
		if err := rows.Scan(&verBin, &fileBin, &key, &ownerBin); err != nil {
			return 0, err
		}
		owner, fileID, versionID := storageKeyParts(key)
		if owner == "" {
			continue
		}
		exists, err := r.Storage.VersionExists(ctx, owner, fileID, versionID)
		if err != nil {
			r.Logger.WarnContext(ctx, "stat version failed", "key", key, "err", err)
			continue
		}
		if !exists {
			var verID uuid.UUID
			copy(verID[:], verBin)
			r.Logger.ErrorContext(ctx, "metadata orphan detected (no S3 object)",
				"version_id", verID.String(), "storage_key", key)
			_ = r.Audit.Insert(ctx, nil, &mysql.AuditEntry{
				ActorKind:  mysql.ActorSystem,
				Action:     "reconcile.metadata_orphan",
				TargetKind: "file_version",
				TargetID:   &verID,
				Details: map[string]any{
					"storage_key": key,
					"detected_at": time.Now().UTC().Format(time.RFC3339Nano),
				},
			})
			count++
		}
	}
	return count, rows.Err()
}
