output "app_repository_url" {
  description = "app リポジトリ URL"
  value       = aws_ecr_repository.this["app"].repository_url
}

output "nginx_repository_url" {
  description = "nginx リポジトリ URL"
  value       = aws_ecr_repository.this["nginx"].repository_url
}

output "repository_arns" {
  description = "全リポジトリの ARN（IAM ポリシーから参照）"
  value       = { for k, v in aws_ecr_repository.this : k => v.arn }
}
