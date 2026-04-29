variable "env" {
  description = "環境名"
  type        = string
}

variable "ecs_cluster_name" {
  description = "監視対象 ECS クラスタ名"
  type        = string
}

variable "ecs_service_name" {
  description = "監視対象 ECS サービス名"
  type        = string
}

variable "rds_replica_identifier" {
  description = "監視対象 RDS Replica の DBInstanceIdentifier"
  type        = string
}

variable "alert_emails" {
  description = "アラートを SNS 経由で受信するメールアドレス（subscription confirmation が手動で必要）"
  type        = list(string)
  default     = []
}

variable "tags" {
  description = "共通タグ"
  type        = map(string)
  default     = {}
}
