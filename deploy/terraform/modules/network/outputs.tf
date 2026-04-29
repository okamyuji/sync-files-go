output "vpc_id" {
  description = "VPC ID"
  value       = aws_vpc.main.id
}

output "public_subnet_id" {
  description = "EC2 を配置する Public サブネット ID"
  value       = aws_subnet.public.id
}

output "private_subnet_ids" {
  description = "RDS を配置する Private サブネット ID"
  value       = [for s in aws_subnet.private : s.id]
}

output "ec2_security_group_id" {
  description = "EC2 SG"
  value       = aws_security_group.ec2.id
}

output "rds_security_group_id" {
  description = "RDS SG"
  value       = aws_security_group.rds.id
}

output "azs" {
  description = "サブネットを配置した AZ 一覧"
  value       = local.azs
}
