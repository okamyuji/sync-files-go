variable "env" {
  description = "環境名 (dev / prod)"
  type        = string
}

variable "vpc_cidr" {
  description = "VPC の CIDR ブロック"
  type        = string
  default     = "10.0.0.0/16"
}

variable "tags" {
  description = "全リソースに付ける共通タグ"
  type        = map(string)
  default     = {}
}
