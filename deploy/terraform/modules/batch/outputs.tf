output "schedule_group_name" {
  value = aws_scheduler_schedule_group.main.name
}

output "schedule_arns" {
  description = "EventBridge Scheduler の各 schedule ARN（gc / prune-old-versions / reconcile）"
  value       = { for k, s in aws_scheduler_schedule.batch : k => s.arn }
}

output "task_definition_arns" {
  value = { for k, td in aws_ecs_task_definition.batch : k => td.arn }
}
