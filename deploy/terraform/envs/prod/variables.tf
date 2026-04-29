variable "aws_region" {
  description = "AWS リージョン"
  type        = string
  default     = "ap-northeast-1"
}

variable "db_instance_class" {
  description = "RDS インスタンスクラス (prod は db.t4g.small 以上を推奨)"
  type        = string
  default     = "db.t4g.small"
}

variable "image_tag" {
  description = "ECR にプッシュ済みイメージタグ"
  type        = string
}

variable "base_url" {
  description = "外部公開ベース URL"
  type        = string
  default     = "https://sync.example.com"
}

variable "alert_emails" {
  description = "SNS alerts subscribers"
  type        = list(string)
  default     = []
}

variable "efs_file_system_id" {
  type    = string
  default = ""
}

variable "efs_access_point_id" {
  type    = string
  default = ""
}
