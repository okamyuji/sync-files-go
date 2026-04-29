// sync-files-admin 運用 CLI（ECS Exec 経由でのみ実行可、HTTP に出さない）。
//
// 設計書 10-operations.md §4.3 / ADR-009 を正準とする。
//
// 使用例:
//
//	sync-files-admin force-purge --user <uuid> --file <uuid>     # ADR-009 例外
//	sync-files-admin reconcile-orphans --dry-run                  # 補正ジョブ起動
//	sync-files-admin prune-old-versions --dry-run                 # 旧版 prune
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	_ "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"

	"github.com/okamyuji/sync-files-go/internal/config"
	hsrv "github.com/okamyuji/sync-files-go/internal/http"
	"github.com/okamyuji/sync-files-go/internal/observability"
	"github.com/okamyuji/sync-files-go/internal/repo"
	"github.com/okamyuji/sync-files-go/internal/repo/mysql"
	"github.com/okamyuji/sync-files-go/internal/storage"
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
		return fmt.Errorf("usage: sync-files-admin <force-purge|reconcile-orphans|prune-old-versions|export-user-data>")
	}

	switch args[0] {
	case "force-purge":
		return runForcePurge(args[1:])
	case "reconcile-orphans":
		return runReconcile(args[1:])
	case "prune-old-versions":
		return runPrune(args[1:])
	}
	return fmt.Errorf("unknown subcommand: %s", args[0])
}

// runForcePurge ADR-009: INV-1 の例外として、特定のファイルを即時物理削除する。
// 必須: --user, --file。確認プロンプト + 「DELETE」入力で実行。
func runForcePurge(args []string) error {
	fs := flag.NewFlagSet("force-purge", flag.ExitOnError)
	user := fs.String("user", "", "owner user UUID (required)")
	file := fs.String("file", "", "file UUID (required)")
	noConfirm := fs.Bool("yes", false, "skip confirmation (CI のみ)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *user == "" || *file == "" {
		return fmt.Errorf("--user and --file are required")
	}
	userID, err := uuid.Parse(*user)
	if err != nil {
		return fmt.Errorf("invalid user UUID: %w", err)
	}
	fileID, err := uuid.Parse(*file)
	if err != nil {
		return fmt.Errorf("invalid file UUID: %w", err)
	}

	if !*noConfirm {
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "⚠️  ADR-009: 即時物理削除（INV-1 の例外）")
		fmt.Fprintln(os.Stderr, "  この操作は取り消せません。S3 上のすべてのバージョンが削除されます。")
		fmt.Fprintf(os.Stderr, "  user: %s\n", *user)
		fmt.Fprintf(os.Stderr, "  file: %s\n\n", *file)
		fmt.Fprint(os.Stderr, "確認のため大文字で 'DELETE' と入力してください: ")
		reader := bufio.NewReader(os.Stdin)
		input, _ := reader.ReadString('\n')
		if strings.TrimSpace(input) != "DELETE" {
			return fmt.Errorf("aborted")
		}
	}

	deps, err := loadDeps()
	if err != nil {
		return err
	}
	defer deps.close()

	ctx := context.Background()
	versions, err := deps.versions.ListByFile(ctx, fileID, 1000)
	if err != nil {
		return fmt.Errorf("list versions: %w", err)
	}

	for _, v := range versions {
		if err := deps.store.RemoveVersion(ctx, userID.String(), fileID.String(), v.ID.String()); err != nil &&
			!strings.Contains(err.Error(), "not found") {
			deps.logger.Warn("remove version failed", "version_id", v.ID, "err", err)
		}
		if err := deps.versions.Delete(ctx, v.ID); err != nil {
			deps.logger.Warn("delete version row failed", "version_id", v.ID, "err", err)
		}
	}

	// files の物理 DELETE（ADR-009: trashed/active 関係なく強制）
	if _, err := deps.router.Writer(ctx).ExecContext(ctx,
		`DELETE FROM files WHERE id_bin = ?`, fileID[:]); err != nil {
		return fmt.Errorf("delete files: %w", err)
	}

	_ = deps.audit.Insert(ctx, nil, &mysql.AuditEntry{
		ActorID:      &userID,
		ActorKind:    mysql.ActorSystem,
		Action:       "file.force_purge",
		TargetKind:   "file",
		TargetID:     &fileID,
		Irreversible: true,
		Details: map[string]any{
			"versions_removed": len(versions),
			"reason":           "ADR-009 admin CLI",
		},
	})

	fmt.Printf("force-purged file %s (versions=%d)\n", fileID, len(versions))
	return nil
}

func runReconcile(args []string) error {
	fs := flag.NewFlagSet("reconcile-orphans", flag.ExitOnError)
	dryRun := fs.Bool("dry-run", true, "dry-run (default true; no destructive ops)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	deps, err := loadDeps()
	if err != nil {
		return err
	}
	defer deps.close()

	if *dryRun {
		fmt.Println("dry-run: メタ・ストレージの不整合検出のみ実施し、修復はしません")
	}
	fmt.Println("reconcile は薄いラッパーです。本番の補正は 'sync-files-batch metadata-orphans' を直接実行してください")
	return nil
}

func runPrune(args []string) error {
	fs := flag.NewFlagSet("prune-old-versions", flag.ExitOnError)
	dryRun := fs.Bool("dry-run", true, "dry-run (default true)")
	days := fs.Int("days", 90, "retention days")
	if err := fs.Parse(args); err != nil {
		return err
	}
	deps, err := loadDeps()
	if err != nil {
		return err
	}
	defer deps.close()
	ctx := context.Background()

	candidates, err := deps.versions.FindPrunable(ctx, *days, 1000)
	if err != nil {
		return err
	}
	fmt.Printf("found %d prunable versions (>= %d days, non-current)\n", len(candidates), *days)
	for _, c := range candidates {
		fmt.Printf("  - file=%s version=%s key=%s\n", c.FileID, c.ID, c.StorageKey)
	}
	if *dryRun {
		fmt.Println("dry-run mode: nothing deleted")
		return nil
	}
	fmt.Println("実際の削除は 'sync-files-batch prune-old-versions' で実施してください（同じ DB 設定で）")
	return nil
}

// adminDeps CLI 共通の依存（DB / Storage / Audit）。
type adminDeps struct {
	cfg      *config.Config
	logger   *slog.Logger
	router   *repo.DBRouter
	versions *mysql.FileVersionsRepo
	audit    *mysql.AuditRepo
	store    storage.Storage
	closeFns []func()
}

func (d *adminDeps) close() {
	for _, f := range d.closeFns {
		f()
	}
}

func loadDeps() (*adminDeps, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	logger := observability.NewLogger(cfg.LogLevel)
	primary, replica, err := hsrv.OpenDBs(cfg)
	if err != nil {
		return nil, err
	}
	router := repo.NewDBRouter(primary, replica)
	store, err := localfs.New(cfg.DataDir)
	if err != nil {
		_ = primary.Close()
		_ = replica.Close()
		return nil, err
	}
	return &adminDeps{
		cfg:      cfg,
		logger:   logger,
		router:   router,
		versions: mysql.NewFileVersionsRepo(router),
		audit:    mysql.NewAuditRepo(router),
		store:    store,
		closeFns: []func(){
			func() { _ = primary.Close() },
			func() { _ = replica.Close() },
		},
	}, nil
}
