package repo

import (
	"context"

	"github.com/google/uuid"
	"github.com/okamyuji/sync-files-go/internal/domain"
)

// FilesReader は読み取り系の操作。Reader 接続（多くは Replica）から呼ばれる。
type FilesReader interface {
	GetByID(ctx context.Context, id uuid.UUID) (*domain.File, error)
	GetByOwnerPath(ctx context.Context, ownerID uuid.UUID, path string) (*domain.File, error)
	ListActiveByOwner(ctx context.Context, ownerID uuid.UUID, limit, offset int) ([]*domain.File, error)
	ListTrashedByOwner(ctx context.Context, ownerID uuid.UUID, limit, offset int) ([]*domain.File, error)
	Search(ctx context.Context, ownerID uuid.UUID, query string, limit int) ([]*domain.File, error)
}

// FilesWriter は書き込み系。Writer 接続（Primary）から呼ばれる。
//
// 全メソッドはトランザクション境界（*sql.Tx）を呼び出し側が制御できるよう、
// `db.DB` を引数に取る形で実装する（本ファイルは抽象 interface のみ）。
type FilesWriter interface {
	Insert(ctx context.Context, f *domain.File) error
	UpdateCurrentVersion(ctx context.Context, fileID, versionID uuid.UUID) error
	SoftDelete(ctx context.Context, fileID uuid.UUID) error
	Restore(ctx context.Context, fileID uuid.UUID) error
	Purge(ctx context.Context, fileID uuid.UUID) error
}

// FileVersionsWriter は file_versions の書き込み。
type FileVersionsWriter interface {
	Insert(ctx context.Context, v *domain.FileVersion) error
	NextVersionNumber(ctx context.Context, fileID uuid.UUID) (int, error)
}

// ShareLinksWriter / ShareLinksReader は公開リンク。
type ShareLinksReader interface {
	FindByTokenHash(ctx context.Context, tokenHash []byte) (*domain.ShareLink, error)
}
type ShareLinksWriter interface {
	Insert(ctx context.Context, s *domain.ShareLink) error
	Revoke(ctx context.Context, id uuid.UUID) error
	IncrementViewCount(ctx context.Context, id uuid.UUID) error
	IncrementDownloadCount(ctx context.Context, id uuid.UUID) error
}

// 実装は Phase 3 の internal/repo/mysql/ で。
