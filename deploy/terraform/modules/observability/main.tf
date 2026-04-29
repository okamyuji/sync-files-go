// Package observability CloudWatch ロググループ + アラート + SNS 通知トピック。
//
// 設計書: docs/09-infrastructure-and-deployment.md §10

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
  containers = ["app", "nginx", "cloudflared"]
}

// === Log Groups ===

resource "aws_cloudwatch_log_group" "container" {
  for_each          = toset(local.containers)
  name              = "/ecs/sync-files-go/${var.env}/${each.key}"
  retention_in_days = var.env == "prod" ? 90 : 14
  tags              = merge(local.tags, { Name = "sync-files-go-${var.env}-${each.key}-logs" })
}

resource "aws_cloudwatch_log_group" "batch" {
  name              = "/ecs/sync-files-go/${var.env}/batch"
  retention_in_days = var.env == "prod" ? 90 : 14
  tags              = merge(local.tags, { Name = "sync-files-go-${var.env}-batch-logs" })
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

resource "aws_cloudwatch_metric_alarm" "task_unhealthy" {
  alarm_name          = "sync-files-go-${var.env}-task-unhealthy"
  alarm_description   = "ECS service running task count below desired (1)"
  comparison_operator = "LessThanThreshold"
  evaluation_periods  = 2
  metric_name         = "RunningTaskCount"
  namespace           = "ECS/ContainerInsights"
  period              = 60
  statistic           = "Average"
  threshold           = 1
  treat_missing_data  = "breaching"
  alarm_actions       = [aws_sns_topic.alerts.arn]
  ok_actions          = [aws_sns_topic.alerts.arn]
  dimensions = {
    ClusterName = var.ecs_cluster_name
    ServiceName = var.ecs_service_name
  }
  tags = local.tags
}

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

// 24h バッチ実行 0 アラート
resource "aws_cloudwatch_metric_alarm" "batch_missed" {
  alarm_name          = "sync-files-go-${var.env}-batch-missed-24h"
  alarm_description   = "EventBridge ECS RunTask invocations are 0 for 24h (gc / prune / reconcile any)"
  comparison_operator = "LessThanOrEqualToThreshold"
  evaluation_periods  = 1
  metric_name         = "Invocations"
  namespace           = "AWS/Events"
  period              = 86400
  statistic           = "Sum"
  threshold           = 0
  treat_missing_data  = "breaching"
  alarm_actions       = [aws_sns_topic.alerts.arn]
  tags                = local.tags
}
