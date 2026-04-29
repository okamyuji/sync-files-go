output "bucket_id" {
  description = "S3 バックエンドバケット ID"
  value       = aws_s3_bucket.data.id
}

output "bucket_arn" {
  description = "S3 バックエンドバケット ARN"
  value       = aws_s3_bucket.data.arn
}
