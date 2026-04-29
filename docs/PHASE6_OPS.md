# Phase 6 運用ドキュメント

> Terraform 適用前後で人間が手動実行する手順。`make tf-validate` までは Claude / CI が自動化済み。

## 1. AWS アカウント側の前提

### 1.1 IAM 設定

- root ユーザに **ハードウェア MFA** を装着
- 普段使う管理 IAM ユーザに **Administrator** を付与（root は緊急用に温存）
- AWS CLI に管理者プロファイルを設定 (`aws configure --profile sync-admin`)

### 1.2 Terraform state 用 S3 + DynamoDB（chicken-and-egg のため手動作成）

```bash
AWS_REGION=ap-northeast-1
ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)

# dev
aws s3api create-bucket \
  --bucket sync-files-go-tfstate-dev \
  --region $AWS_REGION \
  --create-bucket-configuration LocationConstraint=$AWS_REGION
aws s3api put-bucket-versioning \
  --bucket sync-files-go-tfstate-dev \
  --versioning-configuration Status=Enabled
aws s3api put-bucket-encryption \
  --bucket sync-files-go-tfstate-dev \
  --server-side-encryption-configuration \
  '{"Rules":[{"ApplyServerSideEncryptionByDefault":{"SSEAlgorithm":"AES256"}}]}'

# prod も同様 (sync-files-go-tfstate-prod)

# DynamoDB ロックテーブル (dev/prod 共有でよい)
aws dynamodb create-table \
  --region $AWS_REGION \
  --table-name sync-files-go-tfstate-lock \
  --attribute-definitions AttributeName=LockID,AttributeType=S \
  --key-schema AttributeName=LockID,KeyType=HASH \
  --billing-mode PAY_PER_REQUEST
```

### 1.3 ドメインと Cloudflare Zone

- Cloudflare アカウントを作成済みであること
- ドメイン（例: `example.com`）を Cloudflare DNS にネームサーバ移譲済みであること
- 「Universal SSL」を Full(strict) に設定

## 2. 初回適用フロー（dev で先に練習、prod は同じ手順を繰り返す）

### 2.1 Terraform 初期適用（基盤層）

`network`、`ecr`、`secrets`、`s3files` だけ先に作る：

```bash
cd deploy/terraform/envs/dev
cp terraform.tfvars.example terraform.tfvars
# image_tag は適当な値でよい (この時点では ECS は作らない)

terraform init
terraform apply \
  -target=module.network \
  -target=module.ecr \
  -target=module.secrets \
  -target=module.s3files
```

### 2.2 シークレット投入

```bash
../../../scripts/seed-secrets.sh dev
# → 5 つのキーは自動生成、Cloudflared token は対話入力
```

### 2.3 EFS（永続データボリューム）作成

```bash
../../../scripts/setup-s3files.sh dev
# → 最後に echo されている EFS ID と AccessPoint ID を terraform.tfvars に書き込む
```

### 2.4 Cloudflare Tunnel セットアップ

1. Cloudflare Zero Trust ダッシュボードを開く（https://one.dash.cloudflare.com/）
2. **Networks > Tunnels > Create a tunnel**
3. 名前: `sync-files-go-dev`、Service token を Docker でセットアップする方を選ぶ
4. **Save tunnel** で表示されるトークン（base64）をコピー
5. `seed-secrets.sh` の対話プロンプトで貼り付け（後から `--force` で上書き可）
6. **Public Hostname** タブで:
   - Subdomain: `sync-dev`
   - Domain: `example.com`
   - Service Type: `HTTPS`
   - URL: `localhost:8443`
   - **Additional application settings > TLS > No TLS Verify** を ON（ECS タスク内 nginx が自己署名のため）

### 2.5 RDS / ECS / Batch 適用

```bash
terraform apply
# → 10 〜 20 分（RDS Multi-AZ 起動に時間がかかる）
```

### 2.6 ECR にイメージを push

```bash
ECR=$(terraform output -raw ecr_app_url)
NGX=$(terraform output -raw ecr_nginx_url)
TAG="$(git rev-parse --short HEAD)-$(date +%Y%m%d%H%M)"

aws ecr get-login-password --region ap-northeast-1 | docker login --password-stdin --username AWS $(echo $ECR | cut -d/ -f1)

docker buildx build --platform=linux/arm64 -f deploy/docker/app.Dockerfile   -t $ECR:$TAG --push .
docker buildx build --platform=linux/arm64 -f deploy/docker/nginx.Dockerfile -t $NGX:$TAG --push .

# tfvars の image_tag を $TAG に更新して再 apply (ECS task definition 更新)
terraform apply -var="image_tag=$TAG"
```

### 2.7 DB マイグレーション + ユーザ作成

ECS タスクから手動 RunTask で `migrations/*.sql` を流すか、踏み台 EC2 経由で `mysql` クライアントで適用する。`migrations/000_create_users.sql`（要追加）で `sync_app` / `sync_migrate` を作成。

### 2.8 リリースゲート

```bash
# staging で smoke テスト
BASE_URL=https://sync-dev.example.com make release-smoke
# critical-path テスト（事前に E2E 用ユーザを 1 件作成しておく）
E2E_TEST_EMAIL=e2e@example.com \
E2E_TEST_PASSWORD=...           \
BASE_URL=https://sync-dev.example.com make release-critical-path
```

両方 green になったら、prod に対して同じ手順を繰り返す（`-var-file=terraform.tfvars` を prod 用に切り替え）。

最終ゲート: 手動オペレータが Chrome / Safari / Mobile Safari の 3 ブラウザで `docs/11-testing-strategy.md §12.2` の必須シナリオを実行し、UX 確認。

## 3. 日常運用

### 3.1 デプロイ

```bash
# CI 経由
git push origin main
# → GitHub Actions が test + build + push + terraform plan dev を流し、
#   prod は手動承認後に apply
```

`apply` は **必ず人間がレビュー後に承認**。`terraform apply -auto-approve` は使わない。

### 3.2 バッチ実行確認

```bash
aws scheduler list-schedules --group-name sync-files-go-prod
# 24 時間以内に Invocation が 0 のままなら CloudWatch アラート (batch-missed-24h) で通知
```

### 3.3 シークレットローテーション

- DB password: `aws secretsmanager rotate-secret --secret-id sync-files-go/prod/db/password` (アプリ側はリスタートで反映)
- AES master key: ローテーションは `cmd/sync-files-admin rotate-aes-key` を ECS RunTask で実行（旧鍵は最低 30 日保持）

### 3.4 災害復旧

詳細は `docs/10-operations.md` §5 を参照。要点:

- RDS PITR で 30 日以内なら任意の時点に戻せる（`terraform import` で state 反映）
- S3 バックエンドバケットは versioning 有効、誤削除は `aws s3api list-object-versions` で復旧
- Cloudflare 障害時は `docs/10-operations.md` §5.E の Runbook で ALB に一時切替

## 4. Phase 6 受け入れ基準チェックリスト

- [x] `make tf-validate` が dev / prod 両方で 0 issue
- [ ] `terraform plan` が staging アカウントで CRITICAL なリソース欠落なし
- [ ] EventBridge Schedule で gc / prune-old-versions / reconcile の 3 ジョブが ENABLED
- [ ] S3 Files (or EFS) 作成スクリプトが README に明記済み（`deploy/scripts/setup-s3files.sh`）
- [ ] Cloudflare Tunnel トークンが Secrets Manager に保管済み
- [ ] staging で `make release-smoke` と `make release-critical-path` が green
- [ ] 手動 3 ブラウザ確認（Chrome / Safari / Mobile Safari）が完了

## 5. 既知の制約

- **S3 Files**: AWS Provider 未対応のため EFS で代替。S3 Files が一般提供されたら `deploy/terraform/modules/ecs/main.tf` の `dynamic "volume"` を `s3files_volume_configuration` に切替
- **autoscale max=1**: S3 Files / EFS の同時 mount 動作が検証されるまで単一タスクに固定（`docs/02-architecture.md` の方針）
- **TOTP**: アプリ側 `unwrapTOTPSecret` がプレースホルダ（Phase 4 で AES-GCM 復号に置換予定）
- **ALB なし**: Cloudflare Tunnel 障害時のフォールバックは Runbook 経由で手動切替
