output "vpc_id" {
  value = module.network.vpc_id
}

output "ecr_app_url" {
  description = "docker push の宛先 (app)"
  value       = module.ecr.app_repository_url
}

output "ecr_nginx_url" {
  description = "docker push の宛先 (nginx)"
  value       = module.ecr.nginx_repository_url
}

output "rds_primary_endpoint" {
  value = module.rds.primary_endpoint
}

output "rds_replica_endpoint" {
  value = module.rds.replica_endpoint
}

output "rds_master_user_secret_arn" {
  description = "RDS の自動生成マスターパスワードシークレット (manage_master_user_password=true)"
  value       = module.rds.master_user_secret_arn
}

output "ecs_cluster_name" {
  value = module.ecs.cluster_name
}

output "ecs_service_name" {
  value = module.ecs.service_name
}

output "alerts_topic_arn" {
  value = module.observability.alerts_topic_arn
}

output "batch_schedule_group" {
  value = module.batch.schedule_group_name
}
