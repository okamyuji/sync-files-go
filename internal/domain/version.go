package domain

import (
	"time"

	"github.com/google/uuid"
)

// FileVersion immutable な物理バージョン。
// storage_key 'owner-{owner}/versions/{file_id}/{id}' という固定形式（CR-1）。
type FileVersion struct {
	ID            uuid.UUID
	FileID        uuid.UUID
	VersionNumber int
	SizeBytes     int64
	SHA256        []byte
	StorageKey    string
	S3VersionID   string

	// 鍵階層 (CR-3)
	DEKEnc           []byte // KEK で AES-Key-Wrap した DEK
	KEKID            uuid.UUID
	EncryptionScheme string // 例: "tink-streaming-aead-aes256-gcm-hkdf-1mb-v1"
	EncryptionHeader []byte // スキーム固有のヘッダ

	CreatedAt          time.Time
	CreatedBySessionID *uuid.UUID
	DeletedByUser      bool
}
