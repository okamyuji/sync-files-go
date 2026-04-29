output "vpc_id" {
  value = module.network.vpc_id
}

output "elastic_ip" {
  description = "EC2 の固定 IP。ユーザの DNS プロバイダで A レコードを向ける"
  value       = module.ec2.elastic_ip
}

output "ec2_instance_id" {
  description = "SSM Session Manager で接続するときの InstanceId"
  value       = module.ec2.instance_id
}

output "rds_primary_endpoint" {
  value = module.rds.primary_endpoint
}

output "rds_replica_endpoint" {
  value = module.rds.replica_endpoint
}

output "rds_master_user_secret_arn" {
  value = module.rds.master_user_secret_arn
}

output "s3_backup_bucket" {
  value = module.s3files.bucket_id
}

output "alerts_topic_arn" {
  value = module.observability.alerts_topic_arn
}
