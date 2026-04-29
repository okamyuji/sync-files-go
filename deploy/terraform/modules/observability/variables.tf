variable "env" {
  description = "環境名"
  type        = string
}

variable "ec2_instance_id" {
  description = "監視対象 EC2 InstanceId"
  type        = string
}

variable "rds_primary_identifier" {
  description = "監視対象 RDS Primary の DBInstanceIdentifier"
  type        = string
}

variable "rds_replica_identifier" {
  description = "監視対象 RDS Replica の DBInstanceIdentifier"
  type        = string
}

variable "alert_emails" {
  description = "アラートを受信するメールアドレス（subscription confirmation 必須）"
  type        = list(string)
  default     = []
}

variable "tags" {
  type    = map(string)
  default = {}
}
