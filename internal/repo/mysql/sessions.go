package mysql

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/okamyuji/sync-files-go/internal/repo"
)

// Session セッションレコード。
type Session struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
	IPAddr     []byte
	UserAgent  string
}

// SessionsRepo sessions テーブルへのアクセス。
type SessionsRepo struct {
	router *repo.DBRouter
}

// NewSessionsRepo コンストラクタ。
func NewSessionsRepo(router *repo.DBRouter) *SessionsRepo {
	return &SessionsRepo{router: router}
}

// Insert 新規セッション。Primary に書く。
func (r *SessionsRepo) Insert(ctx context.Context, s *Session) error {
	const q = `
INSERT INTO sessions (id_bin, user_id_bin, created_at, last_seen_at, expires_at, ip_addr, user_agent)
VALUES (?,?,?,?,?,?,?)
`
	_, err := r.router.Writer(ctx).ExecContext(ctx, q,
		uuidToBin(s.ID), uuidToBin(s.UserID),
		s.CreatedAt.UTC(), s.LastSeenAt.UTC(), s.ExpiresAt.UTC(),
		s.IPAddr, s.UserAgent,
	)
	return err
}

// FindActive セッション ID から有効なセッションを引く。Primary を読む（認証は強整合性必要）。
func (r *SessionsRepo) FindActive(ctx context.Context, id uuid.UUID) (*Session, error) {
	const q = `
SELECT id_bin, user_id_bin, created_at, last_seen_at, expires_at, ip_addr, user_agent
  FROM sessions
 WHERE id_bin = ? AND expires_at > NOW(6)
 LIMIT 1`
	row := r.router.Writer(ctx).QueryRowContext(ctx, q, uuidToBin(id))
	return scanSession(row)
}

// Touch last_seen_at を更新（必要なら expires_at も再計算）。
func (r *SessionsRepo) Touch(ctx context.Context, id uuid.UUID, when time.Time, newExpiry *time.Time) error {
	if newExpiry != nil {
		_, err := r.router.Writer(ctx).ExecContext(ctx,
			`UPDATE sessions SET last_seen_at = ?, expires_at = ? WHERE id_bin = ?`,
			when.UTC(), newExpiry.UTC(), uuidToBin(id))
		return err
	}
	_, err := r.router.Writer(ctx).ExecContext(ctx,
		`UPDATE sessions SET last_seen_at = ? WHERE id_bin = ?`,
		when.UTC(), uuidToBin(id))
	return err
}

// Delete ログアウト時に呼ぶ。
func (r *SessionsRepo) Delete(ctx context.Context, id uuid.UUID) error {
	_, err := r.router.Writer(ctx).ExecContext(ctx, `DELETE FROM sessions WHERE id_bin = ?`, uuidToBin(id))
	return err
}

// DeleteByUser パスワード変更時などで全セッション失効。
func (r *SessionsRepo) DeleteByUser(ctx context.Context, userID uuid.UUID) (int64, error) {
	res, err := r.router.Writer(ctx).ExecContext(ctx, `DELETE FROM sessions WHERE user_id_bin = ?`, uuidToBin(userID))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func scanSession(row *sql.Row) (*Session, error) {
	var (
		s         Session
		idBin     []byte
		userIDBin []byte
		ipAddr    sql.NullString
		userAgent sql.NullString
	)
	err := row.Scan(&idBin, &userIDBin, &s.CreatedAt, &s.LastSeenAt, &s.ExpiresAt, &ipAddr, &userAgent)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	id, _ := binToUUID(idBin)
	uid, _ := binToUUID(userIDBin)
	s.ID = id
	s.UserID = uid
	if ipAddr.Valid {
		s.IPAddr = []byte(ipAddr.String)
	}
	if userAgent.Valid {
		s.UserAgent = userAgent.String
	}
	return &s, nil
}
