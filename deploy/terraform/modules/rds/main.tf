// Package rds RDS for MySQL Primary (Multi-AZ) + Read Replica × 1。
//
// 設計書: docs/09-infrastructure-and-deployment.md §6
//
// - engine 8.0.39 / parameter group: ngram_token_size=2, max_connections=100, innodb_buffer_pool_size 自動
// - manage_master_user_password=true で master pass を Secrets Manager に自動連携
//   （sync_app / sync_migrate ユーザの作成は migrations/000_create_users.sql + マイグレーション CLI）
// - storage_encrypted=true (KMS)、PI 有効、自動バックアップ 30 日

terraform {
  required_version = ">= 1.7.0"
  required_providers {
    aws = { source = "hashicorp/aws", version = "~> 5.60" }
  }
}

locals {
  tags = merge(var.tags, {
    Project = "sync-files-go"
    Env     = var.env
    Module  = "rds"
  })
}

resource "aws_kms_key" "rds" {
  description             = "KMS key for RDS storage encryption (sync-files-go-${var.env})"
  deletion_window_in_days = var.env == "prod" ? 30 : 7
  enable_key_rotation     = true
  tags                    = merge(local.tags, { Name = "sync-files-go-${var.env}-rds-kms" })
}

resource "aws_kms_alias" "rds" {
  name          = "alias/sync-files-go-${var.env}-rds"
  target_key_id = aws_kms_key.rds.key_id
}

resource "aws_db_subnet_group" "app" {
  name       = "sync-files-go-${var.env}-rds"
  subnet_ids = var.private_subnet_ids
  tags       = merge(local.tags, { Name = "sync-files-go-${var.env}-rds-subnet" })
}

resource "aws_db_parameter_group" "app" {
  family      = "mysql8.0"
  name        = "sync-files-go-${var.env}"
  description = "sync-files-go custom params (ngram_token_size, conn limits)"

  // static パラメタは apply_method=pending-reboot 必須
  parameter {
    name         = "ngram_token_size"
    value        = "2"
    apply_method = "pending-reboot"
  }
  parameter {
    name         = "binlog_format"
    value        = "ROW"
    apply_method = "pending-reboot"
  }
  parameter {
    name  = "max_connections"
    value = "100"
  }
  parameter {
    name  = "require_secure_transport"
    value = "ON"
  }

  tags = merge(local.tags, { Name = "sync-files-go-${var.env}-pg" })
}

// === Primary ===

resource "aws_db_instance" "primary" {
  identifier     = "sync-files-go-${var.env}-primary"
  engine         = "mysql"
  engine_version = "8.0.45"
  instance_class = var.db_instance_class

  allocated_storage     = 20
  max_allocated_storage = 100
  storage_type          = "gp3"
  storage_encrypted     = true
  kms_key_id            = aws_kms_key.rds.arn

  db_name                     = "sync"
  username                    = "syncadmin"
  manage_master_user_password = true

  multi_az               = true
  publicly_accessible    = false
  vpc_security_group_ids = [var.rds_security_group_id]
  db_subnet_group_name   = aws_db_subnet_group.app.name

  backup_retention_period = var.env == "prod" ? 30 : 7
  backup_window           = "17:00-18:00"
  maintenance_window      = "Sun:18:00-Sun:20:00"

  deletion_protection          = var.env == "prod"
  copy_tags_to_snapshot        = true
  performance_insights_enabled = var.enable_performance_insights
  monitoring_interval          = 60
  monitoring_role_arn          = aws_iam_role.rds_monitoring.arn

  parameter_group_name = aws_db_parameter_group.app.name
  apply_immediately    = var.env != "prod"

  // dev / staging はスナップショットなしで destroy 可能、prod は強制スナップショット
  skip_final_snapshot       = var.env != "prod"
  final_snapshot_identifier = var.env == "prod" ? "sync-files-go-${var.env}-final-${formatdate("YYYYMMDDhhmm", timestamp())}" : null

  tags = merge(local.tags, { Name = "sync-files-go-${var.env}-primary" })

  lifecycle {
    ignore_changes = [final_snapshot_identifier] // timestamp() の変動を毎回 plan に出さない
  }
}

// === Read Replica ===

// Read Replica は `manage_master_user_password=true` の MySQL Primary には作れない（AWS 制約）。
// dev/test では create_replica=false で省略。prod で必要なら別途 master password を Secrets で管理して有効化する。
resource "aws_db_instance" "replica" {
  count                  = var.create_replica ? 1 : 0
  identifier             = "sync-files-go-${var.env}-replica-1"
  replicate_source_db    = aws_db_instance.primary.identifier
  instance_class         = var.db_instance_class
  publicly_accessible    = false
  vpc_security_group_ids = [var.rds_security_group_id]

  performance_insights_enabled = var.enable_performance_insights
  monitoring_interval          = 60
  monitoring_role_arn          = aws_iam_role.rds_monitoring.arn

  apply_immediately    = var.env != "prod"
  skip_final_snapshot  = true
  parameter_group_name = aws_db_parameter_group.app.name

  tags = merge(local.tags, { Name = "sync-files-go-${var.env}-replica-1" })
}

// === Enhanced Monitoring IAM ===

data "aws_iam_policy_document" "rds_monitoring_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["monitoring.rds.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "rds_monitoring" {
  name               = "sync-files-go-${var.env}-rds-monitoring"
  assume_role_policy = data.aws_iam_policy_document.rds_monitoring_assume.json
  tags               = local.tags
}

resource "aws_iam_role_policy_attachment" "rds_monitoring" {
  role       = aws_iam_role.rds_monitoring.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonRDSEnhancedMonitoringRole"
}
