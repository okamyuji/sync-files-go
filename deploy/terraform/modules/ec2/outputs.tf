output "instance_id" {
  description = "EC2 インスタンス ID（SSM Session Manager で接続するときに使う）"
  value       = aws_instance.main.id
}

output "elastic_ip" {
  description = "EIP。ユーザはこの IP を DNS A レコードに登録する"
  value       = aws_eip.main.public_ip
}

output "instance_role_arn" {
  value = aws_iam_role.instance.arn
}

output "instance_profile_name" {
  value = aws_iam_instance_profile.instance.name
}

output "public_dns" {
  description = "EC2 の AWS 提供 DNS（接続テスト用、本番ドメインは別途）"
  value       = aws_instance.main.public_dns
}
