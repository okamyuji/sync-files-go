// Package batch EventBridge Scheduler から ECS RunTask で起動するバッチジョブ。
//
// 設計書: docs/12-roadmap.md / docs/09-infrastructure-and-deployment.md / Phase 4
//
// ジョブ:
//   - gc                       state='trashed' AND deleted_at < now()-30d を物理削除（cron 0 18 * * ? *）
//   - prune-old-versions       90 日経過 + 非 current の file_versions を削除（cron 0 19 * * ? *）
//   - reconcile-expired-uploads tmp upload_sessions の期限切れ掃除（cron 0 17 * * ? *）

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
    Module  = "batch"
  })
  jobs = {
    gc = {
      command       = ["gc"]
      schedule_cron = "cron(0 18 * * ? *)"
      description   = "ゴミ箱の物理削除 (state=trashed AND deleted_at < now-30d)"
    }
    prune-old-versions = {
      command       = ["prune-old-versions"]
      schedule_cron = "cron(0 19 * * ? *)"
      description   = "90 日経過 + 非 current の旧版を削除"
    }
    reconcile = {
      command       = ["reconcile"]
      schedule_cron = "cron(0 17 * * ? *)"
      description   = "tmp upload_sessions の期限切れ掃除 + DB/S3 divergence 補正"
    }
  }
}

// === Batch task definition (server image を共用、entryPoint = /sync-files-batch) ===

data "aws_iam_policy_document" "batch_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ecs-tasks.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "batch_execution" {
  name               = "sync-files-go-${var.env}-batch-execution"
  assume_role_policy = data.aws_iam_policy_document.batch_assume.json
  tags               = local.tags
}

resource "aws_iam_role_policy_attachment" "batch_execution_managed" {
  role       = aws_iam_role.batch_execution.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}

data "aws_iam_policy_document" "batch_secrets" {
  statement {
    actions   = ["secretsmanager:GetSecretValue"]
    resources = values(var.secret_arns)
  }
}

resource "aws_iam_role_policy" "batch_secrets" {
  name   = "secrets"
  role   = aws_iam_role.batch_execution.id
  policy = data.aws_iam_policy_document.batch_secrets.json
}

resource "aws_iam_role" "batch_task" {
  name               = "sync-files-go-${var.env}-batch-task"
  assume_role_policy = data.aws_iam_policy_document.batch_assume.json
  tags               = local.tags
}

resource "aws_iam_role_policy" "batch_task" {
  name = "batch-runtime"
  role = aws_iam_role.batch_task.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect   = "Allow"
        Action   = ["cloudwatch:PutMetricData"]
        Resource = "*"
      }
    ]
  })
}

resource "aws_ecs_task_definition" "batch" {
  for_each                 = local.jobs
  family                   = "sync-files-go-batch-${each.key}"
  network_mode             = "awsvpc"
  requires_compatibilities = ["FARGATE"]
  cpu                      = "512"
  memory                   = "1024"
  execution_role_arn       = aws_iam_role.batch_execution.arn
  task_role_arn            = aws_iam_role.batch_task.arn

  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = "ARM64"
  }

  container_definitions = jsonencode([
    {
      name                   = "batch"
      image                  = "${var.app_image_url}:${var.image_tag}"
      essential              = true
      readonlyRootFilesystem = true
      user                   = "65532:65532"
      entryPoint             = ["/sync-files-batch"]
      command                = each.value.command
      environment = [
        { name = "APP_ENV", value = var.env },
        { name = "DB_PRIMARY_HOST", value = var.db_primary_host },
        { name = "DB_REPLICA_HOST", value = var.db_replica_host },
        { name = "DB_PORT", value = "3306" },
        { name = "DB_NAME", value = "sync" },
        { name = "DB_USER", value = "sync_app" },
        { name = "DB_TLS", value = "required" },
        { name = "LOG_LEVEL", value = "info" },
      ]
      secrets = [
        { name = "DB_PASSWORD", valueFrom = var.secret_arns["db_password"] },
        { name = "AES_MASTER_KEY", valueFrom = var.secret_arns["aes_master"] },
      ]
      logConfiguration = {
        logDriver = "awslogs"
        options = {
          "awslogs-group"         = var.log_group_batch
          "awslogs-region"        = data.aws_region.current.name
          "awslogs-stream-prefix" = each.key
        }
      }
    }
  ])

  tags = merge(local.tags, { Name = "sync-files-go-${var.env}-batch-${each.key}", Job = each.key })
}

// === EventBridge Scheduler ===

// Scheduler の実行ロール: ecs:RunTask + iam:PassRole
data "aws_iam_policy_document" "scheduler_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["scheduler.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "scheduler" {
  name               = "sync-files-go-${var.env}-scheduler"
  assume_role_policy = data.aws_iam_policy_document.scheduler_assume.json
  tags               = local.tags
}

resource "aws_iam_role_policy" "scheduler" {
  name = "run-task-and-pass-role"
  role = aws_iam_role.scheduler.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect = "Allow"
        Action = ["ecs:RunTask"]
        // ECS RunTask 対象のタスク定義 ARN（任意リビジョン）
        Resource = [for k, td in aws_ecs_task_definition.batch : "${td.arn_without_revision}:*"]
      },
      {
        Effect   = "Allow"
        Action   = ["iam:PassRole"]
        Resource = [aws_iam_role.batch_execution.arn, aws_iam_role.batch_task.arn]
        Condition = {
          StringEquals = {
            "iam:PassedToService" = "ecs-tasks.amazonaws.com"
          }
        }
      }
    ]
  })
}

resource "aws_scheduler_schedule_group" "main" {
  name = "sync-files-go-${var.env}"
  tags = local.tags
}

resource "aws_scheduler_schedule" "batch" {
  for_each = local.jobs

  name                         = "sync-files-go-${var.env}-${each.key}"
  group_name                   = aws_scheduler_schedule_group.main.name
  description                  = each.value.description
  schedule_expression          = each.value.schedule_cron
  schedule_expression_timezone = "Asia/Tokyo"
  state                        = "ENABLED"

  flexible_time_window {
    mode = "OFF"
  }

  target {
    arn      = var.ecs_cluster_arn
    role_arn = aws_iam_role.scheduler.arn

    ecs_parameters {
      task_definition_arn = aws_ecs_task_definition.batch[each.key].arn
      launch_type         = "FARGATE"
      task_count          = 1
      platform_version    = "LATEST"

      network_configuration {
        subnets          = var.public_subnet_ids
        security_groups  = [var.ecs_security_group_id]
        assign_public_ip = true
      }
    }

    retry_policy {
      maximum_event_age_in_seconds = 3600
      maximum_retry_attempts       = 1
    }
  }
}
