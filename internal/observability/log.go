// Package observability はログ・メトリクスのユーティリティ。
package observability

import (
	"log/slog"
	"os"
	"strings"
)

// NewLogger LOG_LEVEL に応じて JSON 構造化ロガーを返す。
func NewLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl, AddSource: false})
	return slog.New(h)
}
