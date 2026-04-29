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

// FileVersionsRepo file_versions テーブルへのアクセス。
type FileVersionsRepo struct {
	router *repo.DBRouter
}

// NewFileVersionsRepo コンストラクタ。
func NewFileVersionsRepo(router *repo.DBRouter) *FileVersionsRepo {
	return &FileVersionsRepo{router: router}
}

// Insert 新規バージョン行。トランザクション必須なので *sql.Tx を受け取る。
func (r *FileVersionsRepo) Insert(ctx context.Context, tx *sql.Tx, v *domain.FileVersion) error {
	const q = `
INSERT INTO file_versions (
  id_bin, file_id_bin, version_number, size_bytes, sha256,
  storage_key, s3_version_id, dek_enc, kek_id_bin, encryption_scheme, encryption_header,
  created_at, created_by_session_id_bin, deleted_by_user
) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
`
	var sessID any
	if v.CreatedBySessionID != nil {
		sessID = uuidToBin(*v.CreatedBySessionID)
	}
	var s3Ver any
	if v.S3VersionID != "" {
		s3Ver = v.S3VersionID
	}
	_, err := tx.ExecContext(ctx, q,
		uuidToBin(v.ID), uuidToBin(v.FileID), v.VersionNumber, v.SizeBytes, v.SHA256,
		v.StorageKey, s3Ver, v.DEKEnc, uuidToBin(v.KEKID), v.EncryptionScheme, v.EncryptionHeader,
		v.CreatedAt.UTC(), sessID, v.DeletedByUser,
	)
	return err
}

// FindByID 過去版を取り出す（ダウンロードや UI 表示用）。
func (r *FileVersionsRepo) FindByID(ctx context.Context, id uuid.UUID) (*domain.FileVersion, error) {
	const q = `
SELECT id_bin, file_id_bin, version_number, size_bytes, sha256,
       storage_key, s3_version_id, dek_enc, kek_id_bin, encryption_scheme, encryption_header,
       created_at, created_by_session_id_bin, deleted_by_user
  FROM file_versions
 WHERE id_bin = ? LIMIT 1`
	row := r.router.Reader(ctx).QueryRowContext(ctx, q, uuidToBin(id))
	return scanFileVersion(row)
}

// NextVersionNumber 同一ファイルの次に発行する version_number を返す（既存最大値 + 1、無ければ 1）。
func (r *FileVersionsRepo) NextVersionNumber(ctx context.Context, tx *sql.Tx, fileID uuid.UUID) (int, error) {
	const q = `SELECT COALESCE(MAX(version_number), 0) + 1 FROM file_versions WHERE file_id_bin = ?`
	var n int
	if err := tx.QueryRowContext(ctx, q, uuidToBin(fileID)).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// ListByFile UI のバージョン履歴表示用。
func (r *FileVersionsRepo) ListByFile(ctx context.Context, fileID uuid.UUID, limit int) ([]*domain.FileVersion, error) {
	const q = `
SELECT id_bin, file_id_bin, version_number, size_bytes, sha256,
       storage_key, s3_version_id, dek_enc, kek_id_bin, encryption_scheme, encryption_header,
       created_at, created_by_session_id_bin, deleted_by_user
  FROM file_versions
 WHERE file_id_bin = ?
 ORDER BY version_number DESC
 LIMIT ?`
	rows, err := r.router.Reader(ctx).QueryContext(ctx, q, uuidToBin(fileID), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]*domain.FileVersion, 0, 16)
	for rows.Next() {
		v, err := scanFileVersionFromRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// PrunableVersion prune-old-versions バッチが S3 から物理削除する候補。
// 設計書 05 §7.2 / ADR-004 に対応する 90 日経過 + 非 current + deleted_by_user=0 のバージョン。
type PrunableVersion struct {
	ID         uuid.UUID
	FileID     uuid.UUID
	StorageKey string
}

// FindPrunable 90 日経過 + 非 current + 未削除フラグの旧版候補をリストする。
//
// 閾値は Go 側で計算して `?` で渡す（INTERVAL プレースホルダのタイムゾーン解釈ばらつき回避）。
func (r *FileVersionsRepo) FindPrunable(ctx context.Context, olderThanDays int, limit int) ([]PrunableVersion, error) {
	threshold := time.Now().UTC().Add(-time.Duration(olderThanDays) * 24 * time.Hour)
	q := `
SELECT fv.id_bin, fv.file_id_bin, fv.storage_key
  FROM file_versions fv
  JOIN files f ON f.id_bin = fv.file_id_bin
 WHERE fv.created_at < ?
   AND fv.id_bin <> COALESCE(f.current_version_id_bin, X'00000000000000000000000000000000')
   AND fv.deleted_by_user = 0
 ORDER BY fv.created_at ASC
 LIMIT ?`
	rows, err := r.router.Writer(ctx).QueryContext(ctx, q, threshold, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]PrunableVersion, 0, limit)
	for rows.Next() {
		var (
			idBin   []byte
			fileBin []byte
			key     string
		)
		if err := rows.Scan(&idBin, &fileBin, &key); err != nil {
			return nil, err
		}
		id, _ := binToUUID(idBin)
		fid, _ := binToUUID(fileBin)
		out = append(out, PrunableVersion{ID: id, FileID: fid, StorageKey: key})
	}
	return out, rows.Err()
}

// Delete prune バッチが S3 から取り除いた後にメタを消す。
func (r *FileVersionsRepo) Delete(ctx context.Context, id uuid.UUID) error {
	const q = `DELETE FROM file_versions WHERE id_bin = ?`
	res, err := r.router.Writer(ctx).ExecContext(ctx, q, uuidToBin(id))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func scanFileVersion(row *sql.Row) (*domain.FileVersion, error) {
	var (
		v        domain.FileVersion
		idBin    []byte
		fileBin  []byte
		kekIDBin []byte
		s3VerID  sql.NullString
		sessBin  []byte
	)
	err := row.Scan(
		&idBin, &fileBin, &v.VersionNumber, &v.SizeBytes, &v.SHA256,
		&v.StorageKey, &s3VerID, &v.DEKEnc, &kekIDBin, &v.EncryptionScheme, &v.EncryptionHeader,
		&v.CreatedAt, &sessBin, &v.DeletedByUser,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if id, err := binToUUID(idBin); err == nil {
		v.ID = id
	}
	if fid, err := binToUUID(fileBin); err == nil {
		v.FileID = fid
	}
	if kid, err := binToUUID(kekIDBin); err == nil {
		v.KEKID = kid
	}
	if s3VerID.Valid {
		v.S3VersionID = s3VerID.String
	}
	if p, _ := nullableBinToUUIDPtr(sessBin); p != nil {
		v.CreatedBySessionID = p
	}
	return &v, nil
}

func scanFileVersionFromRows(rows *sql.Rows) (*domain.FileVersion, error) {
	var (
		v        domain.FileVersion
		idBin    []byte
		fileBin  []byte
		kekIDBin []byte
		s3VerID  sql.NullString
		sessBin  []byte
	)
	err := rows.Scan(
		&idBin, &fileBin, &v.VersionNumber, &v.SizeBytes, &v.SHA256,
		&v.StorageKey, &s3VerID, &v.DEKEnc, &kekIDBin, &v.EncryptionScheme, &v.EncryptionHeader,
		&v.CreatedAt, &sessBin, &v.DeletedByUser,
	)
	if err != nil {
		return nil, err
	}
	if id, err := binToUUID(idBin); err == nil {
		v.ID = id
	}
	if fid, err := binToUUID(fileBin); err == nil {
		v.FileID = fid
	}
	if kid, err := binToUUID(kekIDBin); err == nil {
		v.KEKID = kid
	}
	if s3VerID.Valid {
		v.S3VersionID = s3VerID.String
	}
	if p, _ := nullableBinToUUIDPtr(sessBin); p != nil {
		v.CreatedBySessionID = p
	}
	return &v, nil
}
