variable "env" {
  description = "環境名"
  type        = string
}

variable "image_tag" {
  description = "ECR app イメージのタグ (server と共用)"
  type        = string
}

variable "app_image_url" {
  description = "ECR app リポジトリ URL"
  type        = string
}

variable "ecs_cluster_arn" {
  description = "RunTask 対象のクラスタ ARN"
  type        = string
}

variable "public_subnet_ids" {
  description = "バッチタスクを配置する Public サブネット ID"
  type        = list(string)
}

variable "ecs_security_group_id" {
  description = "バッチタスク SG"
  type        = string
}

variable "secret_arns" {
  description = "Secrets Manager ARN マップ"
  type        = map(string)
}

variable "db_primary_host" {
  type = string
}

variable "db_replica_host" {
  type = string
}

variable "log_group_batch" {
  description = "バッチコンテナ用ロググループ名"
  type        = string
}

variable "tags" {
  type    = map(string)
  default = {}
}
