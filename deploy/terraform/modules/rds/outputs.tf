output "primary_endpoint" {
  description = "Primary writer エンドポイント (host:port を含むので host だけは split)"
  value       = aws_db_instance.primary.address
}

// Replica が無効化されている場合は Primary を返す（縮退運転）
output "replica_endpoint" {
  description = "Read Replica エンドポイント。create_replica=false なら Primary と同じ"
  value       = var.create_replica ? aws_db_instance.replica[0].address : aws_db_instance.primary.address
}

output "primary_port" {
  value = aws_db_instance.primary.port
}

output "primary_arn" {
  value = aws_db_instance.primary.arn
}

output "primary_identifier" {
  value = aws_db_instance.primary.identifier
}

output "replica_identifier" {
  description = "Replica DBInstanceIdentifier。create_replica=false なら Primary と同じ"
  value       = var.create_replica ? aws_db_instance.replica[0].identifier : aws_db_instance.primary.identifier
}

output "master_user_secret_arn" {
  description = "manage_master_user_password=true で自動生成された Secrets Manager シークレット ARN"
  value       = aws_db_instance.primary.master_user_secret[0].secret_arn
}

output "kms_key_arn" {
  value = aws_kms_key.rds.arn
}
