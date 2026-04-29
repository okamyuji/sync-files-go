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

variable "enable_performance_insights" {
  description = "Performance Insights。db.t4g.micro は非対応のため dev では false 推奨"
  type        = bool
  default     = false
}

variable "create_replica" {
  description = "Read Replica を作るか。manage_master_user_password=true と非互換のため、dev では false。prod で有効化する場合は master password を Secrets 直管理に変更する必要あり"
  type        = bool
  default     = false
}

variable "tags" {
  description = "共通タグ"
  type        = map(string)
  default     = {}
}
