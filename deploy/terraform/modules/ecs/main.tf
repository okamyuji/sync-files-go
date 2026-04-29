// Package ecs ECS Fargate cluster + service + 3-container task definition。
//
// 設計書: docs/09-infrastructure-and-deployment.md §4, §5, §15
//
// - cluster: Container Insights 有効、CloudWatch ログドライバ
// - task definition: arm64 Fargate、3 コンテナ (app / nginx / cloudflared)、HEALTHY 依存チェイン
// - service: ALB なし、desired_count=1、autoscale max=1 (S3 Files 単一マウント前提)、circuit breaker
// - IAM: ecsTaskExecutionRole (ECR / Secrets / Logs)、ecsTaskRole (S3 Files / Secrets 読み取り)
//
// 注: S3 Files (file system + access point) は Terraform AWS provider 未対応のため、
// ARN は変数注入。`deploy/scripts/setup-s3files.sh` で AWS CLI から作成する。
// 当面は EFS で代用し、s3files_*_arn が空なら EFS volume を使う。

terraform {
  required_version = ">= 1.7.0"
  required_providers {
    aws = { source = "hashicorp/aws", version = "~> 5.60" }
  }
}

data "aws_region" "current" {}

locals {
  tags = merge(var.tags, {
    Project = "sync-files-go"
    Env     = var.env
    Module  = "ecs"
  })
  cluster_name = "sync-files-go-${var.env}"
  service_name = "sync-files-go"
}

// === Cluster ===

resource "aws_ecs_cluster" "main" {
  name = local.cluster_name

  setting {
    name  = "containerInsights"
    value = "enabled"
  }

  tags = merge(local.tags, { Name = local.cluster_name })
}

resource "aws_ecs_cluster_capacity_providers" "main" {
  cluster_name       = aws_ecs_cluster.main.name
  capacity_providers = ["FARGATE"]

  default_capacity_provider_strategy {
    base              = 1
    weight            = 100
    capacity_provider = "FARGATE"
  }
}

// === IAM: Execution role (ECR pull / Secrets fetch / Logs write) ===

data "aws_iam_policy_document" "ecs_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ecs-tasks.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "execution" {
  name               = "sync-files-go-${var.env}-ecs-execution"
  assume_role_policy = data.aws_iam_policy_document.ecs_assume.json
  tags               = local.tags
}

resource "aws_iam_role_policy_attachment" "execution_managed" {
  role       = aws_iam_role.execution.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}

data "aws_iam_policy_document" "execution_inline" {
  statement {
    actions = [
      "secretsmanager:GetSecretValue",
    ]
    resources = values(var.secret_arns)
  }
  statement {
    actions   = ["kms:Decrypt"]
    resources = ["*"]
    condition {
      test     = "StringEquals"
      variable = "kms:ViaService"
      values   = ["secretsmanager.${data.aws_region.current.name}.amazonaws.com"]
    }
  }
}

resource "aws_iam_role_policy" "execution_inline" {
  name   = "secrets-and-kms"
  role   = aws_iam_role.execution.id
  policy = data.aws_iam_policy_document.execution_inline.json
}

// === IAM: Task role (アプリの実行権限) ===

resource "aws_iam_role" "task" {
  name               = "sync-files-go-${var.env}-ecs-task"
  assume_role_policy = data.aws_iam_policy_document.ecs_assume.json
  tags               = local.tags
}

// アプリは Secrets はすでに環境変数として渡されているので追加権限不要。
// S3 Files (or EFS) のアクセス権はネットワーク + Resource policy 側で制御する。
// データ平面のオブジェクトアクセスはアプリ側の経路（NFS マウント）で完結する。

// === CloudWatch metric / log permissions for app container (PutMetricData EMF) ===

data "aws_iam_policy_document" "task_inline" {
  statement {
    actions   = ["cloudwatch:PutMetricData"]
    resources = ["*"]
  }
  // S3 backend bucket への直アクセス（バックアップ整合性ジョブ等で使うことがある場合に備えて List のみ許可）
  statement {
    actions   = ["s3:ListBucket"]
    resources = [var.s3_backend_bucket_arn]
  }
}

resource "aws_iam_role_policy" "task_inline" {
  name   = "app-runtime"
  role   = aws_iam_role.task.id
  policy = data.aws_iam_policy_document.task_inline.json
}

// === Task definition ===

locals {
  app_secrets = [
    { name = "DB_PASSWORD", valueFrom = var.secret_arns["db_password"] },
    { name = "AES_MASTER_KEY", valueFrom = var.secret_arns["aes_master"] },
    { name = "TOTP_HMAC_KEY", valueFrom = var.secret_arns["totp_hmac"] },
    { name = "CSRF_KEY", valueFrom = var.secret_arns["csrf"] },
    { name = "SESSION_KEY", valueFrom = var.secret_arns["session"] },
  ]

  app_env = [
    { name = "APP_ENV", value = var.env },
    { name = "DATA_DIR", value = "/var/data" },
    { name = "DB_PRIMARY_HOST", value = var.db_primary_host },
    { name = "DB_REPLICA_HOST", value = var.db_replica_host },
    { name = "DB_PORT", value = "3306" },
    { name = "DB_NAME", value = "sync" },
    { name = "DB_USER", value = "sync_app" },
    { name = "DB_TLS", value = "required" },
    { name = "BASE_URL", value = var.base_url },
    { name = "LOG_LEVEL", value = var.log_level },
    { name = "MAX_UPLOAD_BYTES", value = tostring(var.max_upload_bytes) },
    { name = "READ_AFTER_WRITE_WINDOW_SECONDS", value = "5" },
    { name = "REPLICA_LAG_DEGRADE_SECONDS", value = "10" },
  ]

  // EFS / S3 Files: s3files_file_system_arn が空なら EFS にフォールバック。
  use_efs = var.s3files_file_system_arn == ""
}

resource "aws_ecs_task_definition" "app" {
  family                   = "sync-files-go"
  network_mode             = "awsvpc"
  requires_compatibilities = ["FARGATE"]
  cpu                      = "1024"
  memory                   = "2048"
  execution_role_arn       = aws_iam_role.execution.arn
  task_role_arn            = aws_iam_role.task.arn

  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = "ARM64"
  }

  container_definitions = jsonencode([
    {
      name                   = "app"
      image                  = "${var.app_image_url}:${var.image_tag}"
      essential              = true
      readonlyRootFilesystem = true
      user                   = "65532:65532"
      linuxParameters        = { capabilities = { drop = ["ALL"] } }
      environment            = local.app_env
      secrets                = local.app_secrets
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
          "awslogs-group"         = var.log_group_app
          "awslogs-region"        = data.aws_region.current.name
          "awslogs-stream-prefix" = "app"
        }
      }
    },
    {
      name                   = "nginx"
      image                  = "${var.nginx_image_url}:${var.image_tag}"
      essential              = true
      readonlyRootFilesystem = true
      user                   = "65532:65532"
      linuxParameters        = { capabilities = { drop = ["ALL"], add = ["NET_BIND_SERVICE"] } }
      mountPoints = [
        { sourceVolume = "nginx-tmp", containerPath = "/var/cache/nginx", readOnly = false },
        { sourceVolume = "nginx-run", containerPath = "/run", readOnly = false },
      ]
      dependsOn = [{ containerName = "app", condition = "HEALTHY" }]
      healthCheck = {
        command     = ["CMD-SHELL", "wget -qO- --no-check-certificate https://127.0.0.1:8443/healthz || exit 1"]
        interval    = 15
        timeout     = 5
        retries     = 3
        startPeriod = 20
      }
      logConfiguration = {
        logDriver = "awslogs"
        options = {
          "awslogs-group"         = var.log_group_nginx
          "awslogs-region"        = data.aws_region.current.name
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
        { name = "TUNNEL_TOKEN", valueFrom = var.secret_arns["cloudflared_token"] }
      ]
      dependsOn = [{ containerName = "nginx", condition = "HEALTHY" }]
      logConfiguration = {
        logDriver = "awslogs"
        options = {
          "awslogs-group"         = var.log_group_cloudflared
          "awslogs-region"        = data.aws_region.current.name
          "awslogs-stream-prefix" = "cloudflared"
        }
      }
    }
  ])

  // app の永続データボリューム。S3 Files が使えるなら efs_volume_configuration の代替を CLI で wire up、
  // 当面は EFS を使う運用。
  dynamic "volume" {
    for_each = local.use_efs ? [1] : []
    content {
      name = "data"
      efs_volume_configuration {
        file_system_id     = var.efs_file_system_id
        transit_encryption = "ENABLED"
        authorization_config {
          access_point_id = var.efs_access_point_id
          iam             = "ENABLED"
        }
      }
    }
  }

  // S3 Files が将来 Terraform 対応したら、ここに s3files_volume_configuration を生やす。
  // 当面のマニュアル運用は ops ドキュメント参照。

  volume {
    name = "nginx-tmp"
  }
  volume {
    name = "nginx-run"
  }

  tags = merge(local.tags, { Name = "sync-files-go-${var.env}-task" })
}

// === Service ===

resource "aws_ecs_service" "app" {
  name             = local.service_name
  cluster          = aws_ecs_cluster.main.id
  task_definition  = aws_ecs_task_definition.app.arn
  desired_count    = 1
  launch_type      = "FARGATE"
  platform_version = "LATEST"
  propagate_tags   = "SERVICE"

  enable_execute_command = false

  network_configuration {
    subnets          = var.public_subnet_ids
    security_groups  = [var.ecs_security_group_id]
    assign_public_ip = true
  }

  deployment_circuit_breaker {
    enable   = true
    rollback = true
  }
  deployment_minimum_healthy_percent = 100
  deployment_maximum_percent         = 200

  // ALB なし（cloudflared 経由）
  // load_balancer ブロック無し

  tags = merge(local.tags, { Name = "sync-files-go-${var.env}-service" })

  // task_definition は CI/CD で外部更新するため、tf 側では追従しない（drift 抑制）
  lifecycle {
    ignore_changes = [task_definition, desired_count]
  }
}

// === Autoscaling target (max=1 固定) ===

resource "aws_appautoscaling_target" "ecs" {
  max_capacity       = 1
  min_capacity       = 1
  resource_id        = "service/${aws_ecs_cluster.main.name}/${aws_ecs_service.app.name}"
  scalable_dimension = "ecs:service:DesiredCount"
  service_namespace  = "ecs"
}
