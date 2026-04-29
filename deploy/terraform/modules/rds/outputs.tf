output "primary_endpoint" {
  description = "Primary writer エンドポイント (host:port を含むので host だけは split)"
  value       = aws_db_instance.primary.address
}

output "replica_endpoint" {
  description = "Read Replica エンドポイント"
  value       = aws_db_instance.replica.address
}

output "primary_port" {
  value = aws_db_instance.primary.port
}

output "primary_arn" {
  value = aws_db_instance.primary.arn
}

output "master_user_secret_arn" {
  description = "manage_master_user_password=true で自動生成された Secrets Manager シークレット ARN"
  value       = aws_db_instance.primary.master_user_secret[0].secret_arn
}

output "kms_key_arn" {
  value = aws_kms_key.rds.arn
}
