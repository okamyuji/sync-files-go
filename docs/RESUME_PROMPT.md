# 再開プロンプト

新しいセッションで作業を再開するときに、このファイル全体を貼り付けてください。Claude Code は memory（`~/.claude/projects/-Users-yujiokamoto-devs-golang-sync-files-go/memory/`）も自動でロードします。

---

## このリポジトリの現状（2026-04-29 時点）

`/Users/yujiokamoto/devs/golang/sync-files-go` の Go プロジェクト。設計フェーズ完了 + Phase 1–4 実装完了。

- **コード**: 47 ファイル、6,621 行
- **テスト**: 単体 51 件、統合 18 件すべて pass
- **品質ゲート**: `go vet` / `staticcheck` / `golangci-lint` / `go test -race` 4 段すべて clean
- **コミット**: 21 件、`git log --oneline` で履歴を確認できる

完了済み Phase（[`docs/IMPLEMENTATION_PLAN.md`](./IMPLEMENTATION_PLAN.md) §Phase 一覧）:

- ✅ Phase 1: スケルトン・ローカル開発基盤
- ✅ Phase 2: コアドメイン・永続化層 + 単体テスト
- ✅ Phase 3: HTTP 層 + 統合テスト（middleware 9 種、MySQL repo 6 種、handler 3 種）
- ✅ Phase 4: バッチ・運用 CLI・補正ジョブ（`cmd/batch`、`cmd/sync-files-admin`）

未着手:

- ⏳ Phase 5: フロントエンド (HTMX) + Playwright E2E（11 spec）
- ⏳ Phase 6: Terraform IaC + EventBridge + 本番疎通テスト

---

## 機械的実装可否のレビュー

### Phase 5 の機械化度: **80%**（手動判断が必要な箇所あり）

機械的に書ける部分：

- `internal/ui/templates/` の HTML テンプレート骨格（`html/template`、HTMX 属性）
- `internal/ui/static/css/` のデザイントークン（OKLCH、流体タイポ、デザイン書 [`08-frontend-design.md`](./08-frontend-design.md) §5.2 の定数をそのまま実装）
- `tests/e2e/` の Playwright spec ひな型（[`11-testing-strategy.md`](./11-testing-strategy.md) §4.2 のシナリオ表 11 件をそのままテスト構造に）
- HTMX フォーム ↔ 既存 handler の結線（`/login`、`/api/files`、`/share/{token}` 等）

判断が必要：

- **デザインの最終的な見た目**（質素モダン）：色のアクセント・フォント選定・bento レイアウトの細部は人間レビュー必須。Anti-Pattern Policy（[`docs/08-frontend-design.md`](./08-frontend-design.md) §10）を回避するために、複数案出して選ぶことが推奨
- アクセシビリティ実機検証（VoiceOver / NVDA）

### Phase 6 の機械化度: **90%**（Terraform は機械的、運用部分のみ手動）

機械的に書ける部分：

- `deploy/terraform/modules/` の 7 モジュール（network / ecs / rds / s3files / ecr / secrets / batch / observability）。設計書 [`09-infrastructure-and-deployment.md`](./09-infrastructure-and-deployment.md) §4–§16 にすべての HCL 例が記載済み
- EventBridge Schedule 定義（`cmd/batch` の 4 サブコマンドを cron で起動、§10 §10）
- `deploy/terraform/envs/{dev,prod}/` の結線
- `deploy/scripts/setup-s3files.sh` の S3 Files CLI 作成スクリプト
- staging smoke + critical-path Playwright spec

判断が必要・手動操作必須：

- **AWS アカウント側の初期設定**: Terraform State 用 S3 バケット、ハードウェア MFA、IAM root の保護
- **Cloudflare アカウント設定**: Tunnel 作成 → 認証トークン取得 → Secrets Manager に投入（[`docs/10-operations.md`](./10-operations.md) §2.4）
- **Cloudflare DNS の zone 設定**: ドメイン名（`sync.example.com` などの実値）の確定
- **本番デプロイ**: `terraform apply` の判断は人間オペレータが行う
- **手動リアルブラウザ確認**: Phase 6 リリースゲートとして必須（Chrome / Safari / Mobile Safari の 3 ブラウザ）

### 結論

Phase 5 と Phase 6 のコード生成部分は機械的に進められますが、**デザイン微調整 / AWS・Cloudflare 初期設定 / リリース最終承認** は人間判断を残します。設計書ですべての構造・受け入れ基準が決まっているので、Claude が自律的に Phase 5 → Phase 6 まで進めて、節目ごとに人間レビューを挟む形が現実的です。

---

## 再開時の指示プロンプト（コピペで使う）

以下のプロンプトを Claude Code に貼り付けてください。

```
sync-files-go の実装を再開します。memory に保存した既存の決定事項（technical stack / feedback / gotchas）と、`docs/RESUME_PROMPT.md` を必ず最初に読み、次に `docs/IMPLEMENTATION_PLAN.md` に従って Phase 5 から進めてください。

前提:
- Phase 1–4 は完了済み（git log で確認）
- 設計書 docs/00–13 と adr/ADR-001–009 はすべて codex GO 判定取得済み
- 4 段の品質ゲート (go vet / staticcheck / golangci-lint / go test -race) と統合テスト 18 件はすべて green
- ローカル Docker と mysql:8.0 イメージは pull 済み

Phase 5 の進め方:
1. docs/08-frontend-design.md のデザイントークンを internal/ui/static/css/ に実装
2. internal/ui/templates/ に base.gohtml + partials + pages を作成
3. server.go から HTML レンダリング handler を結線（既存の handler_files/handler_auth/handler_share の HTML 版を作る、JSON 版は API として維持）
4. tests/e2e/ に Playwright 11 spec を追加（docs/11-testing-strategy.md §4.2）
5. 各段階でローカル `make compose-up` で動作確認、`make lint` と `make test` を通す
6. デザイン微調整は **私（ユーザ）に複数案を提示して合意取得** してから進める

Phase 6 の進め方:
1. docs/09-infrastructure-and-deployment.md の HCL 例をベースに deploy/terraform/modules/* を全部書き起こす
2. deploy/terraform/envs/{dev,prod}/ で modules を結線
3. EventBridge Schedule で `gc` / `prune-old-versions` / `reconcile-expired-uploads` の日次起動を設定
4. AWS アカウント側の初期設定（Terraform state S3、IAM root MFA、Cloudflare Tunnel 作成）は **私（ユーザ）に手順を渡して手動実行を依頼**
5. `terraform plan` を CI で必ず実行、`terraform apply` は人間承認必須

進め方の方針:
- 段階ごとに日本語コミット（AI クレジットなし）
- 各 Phase 完了後に codex レビューを依頼してから次へ
- 手動操作が必要な箇所は事前に私（ユーザ）に明示

auto mode で進めて構いません。詰まったり判断が必要になったら止めて私に確認してください。
```

---

## 補足：Phase 5 / 6 を始める前のチェックリスト

新しいセッションで開始する直前に：

- [ ] `cd /Users/yujiokamoto/devs/golang/sync-files-go`
- [ ] `git status` がクリーン（modified なし）
- [ ] `make test` と `make lint` が green
- [ ] `docker info` が正常応答（統合テストで使う）
- [ ] Phase 5 開始時：Node.js + Playwright の準備（`npm init` で `tests/e2e/`、`@playwright/test` インストール）
- [ ] Phase 6 開始時：Terraform 1.7+ がインストール済み（`terraform -version`）、AWS CLI 設定済み（`aws sts get-caller-identity`）、Cloudflare アカウント

---

## 関連ファイル（このリポジトリ内）

設計の正準：
- [`docs/00-overview.md`](./00-overview.md) ～ [`docs/13-risks-and-open-questions.md`](./13-risks-and-open-questions.md)
- [`docs/adr/ADR-001`](./adr/ADR-001-s3-files-vs-direct-s3.md) ～ [`docs/adr/ADR-009`](./adr/ADR-009-immediate-purge-exception.md)
- [`docs/IMPLEMENTATION_PLAN.md`](./IMPLEMENTATION_PLAN.md)

実装：
- `cmd/server/`、`cmd/batch/`、`cmd/sync-files-admin/`
- `internal/`（domain / crypto / repo / storage / http / batch / config / observability）
- `tests/integration/`
- `migrations/0001_init.sql`
- `deploy/docker/`（Dockerfile、nginx.conf、docker-compose.yml）
- `Makefile`、`.golangci.yml`、`go.mod`
