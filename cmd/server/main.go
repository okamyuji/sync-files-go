// sync-files-go HTTP サーバのエントリーポイント。
//
// Phase 1 ではヘルスチェックと最小限の起動シーケンスのみ。
// Phase 3 で internal/http のルータを結線する。
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/okamyuji/sync-files-go/internal/config"
	"github.com/okamyuji/sync-files-go/internal/observability"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// healthcheck サブコマンド (Dockerfile の HEALTHCHECK から呼ばれる想定)
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		return healthCheckClient()
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	logger := observability.NewLogger(cfg.LogLevel)
	logger.Info("starting sync-files-go",
		"env", cfg.AppEnv,
		"port", cfg.Port,
		"data_dir", cfg.DataDir,
	)

	mux := http.NewServeMux()

	// /healthz: アプリプロセスが生きているかだけ。依存はチェックしない (10-operations.md §1.4)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	// /readyz: 依存（DB / S3 Files）の到達確認。Phase 2 で実装拡張。
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           mux,
		ReadTimeout:       30 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      300 * time.Second, // 大容量アップロード考慮
		IdleTimeout:       120 * time.Second,
	}

	// graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", srv.Addr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("listen: %w", err)
	}
}

// healthCheckClient Dockerfile の `ENTRYPOINT ["/sync-files-go", "healthcheck"]` で
// 呼ばれるためのクライアント。`PORT` から組み立てた URL に対し /healthz を叩く。
// PORT は数値として厳格に検証してから URL に組み込む（gosec G704 対策）。
func healthCheckClient() error {
	port := 8080
	if v := os.Getenv("PORT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 || n > 65535 {
			return fmt.Errorf("healthcheck: invalid PORT %q", v)
		}
		port = n
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/healthz", port)
	cli := &http.Client{Timeout: 5 * time.Second}
	resp, err := cli.Get(url) // #nosec G107,G704 -- 127.0.0.1 + 検証済み数値 port のみ。SSRF にならない
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck: unexpected status %d", resp.StatusCode)
	}
	return nil
}
