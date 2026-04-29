variable "aws_region" {
  type    = string
  default = "ap-northeast-1"
}

variable "aws_profile" {
  description = "AWS CLI プロファイル名"
  type        = string
  default     = "sync-admin"
}

variable "instance_type" {
  type    = string
  default = "t4g.small"
}

variable "db_instance_class" {
  type    = string
  default = "db.t4g.micro"
}

variable "domain_name" {
  description = "公開ドメイン名。EIP に A レコードを向ける"
  type        = string
}

variable "letsencrypt_email" {
  description = "Let's Encrypt 通知メール"
  type        = string
}

variable "base_url" {
  description = "BASE_URL (https://<domain_name>)"
  type        = string
}

variable "alert_emails" {
  type    = list(string)
  default = []
}
