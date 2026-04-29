variable "env" {
  description = "環境名"
  type        = string
}

variable "instance_type" {
  description = "EC2 インスタンスタイプ"
  type        = string
  default     = "t4g.small"
}

variable "root_volume_gb" {
  description = "ルートボリュームサイズ"
  type        = number
  default     = 16
}

variable "data_volume_gb" {
  description = "/var/data 用追加 EBS サイズ"
  type        = number
  default     = 50
}

variable "snapshot_retention_days" {
  description = "DLM スナップショット保持日数"
  type        = number
  default     = 14
}

variable "public_subnet_id" {
  description = "EC2 を配置する Public サブネット ID"
  type        = string
}

variable "ec2_security_group_id" {
  description = "EC2 SG ID"
  type        = string
}

variable "secret_arns" {
  description = "Secrets Manager ARN マップ (db_password / aes_master / totp_hmac / csrf / session)"
  type        = map(string)
}

variable "s3_backup_bucket_id" {
  description = "バックアップ用 S3 バケット ID"
  type        = string
}

variable "s3_backup_bucket_arn" {
  description = "バックアップ用 S3 バケット ARN"
  type        = string
}

variable "db_primary_host" {
  type = string
}

variable "db_replica_host" {
  type = string
}

variable "domain_name" {
  description = "公開ドメイン名（A レコードを EIP に向ける。AWS 外の DNS でユーザが設定）"
  type        = string
}

variable "letsencrypt_email" {
  description = "Let's Encrypt 通知メール"
  type        = string
}

variable "base_url" {
  description = "BASE_URL 環境変数。通常は https://<domain_name>"
  type        = string
}

variable "log_level" {
  description = "アプリのログレベル"
  type        = string
  default     = "info"
}

variable "max_upload_bytes" {
  description = "最大アップロードサイズ"
  type        = number
  default     = 2147483648
}

variable "tags" {
  type    = map(string)
  default = {}
}
