// sync-files-go HTTP サーバのエントリーポイント。
//
// Phase 3: DB（MySQL Primary/Replica）と Storage（S3 Files NFS マウント or local FS）を結線し、
// internal/http/server.go の NewServer を起動する。
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

	_ "github.com/go-sql-driver/mysql"

	"github.com/okamyuji/sync-files-go/internal/config"
	hsrv "github.com/okamyuji/sync-files-go/internal/http"
	"github.com/okamyuji/sync-files-go/internal/observability"
	"github.com/okamyuji/sync-files-go/internal/repo"
	"github.com/okamyuji/sync-files-go/internal/repo/mysql"
	"github.com/okamyuji/sync-files-go/internal/storage/localfs"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

//nolint:gocyclo // 主に依存性注入の結線。意味のある分割は Phase 5 以降で
func run() error {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		return healthCheckClient()
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	logger := observability.NewLogger(cfg.LogLevel)
	logger.Info("starting sync-files-go",
		"env", cfg.AppEnv, "port", cfg.Port, "data_dir", cfg.DataDir,
	)

	primary, replica, err := hsrv.OpenDBs(cfg)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() {
		_ = primary.Close()
		_ = replica.Close()
	}()

	router := repo.NewDBRouter(primary, replica)

	store, err := localfs.New(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("init storage: %w", err)
	}

	deps := &hsrv.Deps{
		Cfg:          cfg,
		Logger:       logger,
		Router:       router,
		Storage:      store,
		Users:        mysql.NewUsersRepo(router),
		Sessions:     mysql.NewSessionsRepo(router),
		Files:        mysql.NewFilesRepo(router),
		FileVersions: mysql.NewFileVersionsRepo(router),
		ShareLinks:   mysql.NewShareLinksRepo(router),
		Audit:        mysql.NewAuditRepo(router),
	}

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           hsrv.NewServer(deps),
		ReadTimeout:       30 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      300 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

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
