variable "aws_region" {
  description = "AWS リージョン"
  type        = string
  default     = "ap-northeast-1"
}

variable "db_instance_class" {
  description = "RDS インスタンスクラス (dev は最小)"
  type        = string
  default     = "db.t4g.micro"
}

variable "image_tag" {
  description = "ECR にプッシュ済みのイメージタグ。CI から `<git-sha>-<ts>` で渡す"
  type        = string
}

variable "base_url" {
  description = "外部公開ベース URL"
  type        = string
  default     = "https://sync-dev.example.com"
}

variable "alert_emails" {
  description = "SNS alerts subscribers (subscription confirmation 必須)"
  type        = list(string)
  default     = []
}

variable "efs_file_system_id" {
  description = "EFS ファイルシステム ID。setup-s3files.sh で作成して値を渡す"
  type        = string
  default     = ""
}

variable "efs_access_point_id" {
  description = "EFS アクセスポイント ID"
  type        = string
  default     = ""
}
