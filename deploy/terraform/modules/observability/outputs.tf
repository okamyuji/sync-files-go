output "log_group_names" {
  description = "コンテナ別ロググループ名"
  value       = { for k, lg in aws_cloudwatch_log_group.container : k => lg.name }
}

output "batch_log_group_name" {
  value = aws_cloudwatch_log_group.batch.name
}

output "alerts_topic_arn" {
  value = aws_sns_topic.alerts.arn
}
