// prod 環境のエントリーポイント。dev と同じ module 構成、prod 強化のみ差分。

terraform {
  required_version = ">= 1.7.0"
  required_providers {
    aws = { source = "hashicorp/aws", version = "~> 5.60" }
  }
}

provider "aws" {
  region  = var.aws_region
  profile = var.aws_profile
  default_tags {
    tags = {
      Project   = "sync-files-go"
      Env       = "prod"
      ManagedBy = "terraform"
    }
  }
}

locals {
  env  = "prod"
  tags = { Project = "sync-files-go", Env = local.env }
}

module "network" {
  source = "../../modules/network"
  env    = local.env
  tags   = local.tags
}

module "secrets" {
  source                  = "../../modules/secrets"
  env                     = local.env
  recovery_window_in_days = 30
  tags                    = local.tags
}

module "s3files" {
  source = "../../modules/s3files"
  env    = local.env
  tags   = local.tags
}

module "rds" {
  source                = "../../modules/rds"
  env                   = local.env
  private_subnet_ids    = module.network.private_subnet_ids
  rds_security_group_id = module.network.rds_security_group_id
  db_instance_class     = var.db_instance_class
  tags                  = local.tags
}

module "ec2" {
  source                  = "../../modules/ec2"
  env                     = local.env
  instance_type           = var.instance_type
  data_volume_gb          = var.data_volume_gb
  snapshot_retention_days = 30
  public_subnet_id        = module.network.public_subnet_id
  ec2_security_group_id   = module.network.ec2_security_group_id
  secret_arns             = module.secrets.secret_arns
  s3_backup_bucket_id     = module.s3files.bucket_id
  s3_backup_bucket_arn    = module.s3files.bucket_arn
  db_primary_host         = module.rds.primary_endpoint
  db_replica_host         = module.rds.replica_endpoint
  domain_name             = var.domain_name
  letsencrypt_email       = var.letsencrypt_email
  base_url                = var.base_url
  log_level               = "info"
  tags                    = local.tags
}

module "observability" {
  source                 = "../../modules/observability"
  env                    = local.env
  ec2_instance_id        = module.ec2.instance_id
  rds_primary_identifier = module.rds.primary_identifier
  rds_replica_identifier = module.rds.replica_identifier
  alert_emails           = var.alert_emails
  tags                   = local.tags
}
