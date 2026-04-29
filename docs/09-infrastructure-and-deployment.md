# 09. インフラ・デプロイ

> Docker + Terraform + ECR + ECS Fargate + RDS for MySQL（Primary + Read Replica） + S3 Files + Cloudflare Tunnel。
> ALB を使わず、cloudflared / nginx をサイドカーとして同一タスクに同居させる。

## 1. 全体構成（再掲・拡張）

```
                       [Cloudflare Edge]
                              │ DNS / TLS / DDoS
                              │
                         Cloudflare Tunnel
                              │ outbound のみ
                              ▼
            ┌──────────── VPC: 10.0.0.0/16 ──────────────────┐
            │                                                 │
            │  ┌───────────────────────────────────────────┐  │
            │  │  ECS Fargate Task (app-subnet, AZ-a)      │  │
            │  │                                            │  │
            │  │  cloudflared ─→ nginx ─→ app              │  │
            │  │                                            │  │
            │  │  Mount: /var/data → S3 Files               │  │
            │  └───────────────────────────────────────────┘  │
            │             │                  │                 │
            │             ▼                  ▼                 │
            │   ┌─────────────────┐   ┌─────────────────────┐  │
            │   │ RDS MySQL       │   │ S3 Files            │  │
            │   │ Primary  AZ-a   │   │ (NFS, EFS backed)   │  │
            │   │ Standby  AZ-c   │   │ + S3 backend bucket │  │
            │   │ Read Replica AZ-c│   │ (versioning, SSE)   │  │
            │   └─────────────────┘   └─────────────────────┘  │
            │                                                 │
            │  VPC Endpoints:  ECR / Secrets Manager /        │
            │                  S3 / S3 Files / CloudWatch     │
            │                                                 │
            └─────────────────────────────────────────────────┘
```

NAT Gateway は採用しない。Cloudflare Tunnel への outbound 接続は IGW + ENI 経由（コスト最小）。Inbound はセキュリティグループで全 deny。

## 2. リポジトリ配置

```
sync-files-go/
├── deploy/
│   ├── docker/
│   │   ├── app.Dockerfile
│   │   ├── nginx.Dockerfile
│   │   ├── nginx.conf
│   │   ├── cloudflared.config.yml.tmpl
│   │   └── docker-compose.yml          # ローカル開発用
│   └── terraform/
│       ├── modules/
│       │   ├── network/                # VPC / サブネット / SG
│       │   ├── ecs/                    # クラスタ / サービス / タスク定義（3 コンテナ）
│       │   ├── rds/                    # MySQL Primary + Read Replica
│       │   ├── s3files/                # S3 バケット + ライフサイクル（S3 Files 自体は CLI）
│       │   ├── ecr/                    # 3 リポジトリ（app, nginx, cloudflared）
│       │   ├── secrets/                # Secrets Manager
│       │   └── observability/          # CloudWatch
│       └── envs/
│           ├── dev/
│           └── prod/
└── ...
```

## 3. Dockerfiles（3 コンテナ）

### 3.1 app（Go 本体）

```dockerfile
# syntax=docker/dockerfile:1.7
FROM golang:1.23-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ENV CGO_ENABLED=0 GOOS=linux GOARCH=arm64
RUN go build -tags=netgo,osusergo \
    -ldflags="-s -w -buildid=" \
    -trimpath \
    -o /out/sync-files-go \
    ./cmd/server
RUN cp -r internal/ui/templates /out/templates && \
    cp -r internal/ui/static    /out/static

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/sync-files-go /sync-files-go
COPY --from=build /out/templates    /templates
COPY --from=build /out/static       /static
ENV PORT=8080
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/sync-files-go"]
```

### 3.2 nginx

```dockerfile
FROM nginx:1.27-alpine

# 自己署名証明書（cloudflared ↔ nginx 内部用）
COPY internal/cert.pem /etc/nginx/cert.pem
COPY internal/key.pem  /etc/nginx/key.pem

# nginx.conf を deploy/docker/nginx.conf からコピー
COPY deploy/docker/nginx.conf /etc/nginx/nginx.conf

# nonroot
RUN adduser -D -H -u 65532 nginxnonroot
USER 65532:65532

EXPOSE 8443
CMD ["nginx", "-g", "daemon off;"]
```

### 3.3 cloudflared

公式イメージをそのまま使う：

```
image: cloudflare/cloudflared:latest
command: tunnel --no-autoupdate run --token $TUNNEL_TOKEN
```

`TUNNEL_TOKEN` は ECS の secret 経由で Secrets Manager から注入。

## 4. ECS タスク定義（Terraform 抜粋）

```hcl
resource "aws_ecs_task_definition" "app" {
  family                   = "sync-files-go"
  network_mode             = "awsvpc"
  requires_compatibilities = ["FARGATE"]
  cpu                      = "1024"     # 1 vCPU 合計（3 コンテナ）
  memory                   = "2048"     # 2 GiB
  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = "ARM64"
  }
  execution_role_arn = aws_iam_role.ecs_execution.arn
  task_role_arn      = aws_iam_role.ecs_task.arn

  container_definitions = jsonencode([
    {
      name              = "app"
      image             = "${aws_ecr_repository.app.repository_url}:${var.image_tag}"
      essential         = true
      readonlyRootFilesystem = true
      user              = "65532:65532"
      linuxParameters   = { capabilities = { drop = ["ALL"] } }
      portMappings      = []  # nginx だけが対外。app は internal のみ
      environment = [
        { name = "APP_ENV",      value = var.env },
        { name = "DATA_DIR",     value = "/var/data" },
        { name = "DB_PRIMARY_HOST", value = aws_db_instance.primary.address },
        { name = "DB_REPLICA_HOST", value = aws_db_instance.replica.address },
        { name = "DB_PORT",      value = "3306" },
        { name = "DB_NAME",      value = "sync" },
        { name = "DB_USER",      value = "sync_app" },
      ]
      secrets = [
        { name = "DB_PASSWORD",         valueFrom = aws_secretsmanager_secret.db_password.arn },
        { name = "AES_MASTER_KEY",      valueFrom = aws_secretsmanager_secret.aes_master.arn },
        { name = "TOTP_HMAC_KEY",       valueFrom = aws_secretsmanager_secret.totp_hmac.arn },
        { name = "CSRF_KEY",            valueFrom = aws_secretsmanager_secret.csrf.arn },
        { name = "SESSION_KEY",         valueFrom = aws_secretsmanager_secret.session.arn },
      ]
      mountPoints = [
        { sourceVolume = "data", containerPath = "/var/data", readOnly = false }
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
    },
    {
      name      = "nginx"
      image     = "${aws_ecr_repository.nginx.repository_url}:${var.image_tag}"
      essential = true
      readonlyRootFilesystem = true
      user      = "65532:65532"
      linuxParameters = { capabilities = { drop = ["ALL"], add = ["NET_BIND_SERVICE"] } }
      dependsOn = [{ containerName = "app", condition = "HEALTHY" }]
      portMappings = []
      logConfiguration = {
        logDriver = "awslogs"
        options = {
          "awslogs-group"         = aws_cloudwatch_log_group.nginx.name
          "awslogs-region"        = var.region
          "awslogs-stream-prefix" = "nginx"
        }
      }
    },
    {
      name      = "cloudflared"
      image     = "cloudflare/cloudflared:latest"
      essential = true
      command   = ["tunnel", "--no-autoupdate", "run"]
      secrets = [
        { name = "TUNNEL_TOKEN", valueFrom = aws_secretsmanager_secret.cloudflared_token.arn }
      ]
      dependsOn = [{ containerName = "nginx", condition = "START" }]
      logConfiguration = {
        logDriver = "awslogs"
        options = {
          "awslogs-group"         = aws_cloudwatch_log_group.cloudflared.name
          "awslogs-region"        = var.region
          "awslogs-stream-prefix" = "cloudflared"
        }
      }
    }
  ])

  volume {
    name = "data"
    s3files_volume_configuration {
      file_system_arn  = var.s3files_file_system_arn
      access_point_arn = var.s3files_access_point_arn
      port             = 2049
    }
  }
}
```

注: `aws_s3files_file_system` リソースは Terraform AWS provider で 2026年4月時点では PR pending。CLI で作成し ARN を変数注入する手順を [`10-operations.md`](./10-operations.md) §2.3 に記載。

## 5. ECS サービス

```hcl
resource "aws_ecs_service" "app" {
  name             = "sync-files-go"
  cluster          = aws_ecs_cluster.main.id
  task_definition  = aws_ecs_task_definition.app.arn
  desired_count    = 1
  launch_type      = "FARGATE"
  platform_version = "LATEST"

  enable_execute_command = false

  network_configuration {
    subnets          = var.private_subnet_ids
    security_groups  = [aws_security_group.ecs.id]
    assign_public_ip = true   # cloudflared が outbound 接続するため、IGW へ向ける ENI に Public IP
  }

  deployment_circuit_breaker {
    enable   = true
    rollback = true
  }
  deployment_minimum_healthy_percent = 100
  deployment_maximum_percent         = 200
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

ALB を不採用のため `load_balancer` ブロックは無し。

## 6. RDS for MySQL（Primary + Read Replica）

```hcl
resource "aws_db_instance" "primary" {
  identifier              = "sync-files-go-${var.env}-primary"
  engine                  = "mysql"
  engine_version          = "8.0.39"
  instance_class          = var.db_instance_class       # default db.t4g.micro
  allocated_storage       = 20
  max_allocated_storage   = 100
  storage_type            = "gp3"
  storage_encrypted       = true
  kms_key_id              = aws_kms_key.rds.arn

  db_name                 = "sync"
  username                = "rdsadmin"
  manage_master_user_password = true

  multi_az                = true
  publicly_accessible     = false
  vpc_security_group_ids  = [aws_security_group.rds.id]
  db_subnet_group_name    = aws_db_subnet_group.app.name

  backup_retention_period = 30
  backup_window           = "17:00-18:00"
  maintenance_window      = "Sun:18:00-Sun:20:00"

  deletion_protection     = true
  copy_tags_to_snapshot   = true
  performance_insights_enabled = true

  parameter_group_name = aws_db_parameter_group.app.name
}

resource "aws_db_instance" "replica" {
  identifier              = "sync-files-go-${var.env}-replica-1"
  replicate_source_db     = aws_db_instance.primary.identifier
  instance_class          = var.db_instance_class
  publicly_accessible     = false
  vpc_security_group_ids  = [aws_security_group.rds.id]
  performance_insights_enabled = true
  # Read Replica は自動バックアップ無効が既定（Primary のバックアップで足りる）
}

resource "aws_db_parameter_group" "app" {
  family = "mysql8.0"
  name   = "sync-files-go-${var.env}"

  # ngram FULLTEXT のため
  parameter {
    name  = "ngram_token_size"
    value = "2"
  }
  parameter {
    name  = "max_connections"
    value = "100"
  }
  parameter {
    name  = "innodb_buffer_pool_size"
    value = "{DBInstanceClassMemory*3/4}"
  }
}
```

ユーザ作成は手動（`migrations/000_create_users.sql` をマイグレーション CLI から実行）：

```sql
CREATE USER 'sync_app'@'%' IDENTIFIED BY '<from secrets manager>' REQUIRE SSL;
GRANT SELECT, INSERT, UPDATE, DELETE ON sync.* TO 'sync_app'@'%';
REVOKE UPDATE, DELETE ON sync.audit_logs FROM 'sync_app'@'%';

CREATE USER 'sync_migrate'@'%' IDENTIFIED BY '<from secrets manager>' REQUIRE SSL;
GRANT ALL PRIVILEGES ON sync.* TO 'sync_migrate'@'%';
```

Read Replica にもレプリケーション経由で同じユーザが伝播する。

## 7. S3 Files バケットとライフサイクル

```hcl
resource "aws_s3_bucket" "data" {
  bucket = "sync-files-go-${var.env}-data-${data.aws_caller_identity.current.account_id}"
}

resource "aws_s3_bucket_versioning" "data" {
  bucket = aws_s3_bucket.data.id
  versioning_configuration { status = "Enabled" }
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

S3 Files ファイルシステムと Access Point は CLI 手動作成（[`10-operations.md`](./10-operations.md) §2.3）。

## 8. Cloudflare Tunnel のセットアップ

[`10-operations.md`](./10-operations.md) §2.4 に手順を記載。要点：

1. Cloudflare Zero Trust ダッシュボードで Tunnel 作成
2. 認証トークンを Secrets Manager の `sync-files-go/cloudflared/token` に保存
3. Hostname `sync.example.com` を `https://localhost:8443`（自己署名証明書スキップ設定）に紐づけ
4. Cloudflare Access ポリシー（v2 候補）：オフィス IP のみ許可など

## 9. Secrets Manager

```hcl
resource "aws_secretsmanager_secret" "db_password"        { name = "sync-files-go/${var.env}/db/password" ... }
resource "aws_secretsmanager_secret" "aes_master"          { name = "sync-files-go/${var.env}/aes/master-key" ... }
resource "aws_secretsmanager_secret" "totp_hmac"           { name = "sync-files-go/${var.env}/totp/hmac-key" ... }
resource "aws_secretsmanager_secret" "csrf"                { name = "sync-files-go/${var.env}/csrf/key" ... }
resource "aws_secretsmanager_secret" "session"             { name = "sync-files-go/${var.env}/session/key" ... }
resource "aws_secretsmanager_secret" "cloudflared_token"   { name = "sync-files-go/${var.env}/cloudflared/token" ... }
```

## 10. CloudWatch + アラート

```hcl
resource "aws_cloudwatch_log_group" "app"         { name = "/ecs/sync-files-go/${var.env}/app"         retention_in_days = 90 }
resource "aws_cloudwatch_log_group" "nginx"       { name = "/ecs/sync-files-go/${var.env}/nginx"       retention_in_days = 90 }
resource "aws_cloudwatch_log_group" "cloudflared" { name = "/ecs/sync-files-go/${var.env}/cloudflared" retention_in_days = 90 }

resource "aws_cloudwatch_metric_alarm" "task_unhealthy" {
  alarm_name          = "sync-files-go-${var.env}-task-unhealthy"
  comparison_operator = "LessThanThreshold"
  evaluation_periods  = 2
  metric_name         = "RunningTaskCount"
  namespace           = "ECS/ContainerInsights"
  period              = 60
  statistic           = "Average"
  threshold           = 1
  alarm_actions       = [aws_sns_topic.alerts.arn]
  dimensions = {
    ClusterName = aws_ecs_cluster.main.name
    ServiceName = aws_ecs_service.app.name
  }
}

resource "aws_cloudwatch_metric_alarm" "replica_lag_high" {
  alarm_name          = "sync-files-go-${var.env}-replica-lag-high"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 2
  metric_name         = "ReplicaLag"
  namespace           = "AWS/RDS"
  period              = 60
  statistic           = "Maximum"
  threshold           = 10
  alarm_actions       = [aws_sns_topic.alerts.arn]
  dimensions = {
    DBInstanceIdentifier = aws_db_instance.replica.identifier
  }
}
```

## 11. ECR とイメージ管理

```hcl
resource "aws_ecr_repository" "app"   { name = "sync-files-go/app"   image_tag_mutability = "IMMUTABLE" image_scanning_configuration { scan_on_push = true } }
resource "aws_ecr_repository" "nginx" { name = "sync-files-go/nginx" image_tag_mutability = "IMMUTABLE" image_scanning_configuration { scan_on_push = true } }
# cloudflared は Cloudflare 公式イメージを直接使用
```

イメージタグは `<git-sha>-<timestamp>` 形式（`latest` は不採用）。

## 12. デプロイフロー

```
[ローカル開発]
   make build       → Go ビルド
   make test        → 単体・統合テスト
   make docker-build → app + nginx の 2 イメージ
   make docker-test  → 3 コンテナ起動テスト

[CI]
   1. test           → go test ./...
   2. lint           → staticcheck + gosec + govulncheck
   3. build app      → docker build for arm64 (app)
   4. build nginx    → docker build for arm64 (nginx)
   5. scan           → trivy image scan
   6. push           → aws ecr login + push (app, nginx)
   7. plan           → terraform plan -var-file=prod
   8. apply (manual approval)

[Terraform Apply]
   1. ECS が新タスクを 100% 並行で起動
   2. cloudflared が Cloudflare に再接続
   3. nginx + app が健全になったら旧タスクを drain
   4. 失敗時は deployment_circuit_breaker で自動ロールバック
```

ALB が無いため、ALB の "TG healthy" 判定は無く、ECS タスクヘルスチェックのみで判定する。Cloudflare 側はタスク再起動時に短い接続途切れがあり得る（cloudflared の再接続所要時間）。

## 13. ローカル開発環境

`deploy/docker/docker-compose.yml`:

```yaml
services:
  mysql:
    image: mysql:8.0
    environment:
      MYSQL_DATABASE: sync
      MYSQL_USER: sync_app
      MYSQL_PASSWORD: ${SYNC_FILES_MYSQL_PASSWORD}
      MYSQL_ROOT_PASSWORD: ${SYNC_FILES_MYSQL_ROOT_PASSWORD}
    command: --default-authentication-plugin=mysql_native_password --binlog-format=ROW
    ports: [ "3306:3306" ]
    volumes:
      - mysqldata:/var/lib/mysql

  # ローカルでは Read Replica は省略（必要なら mysql:replica で別コンテナ可）

  app:
    build:
      context: ../..
      dockerfile: deploy/docker/app.Dockerfile
    environment:
      APP_ENV:    local
      DATA_DIR:   /var/data
      DB_PRIMARY_HOST: mysql
      DB_REPLICA_HOST: mysql
      DB_PORT:    "3306"
      DB_NAME:    sync
      DB_USER:    sync_app
      DB_PASSWORD: ${SYNC_FILES_MYSQL_PASSWORD}
      USE_LOCAL_FS: "1"
    ports: [ "8080:8080" ]
    volumes:
      - localdata:/var/data
    depends_on: [ mysql ]

  nginx:
    build:
      context: ../..
      dockerfile: deploy/docker/nginx.Dockerfile
    ports: [ "8443:8443" ]
    depends_on: [ app ]

volumes:
  mysqldata:
  localdata:
```

ローカルでは S3 Files の代わりに `hostPath` ボリュームで FS を模擬。Cloudflare Tunnel もローカル開発では起動せず、`https://localhost:8443` で直接アクセス（自己署名のため警告は出る）。

## 14. ネットワークセキュリティグループ

| SG | Inbound | Outbound |
|---|---|---|
| `ecs` | **すべて拒否** | 3306 → `rds`、443 → VPCエンドポイント、443 → 0.0.0.0/0（Cloudflare 外向き） |
| `rds` | 3306 from `ecs` | none |
| `vpce-*` | 443 from `ecs` | none |

最小権限の原則。

## 15. IAM ロール詳細

### 15.1 ecsTaskExecutionRole

- `AmazonECSTaskExecutionRolePolicy` (managed)
- 追加: 当該 Secrets Manager の `secretsmanager:GetSecretValue`（ARN 限定）
- 追加: ECR pull、CloudWatch Logs put

### 15.2 ecsTaskRole（アプリ実行ロール）

- S3 Files Access Point の特定 ARN への `s3files:DataAccess`
- Secrets Manager の特定シークレットの `GetSecretValue`
- 他は付与しない

### 15.3 rdsadmin

`manage_master_user_password = true` で Secrets Manager 自動連携。

## 16. ドメインと TLS

- Cloudflare に `sync.example.com` のホストゾーンを作成
- TLS 証明書は Cloudflare 自動管理（Universal SSL or Advanced）
- ACM / Route 53 は不採用

## 17. コスト見積もり（月次・概算）

| サービス | 単価/容量 | 月額 (USD) |
|---|---|---|
| ECS Fargate (1vCPU/2GiB, arm64, 24/7) | 0.04/h × 24 × 30 ≒ | ~30 |
| **ALB** | 不採用 | **0** |
| **NAT Gateway** | 不採用 | **0** |
| Cloudflare Tunnel (Free) | | 0 |
| RDS db.t4g.micro Multi-AZ Primary | $0.038/h × 2 | ~55 |
| RDS db.t4g.micro Read Replica × 1 | $0.038/h | ~28 |
| RDS storage gp3 20GB × 2 | $2.5/月 × 2 | ~5 |
| S3 Files (Bursting, 100GB active) | | ~30 |
| S3 backend bucket 500GB | | ~12 |
| データ転送（egress 50GB） | | ~5 |
| Secrets / CloudWatch / その他 | | ~10 |
| **合計** | | **~175 USD / 月** |

ALB / NAT 削減により ALB 構成と比べて月 $20-30 程度削減。Read Replica 追加で $25 程度増えるが、ユーザの記事の方針に従う価値がある。

## 18. 環境変数（アプリ）

| 変数 | デフォルト | 説明 |
|---|---|---|
| `PORT` | 8080 | リスンポート |
| `APP_ENV` | local | local / dev / prod |
| `DATA_DIR` | /var/data | S3 Files マウント先 |
| `DB_PRIMARY_HOST` | (必須) | RDS Primary エンドポイント |
| `DB_REPLICA_HOST` | (必須) | RDS Replica エンドポイント |
| `DB_PORT` | 3306 | |
| `DB_NAME` | sync | |
| `DB_USER` | sync_app | |
| `DB_PASSWORD` | (必須・Secrets) | |
| `DB_TLS` | required | mysql ドライバの `tls=true` |
| `AES_MASTER_KEY` | (必須・Secrets) | base64 32 bytes |
| `TOTP_HMAC_KEY` | (必須・Secrets) | |
| `CSRF_KEY` | (必須・Secrets) | |
| `SESSION_KEY` | (必須・Secrets) | |
| `BASE_URL` | https://sync.example.com | |
| `LOG_LEVEL` | info | |
| `MAX_UPLOAD_BYTES` | 2147483648 | 2 GiB |
| `READ_AFTER_WRITE_WINDOW_SECONDS` | 5 | DBRouter の RAW window |
| `REPLICA_LAG_DEGRADE_SECONDS` | 10 | この秒数を超えたら Replica 縮退 |

## 19. 災害復旧（DR）

詳細は [`10-operations.md`](./10-operations.md) §5。要点：

- RDS PITR で 30 日以内なら任意の時点に復旧可
- S3 バケットバージョニングで本体データは復旧可
- Terraform のステート（S3 backend）も別バケットに保存
- Cloudflare 障害時は ALB を一時起動する Runbook（[`10-operations.md`](./10-operations.md) §5.E）

---

次の章: [`10-operations.md`](./10-operations.md)
