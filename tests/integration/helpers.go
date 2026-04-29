//go:build integration

// Package integration は testcontainers-go ベースの統合テストヘルパーを提供する。
//
// Build tag `integration` で分離。実行は `make test-integration`。
// Docker daemon が必要（macOS なら Docker Desktop）。
package integration

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	tcmysql "github.com/testcontainers/testcontainers-go/modules/mysql"

	"github.com/okamyuji/sync-files-go/internal/repo"
	"github.com/okamyuji/sync-files-go/internal/repo/mysql"
)

// hmacFor middleware と同じく hmac.New を返すヘルパー。
func hmacFor(key []byte) hash.Hash { return hmac.New(sha256.New, key) }

// sha256Hash 32 bytes ハッシュ。
func sha256Hash(b []byte) []byte { h := sha256.Sum256(b); return h[:] }

// Env テスト 1 件分の依存性（DB 接続、DBRouter、tempdir）。
type Env struct {
	Primary *sql.DB
	Replica *sql.DB
	Router  *repo.DBRouter

	Users        *mysql.UsersRepo
	Sessions     *mysql.SessionsRepo
	Files        *mysql.FilesRepo
	FileVersions *mysql.FileVersionsRepo
	ShareLinks   *mysql.ShareLinksRepo
	Audit        *mysql.AuditRepo

	DataDir string

	cleanup func()
}

// Close 後片付け。
func (e *Env) Close() { e.cleanup() }

// migrationsSQL はテストプロセスの起動時に一度だけ読み込んで使い回す。
var (
	migrationsOnce sync.Once
	migrationsSQL  string
)

// SetupEnv MySQL コンテナを起動し、マイグレーションを適用して Env を返す。
//
// パッケージレベルの sync.Once でコンテナを 1 つだけ起動し、テスト関数間で使い回す。
// 各テストでは uuid を含めたユニークなデータを使うことで干渉を防ぐ。
func SetupEnv(t *testing.T) *Env {
	t.Helper()
	dsn := startSharedMySQL(t)

	primary, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open primary: %v", err)
	}
	primary.SetMaxOpenConns(20)
	primary.SetMaxIdleConns(5)
	if err := primary.PingContext(context.Background()); err != nil {
		t.Fatalf("ping primary: %v", err)
	}

	// Replica は同一 MySQL を別接続で開く（v1 ローカル統合テストでは Read Replica の遅延を模さない）
	replica, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open replica: %v", err)
	}
	replica.SetMaxOpenConns(20)
	replica.SetMaxIdleConns(5)

	router := repo.NewDBRouter(primary, replica)

	dataDir, _ := os.MkdirTemp("", "sync-files-int-*")

	env := &Env{
		Primary:      primary,
		Replica:      replica,
		Router:       router,
		Users:        mysql.NewUsersRepo(router),
		Sessions:     mysql.NewSessionsRepo(router),
		Files:        mysql.NewFilesRepo(router),
		FileVersions: mysql.NewFileVersionsRepo(router),
		ShareLinks:   mysql.NewShareLinksRepo(router),
		Audit:        mysql.NewAuditRepo(router),
		DataDir:      dataDir,
		cleanup: func() {
			_ = primary.Close()
			_ = replica.Close()
			_ = os.RemoveAll(dataDir)
		},
	}
	t.Cleanup(env.Close)
	return env
}

// startSharedMySQL コンテナをパッケージで一度だけ起動。
var (
	sharedDSNOnce sync.Once
	sharedDSN     string
	sharedDSNErr  error
)

func startSharedMySQL(t *testing.T) string {
	sharedDSNOnce.Do(func() {
		// arm64 Mac では起動に 60-120 秒かかる場合があるため余裕をもって 240 秒
		ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
		defer cancel()

		t.Log("[testcontainers] mysql:8.0 起動中... (60-120 秒かかる場合があります)")
		container, err := tcmysql.Run(ctx, "mysql:8.0",
			tcmysql.WithDatabase("sync"),
			tcmysql.WithUsername("sync_app"),
			tcmysql.WithPassword("dev"),
			testcontainers.WithEnv(map[string]string{
				"MYSQL_ROOT_PASSWORD": "rootdev",
			}),
		)
		if err != nil {
			sharedDSNErr = fmt.Errorf("start mysql container: %w", err)
			return
		}
		// パラメータ追加（ngram、charset）はコンテナ起動後に SQL で当てる代わりに
		// migration で必要なものだけ済ませる。
		dsnRaw, err := container.ConnectionString(ctx,
			"charset=utf8mb4",
			"parseTime=true",
			"loc=UTC",
			"multiStatements=true",
		)
		if err != nil {
			sharedDSNErr = fmt.Errorf("get conn string: %w", err)
			return
		}
		// ngram_token_size は SET GLOBAL で動的に変更不可なので、コンテナ起動オプションで対応するか
		// MATCH AGAINST のテストはスキップする。本テストでは UNIQUE 検証が中心なので問題なし。
		sharedDSN = dsnRaw

		if err := applyMigrations(ctx, dsnRaw); err != nil {
			sharedDSNErr = fmt.Errorf("apply migrations: %w", err)
			return
		}
		// ngram FULLTEXT パーサが ngram_token_size=2 でないと検索できない。
		// MATCH 検索系テストは Phase 5 で扱うので v1 統合テストでは扱わない。
	})
	if sharedDSNErr != nil {
		t.Fatalf("shared mysql: %v", sharedDSNErr)
	}
	return sharedDSN
}

// applyMigrations migrations/ 配下の .sql を実行する。
// 0001_init.sql に FULLTEXT KEY が含まれており、ngram parser が無効でも CREATE は通る
// （検索クエリ実行時のみ ngram parser が必要）。
func applyMigrations(ctx context.Context, dsn string) error {
	migrationsOnce.Do(func() {
		// repo ルートの migrations/0001_init.sql を読む
		root := repoRoot()
		migPath := filepath.Join(root, "migrations", "0001_init.sql")
		data, err := os.ReadFile(migPath) //nolint:gosec // テスト
		if err == nil {
			migrationsSQL = string(data)
		}
	})
	if migrationsSQL == "" {
		return fmt.Errorf("migrations file not found")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	// multi-statements で 1 ファイルを一括実行
	if _, err := db.ExecContext(ctx, migrationsSQL); err != nil {
		return fmt.Errorf("exec migration: %w", err)
	}
	return nil
}

// repoRoot ヘルパー：testdata の場所からプロジェクトルートを推定する。
func repoRoot() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	// tests/integration から上に上って go.mod がある場所を探す
	for dir := wd; dir != "/" && dir != "."; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
	}
	return "."
}

// MakeUser テスト用ユーザを 1 つ作成して返す。
func MakeUser(t *testing.T, env *Env) *mysql.User {
	t.Helper()
	// 鍵階層: KEK 32 bytes random + dev wrapper の 32 bytes MAC
	plain := make([]byte, 32)
	for i := range plain {
		plain[i] = byte(i*31 + 7) // 適当に固有な値
	}
	master := []byte("test-master-key-32-bytes--------")
	kekEnc := make([]byte, 64)
	copy(kekEnc[:32], plain)
	mac := sha256Hash(append(append([]byte("kek-dev:"), master...), plain...))
	copy(kekEnc[32:], mac)

	u := &mysql.User{
		ID:                uuid.New(),
		Email:             strings.ToLower(uuid.NewString()) + "@example.local",
		PasswordHash:      "$argon2id$v=19$m=65536,t=3,p=2$YWFhYWFhYWFhYWFhYWFhYQ$YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYQ", // dummy
		KEKEnc:            kekEnc,
		KEKID:             uuid.New(),
		MasterKeyVersion:  1,
		RecoveryCodesJSON: []byte("[]"),
		CreatedAt:         time.Now().UTC(),
	}
	if err := env.Users.Insert(context.Background(), u); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return u
}

// nowUTC 現在時刻を UTC で返す。
func nowUTC() time.Time { return time.Now().UTC() }

// newSession テスト用に有効な session を組み立てる。
func newSession(userID uuid.UUID, now time.Time) *mysql.Session {
	return &mysql.Session{
		ID:         uuid.New(),
		UserID:     userID,
		CreatedAt:  now,
		LastSeenAt: now,
		ExpiresAt:  now.Add(24 * time.Hour),
	}
}

// hmacSHA256Bytes middleware の SetSessionCookie と同じロジック。
func hmacSHA256Bytes(key []byte, parts ...[]byte) []byte {
	h := hmacFor(key)
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}
