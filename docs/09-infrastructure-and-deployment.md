# 09. インフラ・デプロイ

> Docker + Terraform + ECR + ECS Fargate + RDS + S3 Files。標準的な構成を、再現性とセキュリティ重視で組む。

## 1. 全体構成（再掲・拡張）

```
                                 [Route 53 Hosted Zone]
                                          │
                                          ▼
                           example.com  → [ACM 証明書]
                                          │
                                          ▼
                    ┌────────────────────────────────────────┐
                    │          VPC: 10.0.0.0/16              │
                    │                                        │
                    │  ┌──────────────────┐  AZ-a            │
                    │  │ public-subnet-a   │ 10.0.0.0/24      │
                    │  │  └ ALB (1 of 2)   │                 │
                    │  └──────────────────┘                  │
                    │  ┌──────────────────┐  AZ-c            │
                    │  │ public-subnet-c   │ 10.0.1.0/24      │
                    │  │  └ ALB (2 of 2)   │                 │
                    │  └──────────────────┘                  │
                    │                                        │
                    │  ┌──────────────────┐  AZ-a            │
                    │  │ private-subnet-a  │ 10.0.10.0/24     │
                    │  │  ├ ECS Task        │                 │
                    │  │  └ RDS Primary     │                 │
                    │  └──────────────────┘                  │
                    │  ┌──────────────────┐  AZ-c            │
                    │  │ private-subnet-c  │ 10.0.11.0/24     │
                    │  │  ├ ECS Task (任意) │                 │
                    │  │  └ RDS Standby     │                 │
                    │  └──────────────────┘                  │
                    │                                        │
                    │  VPC Endpoints (interface, gateway):    │
                    │   - ECR (api, dkr)                      │
                    │   - CloudWatch Logs                     │
                    │   - Secrets Manager                     │
                    │   - S3 Files                            │
                    │   - S3 (gateway)                        │
                    │                                        │
                    └────────────────────────────────────────┘
```

NAT Gateway は不要（VPC エンドポイント経由で AWS 内に閉じる）。

## 2. リポジトリ配置

```
sync-files-go/
├── deploy/
│   ├── docker/
│   │   ├── Dockerfile
│   │   └── docker-compose.yml          # ローカル開発用
│   └── terraform/
│       ├── modules/
│       │   ├── network/                # VPC / サブネット / NACL
│       │   ├── alb/                    # ALB / TG / リスナー
│       │   ├── ecs/                    # クラスタ / サービス / タスク定義
│       │   ├── rds/                    # RDS インスタンス / PG / SG
│       │   ├── s3files/                # S3 Files / Access Point
│       │   ├── ecr/                    # ECR レポジトリ + ライフサイクル
│       │   ├── secrets/                # Secrets Manager
│       │   ├── route53/                # ホストゾーン / レコード
│       │   └── observability/          # CloudWatch + アラーム
│       └── envs/
│           ├── dev/
│           │   ├── main.tf
│           │   ├── variables.tf
│           │   ├── terraform.tfvars   # .gitignore
│           │   └── backend.tf          # S3 backend
│           └── prod/
│               ├── main.tf
│               ├── variables.tf
│               ├── terraform.tfvars   # .gitignore
│               └── backend.tf
└── ...
```

## 3. Dockerfile

セキュアかつ小さく：

```dockerfile
# syntax=docker/dockerfile:1.7

#############################
# Stage 1: build
#############################
FROM golang:1.23-bookworm AS build

WORKDIR /src

# 依存だけ先にキャッシュ
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# 本番ビルド: stripping, no CGO, static binary
ENV CGO_ENABLED=0 GOOS=linux GOARCH=amd64
RUN go build -tags=netgo,osusergo \
    -ldflags="-s -w -buildid=" \
    -trimpath \
    -o /out/sync-files-go \
    ./cmd/server

# テンプレート・静的アセットをコピー（image 内 Read-only）
RUN mkdir -p /out/ui && cp -r internal/ui/templates /out/ui/templates \
                    && cp -r internal/ui/static    /out/ui/static

#############################
# Stage 2: runtime (Distroless)
#############################
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/sync-files-go /sync-files-go
COPY --from=build /out/ui /ui

# /var/data は ECS の volume mount で attach される

ENV PORT=8080
EXPOSE 8080

USER nonroot:nonroot

ENTRYPOINT ["/sync-files-go"]
```

サイズ目標：圧縮で 30 MB 以下。

### 3.1 マルチアーキテクチャ

ECS Fargate は arm64 を選択（コスト 20% 程度安い）。Dockerfile で `GOARCH=arm64`、ECR には arm64 タグでプッシュ。

## 4. ECS タスク定義（Terraform 抜粋）

```hcl
resource "aws_ecs_task_definition" "app" {
  family                   = "sync-files-go"
  network_mode             = "awsvpc"
  requires_compatibilities = ["FARGATE"]
  cpu                      = "512"          # 0.5 vCPU
  memory                   = "1024"         # 1 GiB
  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = "ARM64"
  }
  execution_role_arn = aws_iam_role.ecs_execution.arn
  task_role_arn      = aws_iam_role.ecs_task.arn

  container_definitions = jsonencode([
    {
      name      = "app"
      image     = "${aws_ecr_repository.app.repository_url}:${var.image_tag}"
      essential = true
      readonlyRootFilesystem = true
      user      = "65532:65532"  # nonroot
      linuxParameters = {
        capabilities = { drop = ["ALL"] }
      }
      portMappings = [{ containerPort = 8080, protocol = "tcp" }]

      environment = [
        { name = "APP_ENV",      value = var.env },
        { name = "DATA_DIR",     value = "/var/data" },
        { name = "DB_HOST",      value = aws_db_instance.app.address },
        { name = "DB_PORT",      value = "5432" },
        { name = "DB_NAME",      value = "sync" },
        { name = "DB_USER",      value = "sync_app" },
      ]

      secrets = [
        { name = "DB_PASSWORD",         valueFrom = "${aws_secretsmanager_secret.db_password.arn}" },
        { name = "AES_MASTER_KEY",      valueFrom = "${aws_secretsmanager_secret.aes_master.arn}" },
        { name = "TOTP_HMAC_KEY",       valueFrom = "${aws_secretsmanager_secret.totp_hmac.arn}" },
        { name = "CSRF_KEY",            valueFrom = "${aws_secretsmanager_secret.csrf.arn}" },
        { name = "SESSION_KEY",         valueFrom = "${aws_secretsmanager_secret.session.arn}" },
      ]

      mountPoints = [
        { sourceVolume = "data", containerPath = "/var/data", readOnly = false },
      ]

      healthCheck = {
        command     = ["CMD", "/sync-files-go", "healthcheck"]
        interval    = 30
        timeout     = 5
        retries     = 3
        startPeriod = 30
      }

      logConfiguration = {
        logDriver = "awslogs"
        options = {
          "awslogs-group"         = aws_cloudwatch_log_group.app.name
          "awslogs-region"        = var.region
          "awslogs-stream-prefix" = "app"
        }
      }
    }
  ])

  volume {
    name = "data"

    s3files_volume_configuration {
      file_system_arn   = var.s3files_file_system_arn
      access_point_arn  = var.s3files_access_point_arn
      port              = 2049
    }
  }
}
```

注: `aws_s3files_file_system` リソースは Terraform AWS provider 側で 2026年4月時点で開発中（PR open）。リリース前なら **AWS CLI / CloudFormation で作成し、ARN を Terraform 変数として注入** する手順を取る（[`10-operations.md`](./10-operations.md) §2）。

## 5. ECS サービス

```hcl
resource "aws_ecs_service" "app" {
  name             = "sync-files-go"
  cluster          = aws_ecs_cluster.main.id
  task_definition  = aws_ecs_task_definition.app.arn
  desired_count    = 1
  launch_type      = "FARGATE"
  platform_version = "LATEST"

  enable_execute_command = false      # 緊急時のみ true に切替

  network_configuration {
    subnets         = var.private_subnet_ids
    security_groups = [aws_security_group.ecs.id]
    assign_public_ip = false
  }

  load_balancer {
    target_group_arn = aws_lb_target_group.app.arn
    container_name   = "app"
    container_port   = 8080
  }

  deployment_circuit_breaker {
    enable   = true
    rollback = true
  }

  deployment_minimum_healthy_percent = 100
  deployment_maximum_percent         = 200

  depends_on = [aws_lb_listener.https]
}

resource "aws_appautoscaling_target" "ecs" {
  max_capacity       = 3
  min_capacity       = 1
  resource_id        = "service/${aws_ecs_cluster.main.name}/${aws_ecs_service.app.name}"
  scalable_dimension = "ecs:service:DesiredCount"
  service_namespace  = "ecs"
}

resource "aws_appautoscaling_policy" "cpu" {
  name               = "cpu-scaling"
  policy_type        = "TargetTrackingScaling"
  resource_id        = aws_appautoscaling_target.ecs.resource_id
  scalable_dimension = aws_appautoscaling_target.ecs.scalable_dimension
  service_namespace  = aws_appautoscaling_target.ecs.service_namespace

  target_tracking_scaling_policy_configuration {
    predefined_metric_specification {
      predefined_metric_type = "ECSServiceAverageCPUUtilization"
    }
    target_value       = 70.0
    scale_in_cooldown  = 300
    scale_out_cooldown = 60
  }
}
```

## 6. RDS for PostgreSQL

```hcl
resource "aws_db_instance" "app" {
  identifier              = "sync-files-go-${var.env}"
  engine                  = "postgres"
  engine_version          = "16.4"
  instance_class          = var.db_instance_class       # default db.t4g.micro
  allocated_storage       = 20
  max_allocated_storage   = 100
  storage_type            = "gp3"
  storage_encrypted       = true
  kms_key_id              = aws_kms_key.rds.arn

  db_name                 = "sync"
  username                = "rdsadmin"                  # 専用、アプリは sync_app
  manage_master_user_password = true                    # Secrets Manager 自動連携

  multi_az                = true
  publicly_accessible     = false
  vpc_security_group_ids  = [aws_security_group.rds.id]
  db_subnet_group_name    = aws_db_subnet_group.app.name

  backup_retention_period = 30
  backup_window           = "17:00-18:00"               # JST 02:00-03:00
  maintenance_window      = "Sun:18:00-Sun:20:00"

  deletion_protection     = true
  copy_tags_to_snapshot   = true
  performance_insights_enabled = true

  apply_immediately       = false
}
```

## 7. S3 Files バケットとライフサイクル

```hcl
resource "aws_s3_bucket" "data" {
  bucket = "sync-files-go-${var.env}-data-${data.aws_caller_identity.current.account_id}"
  force_destroy = false
}

resource "aws_s3_bucket_versioning" "data" {
  bucket = aws_s3_bucket.data.id
  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "data" {
  bucket = aws_s3_bucket.data.id
  rule {
    apply_server_side_encryption_by_default { sse_algorithm = "AES256" }
    bucket_key_enabled = true
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "data" {
  bucket = aws_s3_bucket.data.id

  rule {
    id     = "cleanup-incomplete-multipart"
    status = "Enabled"
    abort_incomplete_multipart_upload { days_after_initiation = 7 }
  }

  rule {
    id     = "expire-old-versions"
    status = "Enabled"
    noncurrent_version_expiration { noncurrent_days = 90 }
  }

  rule {
    id     = "expire-tmp"
    status = "Enabled"
    filter { prefix = "owner-*/tmp/" }
    expiration { days = 7 }
  }
}

resource "aws_s3_bucket_public_access_block" "data" {
  bucket = aws_s3_bucket.data.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}
```

`aws_s3files_file_system` は前述の通り PR pending。代替策として、CLI で作成するスクリプトを `deploy/scripts/setup-s3files.sh` に置く。

## 8. ALB

```hcl
resource "aws_lb" "app" {
  name               = "sync-files-go-${var.env}"
  internal           = false
  load_balancer_type = "application"
  security_groups    = [aws_security_group.alb.id]
  subnets            = var.public_subnet_ids
  drop_invalid_header_fields = true
  enable_deletion_protection = true
  desync_mitigation_mode    = "strictest"
}

resource "aws_lb_listener" "http" {
  load_balancer_arn = aws_lb.app.arn
  port              = 80
  protocol          = "HTTP"
  default_action {
    type = "redirect"
    redirect {
      port        = "443"
      protocol    = "HTTPS"
      status_code = "HTTP_301"
    }
  }
}

resource "aws_lb_listener" "https" {
  load_balancer_arn = aws_lb.app.arn
  port              = 443
  protocol          = "HTTPS"
  ssl_policy        = "ELBSecurityPolicy-TLS13-1-2-2021-06"
  certificate_arn   = aws_acm_certificate.app.arn

  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.app.arn
  }
}

resource "aws_lb_target_group" "app" {
  name        = "sync-files-go-${var.env}"
  port        = 8080
  protocol    = "HTTP"
  vpc_id      = var.vpc_id
  target_type = "ip"

  health_check {
    path                = "/healthz"
    matcher             = "200"
    interval            = 30
    timeout             = 5
    healthy_threshold   = 2
    unhealthy_threshold = 3
  }

  deregistration_delay = 30
}
```

## 9. Secrets Manager

```hcl
resource "aws_secretsmanager_secret" "db_password" {
  name = "sync-files-go/${var.env}/db/password"
  kms_key_id = aws_kms_key.secrets.arn
  recovery_window_in_days = 30
}

# その他の鍵も同様: aes_master, totp_hmac, csrf, session
```

初期投入は AWS CLI で（手動・Terraform に値を含めない）。

## 10. CloudWatch + アラート

```hcl
resource "aws_cloudwatch_log_group" "app" {
  name              = "/ecs/sync-files-go/${var.env}/app"
  retention_in_days = 90
  kms_key_id        = aws_kms_key.logs.arn
}

resource "aws_cloudwatch_metric_alarm" "5xx_high" {
  alarm_name          = "sync-files-go-${var.env}-5xx-high"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 2
  metric_name         = "HTTPCode_Target_5XX_Count"
  namespace           = "AWS/ApplicationELB"
  period              = 60
  statistic           = "Sum"
  threshold           = 10
  alarm_actions       = [aws_sns_topic.alerts.arn]
  dimensions          = { LoadBalancer = aws_lb.app.arn_suffix, TargetGroup = aws_lb_target_group.app.arn_suffix }
}
```

カスタムメトリクスはアプリから `embedded metric format` で stdout に出力 → CloudWatch Logs Insights で集計。

## 11. ECR とイメージ管理

```hcl
resource "aws_ecr_repository" "app" {
  name                 = "sync-files-go"
  image_tag_mutability = "IMMUTABLE"
  encryption_configuration { encryption_type = "AES256" }
  image_scanning_configuration { scan_on_push = true }
}

resource "aws_ecr_lifecycle_policy" "app" {
  repository = aws_ecr_repository.app.name
  policy = jsonencode({
    rules = [{
      rulePriority = 1
      description  = "Keep last 30 images"
      selection    = { tagStatus = "any", countType = "imageCountMoreThan", countNumber = 30 }
      action       = { type = "expire" }
    }]
  })
}
```

イメージタグは `<git-sha>-<timestamp>` 形式（mutable な `latest` は使わない）。

## 12. デプロイフロー

```
[ローカル開発]
   make build       → ローカル Go ビルド
   make test        → 単体・統合テスト
   make docker-build → Docker イメージ
   make docker-test  → コンテナ起動テスト

[CI (GitHub Actions / 任意)]
   1. test           → go test ./...
   2. lint           → staticcheck + golangci-lint + gosec + govulncheck
   3. build          → docker build for arm64
   4. scan           → trivy image scan
   5. push           → aws ecr login + docker push
   6. plan           → terraform plan -var-file=prod
   7. apply          → terraform apply（本番デプロイは手動承認）

[Terraform Apply]
   1. ecs service が新タスク起動 (rolling update, min 100% / max 200%)
   2. ALB target group が新タスクを healthy と判定
   3. 旧タスクが drain → 終了
   4. (失敗時) deployment_circuit_breaker で自動ロールバック
```

## 13. ローカル開発環境

`deploy/docker/docker-compose.yml`:

```yaml
services:
  postgres:
    image: postgres:16-alpine
    environment:
      POSTGRES_DB:       sync
      POSTGRES_USER:     postgres
      POSTGRES_PASSWORD: dev
    ports: [ "5432:5432" ]
    volumes:
      - pgdata:/var/lib/postgresql/data

  minio:
    image: minio/minio
    command: server /data --console-address ":9001"
    environment:
      MINIO_ROOT_USER:     minio
      MINIO_ROOT_PASSWORD: minio12345
    ports: [ "9000:9000", "9001:9001" ]
    volumes:
      - miniodata:/data

  app:
    build:
      context: ../..
      dockerfile: deploy/docker/Dockerfile
    environment:
      APP_ENV:    local
      DATA_DIR:   /var/data
      DB_HOST:    postgres
      DB_PORT:    "5432"
      DB_NAME:    sync
      DB_USER:    postgres
      DB_PASSWORD: ${SYNC_FILES_MYSQL_PASSWORD}
      USE_LOCAL_FS: "1"        # MinIO は POSIX ライクではないので、ローカルでは hostPath を使用
      LOCAL_DATA_PATH: /var/data
    ports: [ "8080:8080" ]
    volumes:
      - localdata:/var/data
    depends_on: [ postgres ]

volumes:
  pgdata:
  miniodata:
  localdata:
```

ローカルでは S3 Files の代わりに `hostPath` ボリュームでファイルシステムを模擬。MinIO はメタデータ寄せの用途でのみ使う（v1 ではローカル開発で MinIO を使わない選択もあり）。

## 14. ネットワークセキュリティグループ

| SG | Inbound | Outbound |
|---|---|---|
| `alb` | 80, 443 from 0.0.0.0/0 | All to vpc |
| `ecs` | 8080 from `alb` | 5432 to `rds`, 443 to VPC endpoints, 2049 (NFS) to S3 Files endpoint |
| `rds` | 5432 from `ecs` | none |
| `vpce-secrets` | 443 from `ecs` | none |
| `vpce-s3files` | 2049 from `ecs` | none |

最小権限の原則。デフォルト deny。

## 15. IAM ロール詳細

### 15.1 ecsTaskExecutionRole

- `AmazonECSTaskExecutionRolePolicy` (managed)
- 追加: 当該 Secrets Manager のシークレットの `secretsmanager:GetSecretValue`（ARN 限定）
- 追加: 当該 ECR レポジトリの pull
- 追加: 当該 CloudWatch Logs グループへの put

### 15.2 ecsTaskRole（アプリ実行ロール）

- S3 Files Access Point の特定 ARN への `s3files:DataAccess`（READ/WRITE）
- Secrets Manager の特定シークレットの `GetSecretValue`
- 他は付与しない（IAM `s3:*` は付けない、S3 Files で十分）

### 15.3 rdsadmin

RDS マスタユーザは Terraform が `manage_master_user_password = true` で Secrets Manager に自動投入。手動操作は ECS Exec か AWS Console のみ。

## 16. ドメインと TLS

- ドメイン: `sync.example.com`（仮）
- ACM 証明書を `us-east-1` ではなく ALB と同じリージョン (`ap-northeast-1`) で発行
- DNS 検証
- ALB の TLS 終端
- HSTS ヘッダはアプリで付与

## 17. コスト見積もり（月次・概算）

| サービス | 単価/容量 | 月額 (USD) |
|---|---|---|
| ECS Fargate (0.5vCPU/1GiB, arm64, 24/7) | $0.04 / hour 相当 | ~30 |
| ALB | $20 / month + LCU | ~22 |
| RDS db.t4g.micro Multi-AZ | $0.038/h × 2 | ~55 |
| RDS storage gp3 20GB | $2.5/month | ~3 |
| S3 Files (内部 EFS スループットモード) | アクティブ容量 100GB 想定 | ~30 |
| S3 (バックエンド) 500GB | $11.5 | ~12 |
| データ転送 | 50GB out | ~5 |
| Route 53 / ACM / Secrets / CloudWatch | 雑費 | ~10 |
| **合計** | | **~167 USD / 月** |

参考値であり、実績で見直す。S3 Files のアクティブデータがほぼゼロならスループットモード Bursting で更に下がる。

## 18. 環境変数（アプリ）

| 変数 | デフォルト | 説明 |
|---|---|---|
| `PORT` | 8080 | リスンポート |
| `APP_ENV` | local | local / dev / prod |
| `DATA_DIR` | /var/data | S3 Files マウント先 |
| `DB_HOST` | (必須) | RDS エンドポイント |
| `DB_PORT` | 5432 | |
| `DB_NAME` | sync | |
| `DB_USER` | sync_app | |
| `DB_PASSWORD` | (必須・Secrets) | |
| `DB_SSLMODE` | require | |
| `AES_MASTER_KEY` | (必須・Secrets) | base64 32 bytes |
| `TOTP_HMAC_KEY` | (必須・Secrets) | base64 32 bytes |
| `CSRF_KEY` | (必須・Secrets) | base64 32 bytes |
| `SESSION_KEY` | (必須・Secrets) | base64 32 bytes |
| `BASE_URL` | https://sync.example.com | 公開リンク URL の組み立てに使用 |
| `LOG_LEVEL` | info | debug / info / warn / error |
| `MAX_UPLOAD_BYTES` | 2147483648 | 2 GiB |

## 19. 災害復旧（DR）

詳細は [`10-operations.md`](./10-operations.md) §5。要点：

- RDS PITR で 30 日以内なら任意の時点に復旧可
- S3 バケットバージョニングと S3 Files の自動同期で本体データは復旧可
- Terraform のステート（S3 backend）も別バケットに保存し、定期スナップショット

---

次の章: [`10-operations.md`](./10-operations.md)
