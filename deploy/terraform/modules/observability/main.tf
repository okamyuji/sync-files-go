// Package observability CloudWatch ロググループ + アラート + SNS 通知トピック。
//
// EC2 単一構成 (ADR-007 改訂後) 用。CloudWatch Agent が EC2 から system / nginx / app ログを送る。
// EventBridge / ECS は使わないので、それ関連のアラームは持たない。

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
    Module  = "observability"
  })
  log_streams = ["system", "nginx", "app"]
}

// === Log Groups ===

resource "aws_cloudwatch_log_group" "this" {
  for_each          = toset(local.log_streams)
  name              = "/sync-files-go/${var.env}/${each.key}"
  retention_in_days = var.env == "prod" ? 90 : 14
  tags              = merge(local.tags, { Name = "sync-files-go-${var.env}-${each.key}" })
}

// === SNS Alert Topic ===

resource "aws_sns_topic" "alerts" {
  name         = "sync-files-go-${var.env}-alerts"
  display_name = "sync-files-go ${var.env} alerts"
  tags         = local.tags
}

resource "aws_sns_topic_subscription" "email" {
  for_each  = toset(var.alert_emails)
  topic_arn = aws_sns_topic.alerts.arn
  protocol  = "email"
  endpoint  = each.key
}

// === Alarms ===

// EC2 ステータスチェック失敗
resource "aws_cloudwatch_metric_alarm" "ec2_status_check_failed" {
  alarm_name          = "sync-files-go-${var.env}-ec2-status-check-failed"
  alarm_description   = "EC2 status check failed (system or instance) for 2 minutes"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 2
  metric_name         = "StatusCheckFailed"
  namespace           = "AWS/EC2"
  period              = 60
  statistic           = "Maximum"
  threshold           = 0
  treat_missing_data  = "breaching"
  alarm_actions       = [aws_sns_topic.alerts.arn]
  ok_actions          = [aws_sns_topic.alerts.arn]
  dimensions = {
    InstanceId = var.ec2_instance_id
  }
  tags = local.tags
}

// EC2 CPU 過負荷（80%+ が 5 分続く）
resource "aws_cloudwatch_metric_alarm" "ec2_cpu_high" {
  alarm_name          = "sync-files-go-${var.env}-ec2-cpu-high"
  alarm_description   = "EC2 CPU above 80% for 5 minutes"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 5
  metric_name         = "CPUUtilization"
  namespace           = "AWS/EC2"
  period              = 60
  statistic           = "Average"
  threshold           = 80
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.alerts.arn]
  dimensions = {
    InstanceId = var.ec2_instance_id
  }
  tags = local.tags
}

// /var/data ディスク使用率
resource "aws_cloudwatch_metric_alarm" "ec2_disk_high" {
  alarm_name          = "sync-files-go-${var.env}-ec2-disk-high"
  alarm_description   = "/var/data disk usage above 85%"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 3
  metric_name         = "disk_used_percent"
  namespace           = "sync-files-go"
  period              = 300
  statistic           = "Average"
  threshold           = 85
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.alerts.arn]
  dimensions = {
    InstanceId = var.ec2_instance_id
    path       = "/var/data"
    fstype     = "ext4"
  }
  tags = local.tags
}

// RDS Read Replica lag
resource "aws_cloudwatch_metric_alarm" "replica_lag_high" {
  alarm_name          = "sync-files-go-${var.env}-replica-lag-high"
  alarm_description   = "RDS Read Replica lag above 10s for 2 minutes"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 2
  metric_name         = "ReplicaLag"
  namespace           = "AWS/RDS"
  period              = 60
  statistic           = "Maximum"
  threshold           = 10
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.alerts.arn]
  ok_actions          = [aws_sns_topic.alerts.arn]
  dimensions = {
    DBInstanceIdentifier = var.rds_replica_identifier
  }
  tags = local.tags
}

// RDS CPU 過負荷
resource "aws_cloudwatch_metric_alarm" "rds_cpu_high" {
  alarm_name          = "sync-files-go-${var.env}-rds-cpu-high"
  alarm_description   = "RDS Primary CPU above 80% for 5 minutes"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 5
  metric_name         = "CPUUtilization"
  namespace           = "AWS/RDS"
  period              = 60
  statistic           = "Average"
  threshold           = 80
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.alerts.arn]
  dimensions = {
    DBInstanceIdentifier = var.rds_primary_identifier
  }
  tags = local.tags
}
