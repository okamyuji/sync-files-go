package mysql

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
	"golang.org/x/text/unicode/norm"

	"github.com/okamyuji/sync-files-go/internal/domain"
	"github.com/okamyuji/sync-files-go/internal/repo"
)

// FilesRepo files テーブルへのアクセス。OCC は呼び出し側でトランザクションと組み合わせる。
type FilesRepo struct {
	router *repo.DBRouter
}

// NewFilesRepo コンストラクタ。
func NewFilesRepo(router *repo.DBRouter) *FilesRepo {
	return &FilesRepo{router: router}
}

const fileColumns = `
  id_bin, owner_id_bin, parent_folder_id_bin, name, path, path_hash,
  current_version_id_bin, size_bytes, content_type, sha256, state,
  created_at, updated_at, deleted_at
`

// NormalizePath NFC 正規化したパスを返す（DB 検索キーは常に正規形式を使う）。
func NormalizePath(path string) string { return norm.NFC.String(path) }

// PathHash NFC 正規化したパスの SHA-256（VARBINARY(32) 検索用）。
func PathHash(path string) []byte {
	h := sha256.Sum256([]byte(NormalizePath(path)))
	return h[:]
}

// Insert 新規 file を Primary に書く。INV-2 / INV-4 の文脈で「先に file_versions を入れてから Update」する手順を呼び出し側で守る。
func (r *FilesRepo) Insert(ctx context.Context, f *domain.File) error {
	const q = `
INSERT INTO files (
  id_bin, owner_id_bin, parent_folder_id_bin, name, path, path_hash,
  current_version_id_bin, size_bytes, content_type, sha256, state, created_at, updated_at
) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
`
	var parent any
	if f.ParentFolderID != nil {
		parent = uuidToBin(*f.ParentFolderID)
	}
	var curVer any
	if f.CurrentVersionID != nil {
		curVer = uuidToBin(*f.CurrentVersionID)
	}
	_, err := r.router.Writer(ctx).ExecContext(ctx, q,
		uuidToBin(f.ID), uuidToBin(f.OwnerID), parent,
		f.Name, NormalizePath(f.Path), PathHash(f.Path),
		curVer, f.SizeBytes, f.ContentType, f.SHA256, string(f.State),
		f.CreatedAt.UTC(), f.UpdatedAt.UTC(),
	)
	return err
}

// FindByID Reader 経由（RAW window 中は Primary）。
func (r *FilesRepo) FindByID(ctx context.Context, id uuid.UUID) (*domain.File, error) {
	q := `SELECT ` + fileColumns + ` FROM files WHERE id_bin = ? LIMIT 1`
	row := r.router.Reader(ctx).QueryRowContext(ctx, q, uuidToBin(id))
	return scanFile(row)
}

// FindActiveByOwnerPath OCC で「同名 active が既存か」を確認する用途。Primary で FOR UPDATE する。
func (r *FilesRepo) FindActiveByOwnerPath(ctx context.Context, tx *sql.Tx, ownerID uuid.UUID, path string) (*domain.File, error) {
	q := `SELECT ` + fileColumns + ` FROM files WHERE owner_id_bin = ? AND path_hash = ? AND state = 'active' FOR UPDATE`
	row := tx.QueryRowContext(ctx, q, uuidToBin(ownerID), PathHash(path))
	return scanFile(row)
}

// GetByOwnerPathActive 事前 OCC チェック用。FOR UPDATE しない短いクエリで Primary を読む。
// 並列 INSERT の排他は active_marker UNIQUE で DB 層が守る（CR-2）。
func (r *FilesRepo) GetByOwnerPathActive(ctx context.Context, ownerID uuid.UUID, path string) (*domain.File, error) {
	q := `SELECT ` + fileColumns + ` FROM files WHERE owner_id_bin = ? AND path_hash = ? AND state = 'active' LIMIT 1`
	row := r.router.Writer(ctx).QueryRowContext(ctx, q, uuidToBin(ownerID), PathHash(path))
	return scanFile(row)
}

// ListActiveByOwner Replica 経由（通常時）。
func (r *FilesRepo) ListActiveByOwner(ctx context.Context, ownerID uuid.UUID, limit, offset int) ([]*domain.File, error) {
	q := `SELECT ` + fileColumns + `
  FROM files
 WHERE owner_id_bin = ? AND state = 'active'
 ORDER BY updated_at DESC
 LIMIT ? OFFSET ?`
	rows, err := r.router.Reader(ctx).QueryContext(ctx, q, uuidToBin(ownerID), limit, offset)
	if err != nil {
		return nil, err
	}
	return scanFiles(rows)
}

// ListTrashedByOwner ゴミ箱表示。
func (r *FilesRepo) ListTrashedByOwner(ctx context.Context, ownerID uuid.UUID, limit, offset int) ([]*domain.File, error) {
	q := `SELECT ` + fileColumns + `
  FROM files
 WHERE owner_id_bin = ? AND state = 'trashed'
 ORDER BY deleted_at DESC
 LIMIT ? OFFSET ?`
	rows, err := r.router.Reader(ctx).QueryContext(ctx, q, uuidToBin(ownerID), limit, offset)
	if err != nil {
		return nil, err
	}
	return scanFiles(rows)
}

// SearchActive ファイル名 + ngram 全文検索（Replica 経由）。
func (r *FilesRepo) SearchActive(ctx context.Context, ownerID uuid.UUID, query string, limit int) ([]*domain.File, error) {
	q := `SELECT ` + fileColumns + `
  FROM files
 WHERE owner_id_bin = ?
   AND state = 'active'
   AND MATCH(name) AGAINST (? IN BOOLEAN MODE)
 ORDER BY updated_at DESC
 LIMIT ?`
	rows, err := r.router.Reader(ctx).QueryContext(ctx, q, uuidToBin(ownerID), query, limit)
	if err != nil {
		return nil, err
	}
	return scanFiles(rows)
}

// UpdateCurrentVersion 上書き完了時に新版に切り替える。トランザクション内で呼ぶ。
func (r *FilesRepo) UpdateCurrentVersion(ctx context.Context, tx *sql.Tx, fileID, versionID uuid.UUID, sha256 []byte, sizeBytes int64, when time.Time) error {
	const q = `UPDATE files
   SET current_version_id_bin = ?, sha256 = ?, size_bytes = ?, updated_at = ?
 WHERE id_bin = ?`
	_, err := tx.ExecContext(ctx, q, uuidToBin(versionID), sha256, sizeBytes, when.UTC(), uuidToBin(fileID))
	return err
}

// SoftDelete state を 'trashed' に。
func (r *FilesRepo) SoftDelete(ctx context.Context, fileID uuid.UUID, when time.Time) error {
	const q = `UPDATE files SET state = 'trashed', deleted_at = ?, updated_at = ?
 WHERE id_bin = ? AND state = 'active'`
	res, err := r.router.Writer(ctx).ExecContext(ctx, q, when.UTC(), when.UTC(), uuidToBin(fileID))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Restore state を 'active' に戻す。
func (r *FilesRepo) Restore(ctx context.Context, fileID uuid.UUID, when time.Time) error {
	const q = `UPDATE files SET state = 'active', deleted_at = NULL, updated_at = ?
 WHERE id_bin = ? AND state = 'trashed'`
	res, err := r.router.Writer(ctx).ExecContext(ctx, q, when.UTC(), uuidToBin(fileID))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Purge state を 'purged' に（INV-1: trashed → purged のみ）。
func (r *FilesRepo) Purge(ctx context.Context, fileID uuid.UUID, when time.Time) error {
	const q = `UPDATE files SET state = 'purged', updated_at = ?
 WHERE id_bin = ? AND state = 'trashed'`
	res, err := r.router.Writer(ctx).ExecContext(ctx, q, when.UTC(), uuidToBin(fileID))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func scanFile(row *sql.Row) (*domain.File, error) {
	var (
		f           domain.File
		idBin       []byte
		ownerBin    []byte
		parentBin   []byte
		curVerBin   []byte
		state       string
		deletedAt   sql.NullTime
		contentType sql.NullString
	)
	err := row.Scan(
		&idBin, &ownerBin, &parentBin, &f.Name, &f.Path, &f.PathHash,
		&curVerBin, &f.SizeBytes, &contentType, &f.SHA256, &state,
		&f.CreatedAt, &f.UpdatedAt, &deletedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if id, err := binToUUID(idBin); err == nil {
		f.ID = id
	}
	if oid, err := binToUUID(ownerBin); err == nil {
		f.OwnerID = oid
	}
	if p, _ := nullableBinToUUIDPtr(parentBin); p != nil {
		f.ParentFolderID = p
	}
	if cv, _ := nullableBinToUUIDPtr(curVerBin); cv != nil {
		f.CurrentVersionID = cv
	}
	f.State = domain.FileState(state)
	if contentType.Valid {
		f.ContentType = contentType.String
	}
	if deletedAt.Valid {
		t := deletedAt.Time
		f.DeletedAt = &t
	}
	return &f, nil
}

func scanFiles(rows *sql.Rows) ([]*domain.File, error) {
	defer func() { _ = rows.Close() }()
	out := make([]*domain.File, 0, 32)
	for rows.Next() {
		var (
			f           domain.File
			idBin       []byte
			ownerBin    []byte
			parentBin   []byte
			curVerBin   []byte
			state       string
			deletedAt   sql.NullTime
			contentType sql.NullString
		)
		if err := rows.Scan(
			&idBin, &ownerBin, &parentBin, &f.Name, &f.Path, &f.PathHash,
			&curVerBin, &f.SizeBytes, &contentType, &f.SHA256, &state,
			&f.CreatedAt, &f.UpdatedAt, &deletedAt,
		); err != nil {
			return nil, err
		}
		if id, err := binToUUID(idBin); err == nil {
			f.ID = id
		}
		if oid, err := binToUUID(ownerBin); err == nil {
			f.OwnerID = oid
		}
		if p, _ := nullableBinToUUIDPtr(parentBin); p != nil {
			f.ParentFolderID = p
		}
		if cv, _ := nullableBinToUUIDPtr(curVerBin); cv != nil {
			f.CurrentVersionID = cv
		}
		f.State = domain.FileState(state)
		if contentType.Valid {
			f.ContentType = contentType.String
		}
		if deletedAt.Valid {
			t := deletedAt.Time
			f.DeletedAt = &t
		}
		out = append(out, &f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
