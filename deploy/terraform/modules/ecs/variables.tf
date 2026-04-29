variable "env" {
  description = "環境名"
  type        = string
}

variable "image_tag" {
  description = "ECR にプッシュ済みのイメージタグ (<sha>-<ts>)"
  type        = string
}

variable "app_image_url" {
  description = "ECR app リポジトリ URL"
  type        = string
}

variable "nginx_image_url" {
  description = "ECR nginx リポジトリ URL"
  type        = string
}

variable "public_subnet_ids" {
  description = "ECS Fargate タスクを配置する Public サブネット ID（IGW 経由 outbound）"
  type        = list(string)
}

variable "ecs_security_group_id" {
  description = "ECS Fargate タスク SG"
  type        = string
}

variable "secret_arns" {
  description = "Secrets Manager ARN マップ (db_password / aes_master / totp_hmac / csrf / session / cloudflared_token)"
  type        = map(string)
}

variable "db_primary_host" {
  description = "RDS Primary writer エンドポイント"
  type        = string
}

variable "db_replica_host" {
  description = "RDS Read Replica エンドポイント"
  type        = string
}

variable "base_url" {
  description = "外部公開ベース URL"
  type        = string
  default     = "https://sync.example.com"
}

variable "log_level" {
  description = "アプリのログレベル"
  type        = string
  default     = "info"
}

variable "max_upload_bytes" {
  description = "アップロード最大バイト数 (default 2 GiB)"
  type        = number
  default     = 2147483648
}

variable "log_group_app" {
  description = "app コンテナのロググループ名"
  type        = string
}

variable "log_group_nginx" {
  description = "nginx コンテナのロググループ名"
  type        = string
}

variable "log_group_cloudflared" {
  description = "cloudflared コンテナのロググループ名"
  type        = string
}

variable "s3_backend_bucket_arn" {
  description = "S3 backend バケット ARN（task role の List 権限用）"
  type        = string
}

variable "s3files_file_system_arn" {
  description = "S3 Files file system ARN（CLI 作成、未設定なら EFS にフォールバック）"
  type        = string
  default     = ""
}

variable "s3files_access_point_arn" {
  description = "S3 Files access point ARN"
  type        = string
  default     = ""
}

variable "efs_file_system_id" {
  description = "EFS file system ID（s3files_file_system_arn 未設定時のフォールバック）"
  type        = string
  default     = ""
}

variable "efs_access_point_id" {
  description = "EFS access point ID"
  type        = string
  default     = ""
}

variable "tags" {
  description = "共通タグ"
  type        = map(string)
  default     = {}
}
