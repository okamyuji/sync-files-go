variable "env" {
  description = "環境名"
  type        = string
}

variable "private_subnet_ids" {
  description = "RDS を配置する Private サブネット ID（最低 2 AZ）"
  type        = list(string)
}

variable "rds_security_group_id" {
  description = "RDS に attach する SG ID"
  type        = string
}

variable "db_instance_class" {
  description = "DB インスタンスクラス"
  type        = string
  default     = "db.t4g.micro"
}

variable "tags" {
  description = "共通タグ"
  type        = map(string)
  default     = {}
}
