package domain

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"time"

	"github.com/google/uuid"
)

// ShareLink は公開リンク。
//
// CR-3/H-3 修正: URL に出すのは TokenHash の元になる base64url ランダム値で、
// DB SHA-256(token) のみ保管する（平文は保管しない）。
type ShareLink struct {
	ID            uuid.UUID
	FileID        uuid.UUID
	CreatedBy     uuid.UUID
	TokenHash     []byte // SHA-256(token), 32 bytes
	PasswordHash  string // Argon2id; 空なら認証不要
	ExpiresAt     time.Time
	CreatedAt     time.Time
	RevokedAt     *time.Time
	ViewCount     int64
	DownloadCount int64
}

// IsActive はアクセス可能性を返す（取り消し済み・期限切れでないこと）。
func (s *ShareLink) IsActive(now time.Time) bool {
	if s.RevokedAt != nil {
		return false
	}
	if !s.ExpiresAt.IsZero() && now.After(s.ExpiresAt) {
		return false
	}
	return true
}

// ExpiresInOption v1 が許す期限（HIGH 修正：期限なし禁止）。
type ExpiresInOption string

const (
	ExpiresIn1Hour ExpiresInOption = "1h"
	ExpiresIn1Day  ExpiresInOption = "1d"
	ExpiresIn7Days ExpiresInOption = "7d"
)

// Duration ExpiresInOption を time.Duration に。
func (e ExpiresInOption) Duration() (time.Duration, bool) {
	switch e {
	case ExpiresIn1Hour:
		return 1 * time.Hour, true
	case ExpiresIn1Day:
		return 24 * time.Hour, true
	case ExpiresIn7Days:
		return 7 * 24 * time.Hour, true
	}
	return 0, false
}

// ErrInvalidExpiresIn は許されない期限指定（v1 では「期限なし」も含む）。
var ErrInvalidExpiresIn = errors.New("invalid expires_in: v1 requires 1h / 1d / 7d")

// GenerateShareToken base64url 32 bytes ランダム token を生成し、平文と SHA-256 ハッシュを返す。
//
// 戻り値:
//   - tokenPlain: ユーザに渡す URL に含める文字列（DB には保管しない）
//   - tokenHash: DB に保管するハッシュ
func GenerateShareToken() (tokenPlain string, tokenHash []byte, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, err
	}
	tokenPlain = base64.RawURLEncoding.EncodeToString(buf)
	hash := sha256.Sum256([]byte(tokenPlain))
	return tokenPlain, hash[:], nil
}

// HashShareToken は受信した token を DB 索引引きするためにハッシュ化する。
// （定数時間比較は DB の UNIQUE 索引でカバー）
func HashShareToken(tokenPlain string) []byte {
	h := sha256.Sum256([]byte(tokenPlain))
	return h[:]
}
