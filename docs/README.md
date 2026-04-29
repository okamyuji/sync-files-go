# 設計ドキュメント目次

本ディレクトリは sync-files-go の設計文書群です。**MECE（漏れなくダブりなく）** を意識したロジックツリー構造で、各章が独立して読める粒度で分割されています。

## 読み順（推奨）

| # | 文書 | 章の目的 | 想定読者 |
|---|---|---|---|
| 0 | [00-overview.md](./00-overview.md) | プロダクト概要・ゴール・非ゴール・用語定義 | 全員 |
| 1 | [01-requirements.md](./01-requirements.md) | 機能要件・非機能要件・要件のロジックツリー | 全員 |
| 2 | [02-architecture.md](./02-architecture.md) | 全体アーキテクチャ・コンポーネント・データフロー | 設計者 / インフラ |
| 3 | [03-domain-model.md](./03-domain-model.md) | ドメインモデル・ER 図・MySQL スキーマ・S3 Files 配置 | 実装者 / DBA |
| 4 | [04-sync-semantics.md](./04-sync-semantics.md) | **本システムの最重要章。** 同期セマンティクス・OCC・競合解決 | 全員 |
| 5 | [05-file-operations-logic-tree.md](./05-file-operations-logic-tree.md) | ファイル操作（アップロード／ダウンロード／更新／削除／リネーム／共有）の MECE ロジックツリー | 実装者 |
| 6 | [06-data-loss-prevention.md](./06-data-loss-prevention.md) | ファイル損失防止戦略の多層防御・損失シナリオ別対策 | 全員 |
| 7 | [07-security.md](./07-security.md) | 認証・認可・暗号化・CSRF・レート制限・公開リンクのセキュリティ | セキュリティ担当 |
| 8 | [08-frontend-design.md](./08-frontend-design.md) | HTMX 中心の UI・画面遷移・モダンで質素なデザイン方針・アクセシビリティ | フロント実装者 |
| 9 | [09-infrastructure-and-deployment.md](./09-infrastructure-and-deployment.md) | Docker・Terraform・ECR・ECS Fargate・ネットワーク・IAM | インフラ担当 |
| 10 | [10-operations.md](./10-operations.md) | 監視・ログ・バックアップ・障害対応 Runbook | 運用担当 |
| 11 | [11-testing-strategy.md](./11-testing-strategy.md) | 単体・統合・E2E・障害シナリオ・セキュリティテスト | QA / 実装者 |
| 12 | [12-roadmap.md](./12-roadmap.md) | v1 → v2 → v3 のマイルストーン | プロダクト |
| 13 | [13-risks-and-open-questions.md](./13-risks-and-open-questions.md) | リスク台帳とオープンイシュー | 全員 |

## ADR（Architecture Decision Records）

主要な技術選択の意思決定記録は [`adr/`](./adr/) を参照：

- [ADR-001](./adr/ADR-001-s3-files-vs-direct-s3.md): S3 Files (NFS) を選択し、S3 SDK 直接利用は最小化
- [ADR-002](./adr/ADR-002-rds-mysql-not-self-hosted.md): メタデータ DB は RDS for MySQL（自前構築・SQLite を不採用、PostgreSQL 案から変更）
- [ADR-003](./adr/ADR-003-occ-not-last-write-wins.md): 同期は OCC + コンフリクトコピー（後勝ち / 先勝ち を不採用）
- [ADR-004](./adr/ADR-004-soft-delete-30-days.md): 削除はゴミ箱 30 日 + S3 バージョニングの二段ガード
- [ADR-005](./adr/ADR-005-server-side-encryption-aes-gcm.md): 保存時暗号化は AES-256-GCM（アプリ層）+ S3 SSE の多重防御
- [ADR-006](./adr/ADR-006-htmx-not-spa.md): フロントは HTMX（SPA を不採用）
- [ADR-007](./adr/ADR-007-cloudflare-tunnel-not-alb.md): 外部公開は Cloudflare Tunnel + サイドカー nginx（ALB を不採用）
- [ADR-008](./adr/ADR-008-mysql-read-replica-write-ahead.md): MySQL の読み書き分離（Primary Write / Read Replica）と DBRouter 設計

## 設計の不変条件（最重要）

すべての章は次の 5 つの不変条件を前提として書かれています。実装中に「不変条件と矛盾するコード」を見つけたら、**コードではなく設計書側を疑う前に、まずコードを止めてください**。

| ID | 不変条件 |
|---|---|
| INV-1 | active からの即時物理削除は禁止。trashed を経た上で「パスワード再入力済みの明示 purge」または「30 日経過バッチ」でのみ物理削除可 |
| INV-2 | すべての書き込みは累積的（上書きでも S3 バージョニングで旧版が必ず残る） |
| INV-3 | サーバは自動マージを行わない（バイトレベルでも文字列レベルでも） |
| INV-4 | 未完了アップロードは正規パスに反映されない（一時領域のみ） |
| INV-5 | 破壊的操作（強制上書き、ゴミ箱の強制空にする等）は UI 上の確認モーダルなしには実行されない |

## 用語

- **同期** = サーバを介した片方向書き込みの累積。端末間の双方向自動マージは含まない。
- **OCC** = Optimistic Concurrency Control。事前ロックではなく `If-Match` で衝突検出。
- **コンフリクトコピー** = 競合時に旧版／新版の両方を別名で保存する戦略。
- **ソフト削除** = メタデータ上で `deleted_at` を立てるだけで、S3 上のオブジェクトには手をつけない削除。
- **CAS** = Content Addressable Storage（本設計では明示的には採用していない。理由は ADR-001）。

## 文書のメンテナンス方針

- 各章は独立して読める。クロスリファレンスは markdown リンクで張る。
- 設計変更は ADR を新規追加し、既存章の該当箇所に「ADR-XXX により変更」と記す。既存 ADR は **削除しない**。
- リスク・未解決事項は [`13-risks-and-open-questions.md`](./13-risks-and-open-questions.md) に集約。
