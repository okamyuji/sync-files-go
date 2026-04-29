// Package mysql は MySQL 8.0 を裏に持つ repo 実装。
//
// 設計書 03-domain-model.md §4 を正準とする。プレースホルダは `?`、
// UUID v4 は BINARY(16) で保管、UTC を `loc=UTC` で扱う。
package mysql

import (
	"database/sql"
	"errors"

	"github.com/google/uuid"
)

// ErrNotFound 行が見つからなかったときの汎用センチネル。
var ErrNotFound = errors.New("mysql: row not found")

// uuidToBin uuid.UUID を BINARY(16) 用 []byte に。
func uuidToBin(u uuid.UUID) []byte { b := u[:]; return b }

// binToUUID クエリ結果の []byte を uuid.UUID に。
func binToUUID(b []byte) (uuid.UUID, error) {
	if len(b) != 16 {
		return uuid.Nil, errors.New("expected 16 bytes for UUID")
	}
	var u uuid.UUID
	copy(u[:], b)
	return u, nil
}

// nullableBinToUUIDPtr NULL 許容な BINARY(16) 列を *uuid.UUID に。
func nullableBinToUUIDPtr(b []byte) (*uuid.UUID, error) {
	if len(b) == 0 {
		return nil, nil
	}
	u, err := binToUUID(b)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// translateError sql.ErrNoRows を ErrNotFound に変換するヘルパ。
func translateError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
