package mysql

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/okamyuji/sync-files-go/internal/domain"
	"github.com/okamyuji/sync-files-go/internal/repo"
)

// ShareLinksRepo H-2 修正により、判定系は常に Primary を使う。
type ShareLinksRepo struct {
	router *repo.DBRouter
}

// NewShareLinksRepo コンストラクタ。
func NewShareLinksRepo(router *repo.DBRouter) *ShareLinksRepo {
	return &ShareLinksRepo{router: router}
}

const shareLinkColumns = `
  id_bin, file_id_bin, created_by_bin, token_hash, password_hash,
  expires_at, created_at, revoked_at, view_count, download_count
`

// Insert 公開リンクを Primary に書く。
func (r *ShareLinksRepo) Insert(ctx context.Context, s *domain.ShareLink) error {
	const q = `
INSERT INTO share_links (id_bin, file_id_bin, created_by_bin, token_hash, password_hash, expires_at, created_at)
VALUES (?,?,?,?,?,?,?)
`
	var pwHash any
	if s.PasswordHash != "" {
		pwHash = s.PasswordHash
	}
	_, err := r.router.Writer(ctx).ExecContext(ctx, q,
		uuidToBin(s.ID), uuidToBin(s.FileID), uuidToBin(s.CreatedBy),
		s.TokenHash, pwHash, s.ExpiresAt.UTC(), s.CreatedAt.UTC(),
	)
	return err
}

// FindByTokenHash H-2 修正で常に Primary を読む（取り消し漏れ防止）。
func (r *ShareLinksRepo) FindByTokenHash(ctx context.Context, tokenHash []byte) (*domain.ShareLink, error) {
	q := `SELECT ` + shareLinkColumns + ` FROM share_links WHERE token_hash = ? LIMIT 1`
	row := r.router.Writer(ctx).QueryRowContext(ctx, q, tokenHash)
	return scanShareLink(row)
}

// Revoke 取り消し（revoked_at 設定）。
func (r *ShareLinksRepo) Revoke(ctx context.Context, id uuid.UUID, when time.Time) error {
	const q = `UPDATE share_links SET revoked_at = ? WHERE id_bin = ? AND revoked_at IS NULL`
	res, err := r.router.Writer(ctx).ExecContext(ctx, q, when.UTC(), uuidToBin(id))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RevokeByFile 元ファイルの削除時に呼ぶ（auto-revoke）。
func (r *ShareLinksRepo) RevokeByFile(ctx context.Context, fileID uuid.UUID, when time.Time) error {
	const q = `UPDATE share_links SET revoked_at = ? WHERE file_id_bin = ? AND revoked_at IS NULL`
	_, err := r.router.Writer(ctx).ExecContext(ctx, q, when.UTC(), uuidToBin(fileID))
	return err
}

// IncrementViewCount アクセス時の集計。
func (r *ShareLinksRepo) IncrementViewCount(ctx context.Context, id uuid.UUID) error {
	_, err := r.router.Writer(ctx).ExecContext(ctx, `UPDATE share_links SET view_count = view_count + 1 WHERE id_bin = ?`, uuidToBin(id))
	return err
}

// IncrementDownloadCount ダウンロード集計。
func (r *ShareLinksRepo) IncrementDownloadCount(ctx context.Context, id uuid.UUID) error {
	_, err := r.router.Writer(ctx).ExecContext(ctx, `UPDATE share_links SET download_count = download_count + 1 WHERE id_bin = ?`, uuidToBin(id))
	return err
}

func scanShareLink(row *sql.Row) (*domain.ShareLink, error) {
	var (
		s         domain.ShareLink
		idBin     []byte
		fileBin   []byte
		createdBy []byte
		pwHash    sql.NullString
		revoked   sql.NullTime
	)
	err := row.Scan(
		&idBin, &fileBin, &createdBy, &s.TokenHash, &pwHash,
		&s.ExpiresAt, &s.CreatedAt, &revoked, &s.ViewCount, &s.DownloadCount,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	s.ID, _ = binToUUID(idBin)
	s.FileID, _ = binToUUID(fileBin)
	s.CreatedBy, _ = binToUUID(createdBy)
	if pwHash.Valid {
		s.PasswordHash = pwHash.String
	}
	if revoked.Valid {
		t := revoked.Time
		s.RevokedAt = &t
	}
	return &s, nil
}
