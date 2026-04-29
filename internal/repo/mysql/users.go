package mysql

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/okamyuji/sync-files-go/internal/repo"
)

// User MySQL 上のユーザレコード。
type User struct {
	ID                uuid.UUID
	Email             string
	PasswordHash      string
	TOTPSecretEnc     []byte
	TOTPSecretHeader  []byte
	TOTPEnabled       bool
	RecoveryCodesJSON []byte // JSON 文字列（[{"hash": "..."}, ...]）
	KEKEnc            []byte
	KEKID             uuid.UUID
	MasterKeyVersion  int
	CreatedAt         time.Time
	LastLoginAt       *time.Time
	LockedUntil       *time.Time
	FailedLoginCount  int
}

// UsersRepo users テーブルへのアクセス。
type UsersRepo struct {
	router *repo.DBRouter
}

// NewUsersRepo コンストラクタ。
func NewUsersRepo(router *repo.DBRouter) *UsersRepo {
	return &UsersRepo{router: router}
}

const userColumns = `
  id_bin, email, password_hash, totp_secret_enc, totp_secret_header,
  totp_enabled, recovery_codes_hash, kek_enc, kek_id_bin, master_key_version,
  created_at, last_login_at, locked_until, failed_login_count
`

// Insert 新規ユーザを Primary に書き込む。
func (r *UsersRepo) Insert(ctx context.Context, u *User) error {
	const q = `
INSERT INTO users (
  id_bin, email, password_hash, totp_secret_enc, totp_secret_header,
  totp_enabled, recovery_codes_hash, kek_enc, kek_id_bin, master_key_version,
  created_at, failed_login_count
) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
`
	_, err := r.router.Writer(ctx).ExecContext(ctx, q,
		uuidToBin(u.ID), u.Email, u.PasswordHash, u.TOTPSecretEnc, u.TOTPSecretHeader,
		u.TOTPEnabled, u.RecoveryCodesJSON, u.KEKEnc, uuidToBin(u.KEKID), u.MasterKeyVersion,
		u.CreatedAt.UTC(), u.FailedLoginCount,
	)
	return err
}

// FindByEmail ログイン処理から呼ばれる。Primary を読む（ログインは強整合性必要）。
func (r *UsersRepo) FindByEmail(ctx context.Context, email string) (*User, error) {
	q := `SELECT ` + userColumns + ` FROM users WHERE email = ? LIMIT 1`
	row := r.router.Writer(ctx).QueryRowContext(ctx, q, email)
	return scanUser(row)
}

// FindByID 任意の場面で。Reader（RAW window 中は Primary）。
func (r *UsersRepo) FindByID(ctx context.Context, id uuid.UUID) (*User, error) {
	q := `SELECT ` + userColumns + ` FROM users WHERE id_bin = ? LIMIT 1`
	row := r.router.Reader(ctx).QueryRowContext(ctx, q, uuidToBin(id))
	return scanUser(row)
}

// UpdateLastLogin ログイン成功時に呼ぶ。失敗カウントもリセット。
func (r *UsersRepo) UpdateLastLogin(ctx context.Context, id uuid.UUID, when time.Time) error {
	const q = `UPDATE users SET last_login_at = ?, failed_login_count = 0, locked_until = NULL WHERE id_bin = ?`
	_, err := r.router.Writer(ctx).ExecContext(ctx, q, when.UTC(), uuidToBin(id))
	return err
}

// IncrementFailedLogin ログイン失敗時に呼ぶ。閾値超過なら locked_until を設定する。
func (r *UsersRepo) IncrementFailedLogin(ctx context.Context, id uuid.UUID, threshold int, lockDuration time.Duration) (newCount int, locked bool, err error) {
	tx, err := r.router.Writer(ctx).BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = tx.Rollback() }()

	var cnt int
	if err := tx.QueryRowContext(ctx, `SELECT failed_login_count FROM users WHERE id_bin = ? FOR UPDATE`, uuidToBin(id)).Scan(&cnt); err != nil {
		return 0, false, translateError(err)
	}
	cnt++
	var lockUntil *time.Time
	if cnt >= threshold {
		t := time.Now().UTC().Add(lockDuration)
		lockUntil = &t
		locked = true
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET failed_login_count = ?, locked_until = ? WHERE id_bin = ?`, cnt, lockUntil, uuidToBin(id)); err != nil {
		return 0, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	return cnt, locked, nil
}

func scanUser(row *sql.Row) (*User, error) {
	var (
		u                User
		idBin            []byte
		kekIDBin         []byte
		lastLoginAt      sql.NullTime
		lockedUntil      sql.NullTime
		totpSecretEnc    sql.NullString
		totpSecretHeader sql.NullString
	)
	err := row.Scan(
		&idBin, &u.Email, &u.PasswordHash, &totpSecretEnc, &totpSecretHeader,
		&u.TOTPEnabled, &u.RecoveryCodesJSON, &u.KEKEnc, &kekIDBin, &u.MasterKeyVersion,
		&u.CreatedAt, &lastLoginAt, &lockedUntil, &u.FailedLoginCount,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	id, err := binToUUID(idBin)
	if err != nil {
		return nil, err
	}
	u.ID = id
	if kid, err := binToUUID(kekIDBin); err == nil {
		u.KEKID = kid
	}
	if totpSecretEnc.Valid {
		u.TOTPSecretEnc = []byte(totpSecretEnc.String)
	}
	if totpSecretHeader.Valid {
		u.TOTPSecretHeader = []byte(totpSecretHeader.String)
	}
	if lastLoginAt.Valid {
		t := lastLoginAt.Time
		u.LastLoginAt = &t
	}
	if lockedUntil.Valid {
		t := lockedUntil.Time
		u.LockedUntil = &t
	}
	return &u, nil
}
