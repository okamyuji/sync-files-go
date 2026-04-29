variable "aws_region" {
  type    = string
  default = "ap-northeast-1"
}

variable "aws_profile" {
  type    = string
  default = "sync-admin"
}

variable "instance_type" {
  type    = string
  default = "t4g.small"
}

variable "data_volume_gb" {
  type    = number
  default = 100
}

variable "db_instance_class" {
  type    = string
  default = "db.t4g.small"
}

variable "domain_name" {
  type = string
}

variable "letsencrypt_email" {
  type = string
}

variable "base_url" {
  type = string
}

variable "alert_emails" {
  type    = list(string)
  default = []
}
