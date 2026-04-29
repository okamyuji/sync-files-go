// sync-files-batch サブコマンド分岐エントリーポイント。
//
// 設計書 09-infrastructure-and-deployment.md §10 / 10-operations.md §4.3 に対応。
// EventBridge Schedule から ECS RunTask で起動される。
//
// 使用例:
//
//	sync-files-batch gc                       # ゴミ箱 30 日経過の物理削除
//	sync-files-batch prune-old-versions       # 90 日経過の旧バージョン削除（CR-5）
//	sync-files-batch reconcile-expired-uploads # tmp/* の 7 日 TTL 掃除
//	sync-files-batch metadata-orphans          # メタ ↔ S3 不整合検出
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/okamyuji/sync-files-go/internal/batch"
	"github.com/okamyuji/sync-files-go/internal/config"
	hsrv "github.com/okamyuji/sync-files-go/internal/http"
	"github.com/okamyuji/sync-files-go/internal/observability"
	"github.com/okamyuji/sync-files-go/internal/repo"
	"github.com/okamyuji/sync-files-go/internal/repo/mysql"
	"github.com/okamyuji/sync-files-go/internal/storage/localfs"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: sync-files-batch <gc|prune-old-versions|reconcile-expired-uploads|metadata-orphans>")
	}
	sub := args[0]

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	logger := observability.NewLogger(cfg.LogLevel)
	logger.Info("sync-files-batch start", "subcommand", sub, "env", cfg.AppEnv)

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

	files := mysql.NewFilesRepo(router)
	versions := mysql.NewFileVersionsRepo(router)
	audit := mysql.NewAuditRepo(router)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	// 1 ジョブ最大 30 分で打ち切り
	ctx, cancelTO := context.WithTimeout(ctx, 30*time.Minute)
	defer cancelTO()

	switch sub {
	case "gc":
		gc := &batch.GarbageCollector{
			Router: router, Files: files, FileVersions: versions, Audit: audit, Storage: store,
			Logger: logger, RetentionDays: 30, BatchSize: 1000,
		}
		n, err := gc.Run(ctx)
		logger.Info("gc completed", "purged", n)
		return err

	case "prune-old-versions":
		p := &batch.OldVersionPruner{
			FileVersions: versions, Audit: audit, Storage: store,
			Logger: logger, RetentionDays: 90, BatchSize: 1000,
		}
		n, err := p.Run(ctx)
		logger.Info("prune-old-versions completed", "pruned", n)
		return err

	case "reconcile-expired-uploads":
		r := &batch.Reconciler{
			Router: router, FileVersions: versions, Audit: audit, Storage: store, Logger: logger,
		}
		n, err := r.CleanupExpiredUploads(ctx)
		logger.Info("reconcile-expired-uploads completed", "cleaned", n)
		return err

	case "metadata-orphans":
		r := &batch.Reconciler{
			Router: router, FileVersions: versions, Audit: audit, Storage: store, Logger: logger,
		}
		n, err := r.MetadataOrphans(ctx, 100)
		logger.Info("metadata-orphans completed", "found", n)
		return err
	}

	return fmt.Errorf("unknown subcommand: %s", sub)
}
