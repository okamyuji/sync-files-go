// Package config はアプリの環境変数から設定値を読み出す。
//
// 設計書 09-infrastructure-and-deployment.md §18 を正準とする。
// すべての値はプロセス起動時に一度だけ読み出してメモリに保持する。
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config はアプリ全体の設定。値はすべて起動時に確定する不変オブジェクト。
type Config struct {
	AppEnv            string
	Port              int
	DataDir           string
	BaseURL           string
	LogLevel          string
	MaxUploadBytes    int64
	RAWWindow         time.Duration
	ReplicaLagDegrade time.Duration

	DB DBConfig

	// 暗号鍵類（Secrets Manager から渡される base64 32 bytes）
	AESMasterKey []byte
	TOTPHMACKey  []byte
	CSRFKey      []byte
	SessionKey   []byte
}

// DBConfig MySQL Primary / Replica の接続情報。
type DBConfig struct {
	PrimaryHost string
	ReplicaHost string
	Port        int
	Name        string
	User        string
	Password    string
	TLS         string // "true" / "preferred" / "skip-verify"
}

// Load は環境変数から Config を組み立てる。必須項目が欠けていればエラー。
func Load() (*Config, error) {
	c := &Config{
		AppEnv:   getenv("APP_ENV", "local"),
		Port:     atoiDefault("PORT", 8080),
		DataDir:  getenv("DATA_DIR", "/var/data"),
		BaseURL:  getenv("BASE_URL", "http://localhost:8080"),
		LogLevel: getenv("LOG_LEVEL", "info"),
	}

	c.MaxUploadBytes = atoi64Default("MAX_UPLOAD_BYTES", 2*1024*1024*1024)
	c.RAWWindow = time.Duration(atoiDefault("READ_AFTER_WRITE_WINDOW_SECONDS", 5)) * time.Second
	c.ReplicaLagDegrade = time.Duration(atoiDefault("REPLICA_LAG_DEGRADE_SECONDS", 10)) * time.Second

	c.DB = DBConfig{
		PrimaryHost: getenv("DB_PRIMARY_HOST", "mysql"),
		ReplicaHost: getenv("DB_REPLICA_HOST", "mysql"),
		Port:        atoiDefault("DB_PORT", 3306),
		Name:        getenv("DB_NAME", "sync"),
		User:        getenv("DB_USER", "sync_app"),
		Password:    os.Getenv("DB_PASSWORD"),
		TLS:         getenv("DB_TLS", "preferred"),
	}

	keys := map[string]*[]byte{
		"AES_MASTER_KEY": &c.AESMasterKey,
		"TOTP_HMAC_KEY":  &c.TOTPHMACKey,
		"CSRF_KEY":       &c.CSRFKey,
		"SESSION_KEY":    &c.SessionKey,
	}
	for name, dst := range keys {
		raw := os.Getenv(name)
		if raw == "" {
			return nil, fmt.Errorf("config: %s is required", name)
		}
		decoded, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			return nil, fmt.Errorf("config: %s: %w", name, err)
		}
		if len(decoded) < 32 {
			return nil, fmt.Errorf("config: %s must be at least 32 bytes (base64)", name)
		}
		*dst = decoded
	}

	if c.DB.Password == "" && c.AppEnv != "local" {
		return nil, errors.New("config: DB_PASSWORD is required outside local")
	}

	return c, nil
}

// MySQLDSN database/sql で使う DSN 文字列を組み立てる (host=primary or replica)。
func (c *Config) MySQLDSN(host string) string {
	return fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=utf8mb4&parseTime=true&loc=UTC&multiStatements=false&tls=%s",
		c.DB.User, c.DB.Password, host, c.DB.Port, c.DB.Name, c.DB.TLS)
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func atoiDefault(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func atoi64Default(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}
