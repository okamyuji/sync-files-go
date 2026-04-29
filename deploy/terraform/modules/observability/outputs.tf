output "log_group_names" {
  description = "ログストリーム別ロググループ名 (system / nginx / app)"
  value       = { for k, lg in aws_cloudwatch_log_group.this : k => lg.name }
}

output "alerts_topic_arn" {
  value = aws_sns_topic.alerts.arn
}
