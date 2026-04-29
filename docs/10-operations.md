# 10. 運用

> 「設計どおりに作る」だけでなく、「設計どおりに動かし続ける」ための章。

## 1. 観測性（Observability）

### 1.1 ログ

- フォーマット: JSON 構造化（`slog`）
- 必須フィールド: `time`, `level`, `msg`, `request_id`, `user_id`, `route`
- 機密情報は出さない（[`07-security.md`](./07-security.md) §9）
- ログレベル: `info` 既定、`debug` は環境変数で切替

例：

```json
{"time":"2026-04-29T05:32:11Z","level":"info","msg":"file.upload.complete","request_id":"r-abc","user_id":"u-123","file_id":"f-456","size":84211,"version_id":"v-789"}
```

### 1.2 メトリクス

CloudWatch Embedded Metric Format（EMF）でアプリから出力：

| メトリクス | 単位 | アラート |
|---|---|---|
| `Uploads.Started` | Count | — |
| `Uploads.Completed` | Count | — |
| `Uploads.Failed` | Count | > 5 / 5min |
| `Conflicts.Detected` | Count | — (情報) |
| `Conflicts.Resolved.SaveAsCopy` | Count | — |
| `Conflicts.Resolved.ForceOverwrite` | Count | — |
| `Trash.Restored` | Count | — |
| `PhysicalDeletes` | Count | — |
| `OrphanedFilesDetected` | Count | > 0 / 24h |
| `SSEConnections.Open` | Count | — |
| `Auth.Failures.PerUser` | Count | > 10 / 15min |
| `Auth.Failures.PerIP` | Count | > 100 / 15min |
| `RequestDuration.p95` | Milliseconds | > 1000 |
| `RDSConnPool.Utilization` | Percent | > 90 |

### 1.3 トレーシング

v1 では分散トレーシングは導入しない（単一サービスのため）。`request_id` の伝播のみで十分。v2 で OpenTelemetry を検討。

### 1.4 ヘルスチェック

```
GET /healthz
  → 200: アプリプロセスが生きている (即時応答、依存はチェックしない)

GET /readyz
  → 200: DB 接続 OK + S3 Files マウント OK
  → 503: 何か NG
```

ALB は `/healthz` を使用。Kubernetes 流の readiness は使わないが、`/readyz` は手動確認用に提供。

## 2. デプロイ手順

### 2.1 通常デプロイ

```bash
# 1. ローカルでテスト
make test
make lint

# 2. Docker イメージビルド & プッシュ
export IMAGE_TAG=$(git rev-parse --short HEAD)-$(date +%Y%m%d%H%M)
make docker-build TAG=$IMAGE_TAG
make ecr-login
make docker-push TAG=$IMAGE_TAG

# 3. Terraform plan & apply
cd deploy/terraform/envs/prod
terraform plan -var "image_tag=$IMAGE_TAG" -out=plan.out
terraform apply plan.out

# 4. ECS Service が rolling update を実行
# 5. デプロイ後、smoke test
make smoke-test BASE_URL=https://sync.example.com
```

### 2.2 ロールバック

```bash
# 前回の image tag に戻す
PREV_TAG=$(aws ecr describe-images \
   --repository-name sync-files-go \
   --query 'sort_by(imageDetails,&imagePushedAt)[-2].imageTags[0]' \
   --output text)

cd deploy/terraform/envs/prod
terraform apply -var "image_tag=$PREV_TAG"

# DB マイグレーションの forward-only 性が崩れた場合の手順は §3.2
```

### 2.3 S3 Files の初期セットアップ（Terraform 未対応分）

```bash
# 1. S3 バケット作成 (Terraform で済んでいる)
# 2. S3 Files ファイルシステム作成 (CLI)
aws s3files create-file-system \
  --bucket-name sync-files-go-prod-data-XXXXX \
  --region ap-northeast-1 \
  --tags Key=Project,Value=sync-files-go

# 3. Access Point 作成
aws s3files create-access-point \
  --file-system-id <fs-id> \
  --name app-rw \
  --posix-user "uid=65532,gid=65532" \
  --root-directory "/"

# 4. ARN を控え、Terraform variables.tfvars に書く
```

将来 Terraform が対応したら IaC に取り込む。

## 3. データベースマイグレーション

### 3.1 ツール

- 標準ライブラリ志向で `golang-migrate/migrate` を採用（`database/sql` を直接使う薄いラッパ）
- マイグレーションは `up` のみ前提（forward-only）
- `down` は緊急用に提供するが、本番では使わない
- ランタイムから自動マイグレーションは「初回起動 + 環境変数 `AUTO_MIGRATE=1`」のみ。通常は別タスクで実行

### 3.2 マイグレーション戦略

「アプリのデプロイ前に SQL を流す」のではなく、

1. **互換性のある DDL** を先に流す（カラム追加、index 追加）
2. アプリの新版をデプロイ（旧スキーマでも動く）
3. 旧コードに依存していたカラム削除は次回リリースで（バッチで `migrate v1.2.x → 1.3.x`）

これにより rolling deploy 中にスキーマ不整合で 5xx が出ない。

### 3.3 マイグレーション実行コマンド

```bash
make db-migrate ENV=prod    # ECS RunTask でマイグレーションタスクを起動
```

## 4. バックアップとリストア

### 4.1 自動バックアップ

| 対象 | 方式 | 保持期間 |
|---|---|---|
| RDS | 自動バックアップ + PITR | 30 日 |
| RDS | 手動スナップショット（月 1） | 12 ヶ月 |
| S3 Files | バケットバージョニング | 90 日（旧版） |
| Terraform state | S3 backend + バージョニング | 無期限 |
| シークレット | Secrets Manager の自動世代管理 | 30 日 |

### 4.2 リストア手順

#### RDS PITR（30 日以内）

```bash
aws rds restore-db-instance-to-point-in-time \
  --source-db-instance-identifier sync-files-go-prod \
  --target-db-instance-identifier sync-files-go-prod-restored \
  --restore-time 2026-04-28T18:00:00Z \
  --db-subnet-group-name <subnet-group> \
  --vpc-security-group-ids <sg-id> \
  --no-publicly-accessible

# 検証
psql -h <new-endpoint> -U rdsadmin -d sync -c "SELECT count(*) FROM files;"

# 切替
# 1. アプリの DB_HOST を新エンドポイントに変更（Secrets / 環境変数）
# 2. ECS service を新タスクで起動
# 3. 旧 DB を削除前に 7 日待つ
```

#### S3 Files / S3 バケットからの個別ファイル復元

```bash
# S3 上で「以前のバージョン」を確認
aws s3api list-object-versions \
  --bucket sync-files-go-prod-data-XXXXX \
  --prefix "owner-<user-uuid>/current/" \
  --max-items 50

# 旧版を復元
aws s3 cp \
  s3://sync-files-go-prod-data-XXXXX/owner-../current/<file-uuid> \
  s3://sync-files-go-prod-data-XXXXX/owner-../current/<file-uuid> \
  --version-id <past-version-id>
```

ただしアプリ側のメタデータ整合性も必要なので、運用 CLI コマンドを用意（次節）。

### 4.3 運用 CLI

`bin/sync-files-admin` という CLI を提供：

```
sync-files-admin restore-file --user <id> --file <id> --to-version <s3-version-id>
sync-files-admin restore-purged --user <id> --file <id> --before <ts>
sync-files-admin reconcile-orphans --dry-run
sync-files-admin export-user-data --user <id> --out <dir>
sync-files-admin rotate-aes-key --new-master-key <base64>
```

CLI は ECS Exec から実行し、操作はすべて `audit_logs` に `actor_kind='system'` で記録。

## 5. 障害対応 Runbook

### 5.1 共通

1. アラート受信
2. 直近 30 分のログを CloudWatch Logs Insights で確認
3. 影響範囲を特定（影響ユーザ数、操作種別、時間帯）
4. 必要なら ECS Exec で内部調査
5. 緊急対応 → 根本原因分析 → 恒久対策

### 5.2 ケース別

#### A. アプリが応答しない（5xx 100% / 1 分継続）

```
1. ECS Service の Status を確認
2. タスクが再起動を繰り返している?
   YES → CloudWatch Logs で起動エラー確認
   NO  → ALB の Target Group 健全性確認
3. RDS が落ちている? → RDS Status を確認
4. S3 Files が落ちている? → /var/data でファイル read を試す
5. Secret 取得失敗? → IAM ロールと Secret ARN を確認
6. 暫定: 直近のイメージにロールバック
```

#### B. ファイルが消えた（ユーザ報告）

```
1. アクティビティタイムラインで該当ファイル ID の操作を時系列確認
2. file.delete があれば: ゴミ箱から復元（30 日以内）
3. file.purge があれば: §4.2 で S3 旧版から復元
4. ない: 補正ジョブログ確認 → /_orphan/ にあれば再登録
5. それでもなければ: RDS PITR
6. 最終的に S3 バケットバージョニングを直接調査
```

#### C. アップロードが必ず失敗する

```
1. Network: ALB ヘルスチェック OK?
2. Storage: S3 Files マウント OK? df -h /var/data
3. DB: 接続プール枯渇? select count(*) from pg_stat_activity;
4. アプリ: アップロード失敗ログを確認
5. 暫定: タスクを再起動 (ECS service update --force-new-deployment)
```

#### D. ログイン突破試行（ブルートフォース）

```
1. CloudWatch アラート (Auth.Failures.PerIP > 100/15min) を確認
2. 攻撃 IP を特定 → ALB の listener rule で deny ルール追加
3. 攻撃された account の確認 → 必要ならロック延長
4. AWS WAF を有効化検討（v2）
```

#### E. AWS 課金高騰

```
1. Cost Explorer で内訳確認
2. 上昇要因: データ転送 / S3 Files 同期リクエスト / Fargate オートスケール
3. 短期: AutoScaling 上限を一時的に下げる
4. 長期: コスト最適化（後述 §8）
```

### 5.3 完全障害時の手動切替

リージョン全域障害の場合、v1 は復旧待ち（NG-6）。v2 でクロスリージョン候補。

## 6. 鍵ローテーション手順

### 6.1 セッション署名鍵 / CSRF 鍵（3 ヶ月ごと）

```
1. 新しい鍵を Secrets Manager に追加（旧鍵も並存）
2. アプリは「新鍵で署名、新鍵 or 旧鍵で検証」する設定で再起動
3. 7 日後、旧鍵を Secrets Manager から削除
```

### 6.2 マスタ鍵（6 ヶ月ごと）

```
1. 新マスタ鍵を生成 (32 bytes random)
2. 運用 CLI: sync-files-admin rotate-aes-key --new-master-key <base64>
3. CLI が全ユーザの KEK を新マスタ鍵で再ラップ（DEK・ファイル本体は触らない）
4. 完了後、Secrets Manager のマスタ鍵を新値に更新
5. アプリ再起動
```

### 6.3 DB パスワード（12 ヶ月ごと）

```
1. RDS の Modify DB Instance で新パスワードに変更
2. Secrets Manager の値を更新
3. ECS Service の Force New Deployment（タスクが再起動して新 secret を取得）
4. 旧コネクションは切断される
```

## 7. 容量管理

### 7.1 監視

- ECS タスク: CPU / Memory / Disk I/O
- RDS: ストレージ / 接続数 / Performance Insights
- S3 / S3 Files: 容量 / リクエスト数 / コスト
- ALB: LCU 消費

### 7.2 容量逼迫時の対応

```
[ECS]   → desiredCount 増 (AutoScaling 上限見直し)
[RDS]   → instance class アップ (db.t4g.micro → small)
        → ストレージ自動拡張は 100GB 上限で済むかチェック
[S3]    → 古いバージョンの保持期間短縮
        → ライフサイクルポリシー見直し
```

### 7.3 ユーザ容量制限

`users` テーブルに `quota_bytes` 列を追加（v1 はデフォルト 500GB）。アップロード時に集計してチェック。

## 8. コスト最適化

| 施策 | 効果 |
|---|---|
| Fargate を arm64 にする | -20% |
| Savings Plans (Compute) | -30% |
| S3 Files のスループットモード Bursting | -40% |
| 古いバージョンの保持期間を短縮 | -10% |
| RDS db.t4g.micro Multi-AZ → Single-AZ | -50%（ただし可用性低下） |

v1 は Multi-AZ を維持。コスト >$200/月 になったら Savings Plans を検討。

## 9. 定期メンテナンスタスク

| 頻度 | タスク |
|---|---|
| 日次 | バッチ：物理削除 / 補正ジョブ実行 |
| 日次 | アクセスログ確認（異常検知） |
| 週次 | CloudWatch アラート履歴レビュー |
| 月次 | RDS 手動スナップショット |
| 月次 | コスト確認 |
| 月次 | DR リハーサル（PITR を一度試す） |
| 四半期 | セッション鍵ローテーション |
| 四半期 | 依存ライブラリ更新（go.mod） |
| 半期 | マスタ鍵ローテーション |
| 半期 | セキュリティスキャン全体（OWASP ZAP） |
| 年次 | DB パスワードローテーション |
| 年次 | 全体構成の見直し（このドキュメント自体） |

## 10. 緊急連絡

- アラート先: 自分のメール、SMS（SNS 経由）、PushOver / ntfy（v2 候補）
- AWS サポート: Developer プラン以上を推奨

## 11. 退役（廃止）手順

サービスを終了する場合：

```
1. 全データを sync-files-admin export-user-data でローカル NAS にエクスポート
2. ユーザに通知（自分なのでセルフトーク）
3. Terraform destroy（ただし RDS の deletion_protection を一時オフ）
4. AWS アカウントの請求停止確認
5. 関連 Route 53 レコード削除、ドメイン解放
6. CloudTrail のログだけは法的保管要件があるなら別アカウントへ移動
```

---

次の章: [`11-testing-strategy.md`](./11-testing-strategy.md)
