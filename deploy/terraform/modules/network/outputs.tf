output "vpc_id" {
  description = "VPC ID"
  value       = aws_vpc.main.id
}

output "public_subnet_ids" {
  description = "Public サブネット ID（ECS Fargate タスクが配置される）"
  value       = [for s in aws_subnet.public : s.id]
}

output "private_subnet_ids" {
  description = "Private サブネット ID（RDS が配置される）"
  value       = [for s in aws_subnet.private : s.id]
}

output "ecs_security_group_id" {
  description = "ECS Fargate タスク SG"
  value       = aws_security_group.ecs.id
}

output "rds_security_group_id" {
  description = "RDS SG"
  value       = aws_security_group.rds.id
}

output "vpce_security_group_id" {
  description = "VPC endpoint SG"
  value       = aws_security_group.vpce.id
}

output "azs" {
  description = "サブネットを配置した AZ 一覧"
  value       = local.azs
}
