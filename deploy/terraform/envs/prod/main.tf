// prod 環境のエントリーポイント。dev とほぼ同じだが env=prod、deletion_protection、ログ保持 90 日 etc。

terraform {
  required_version = ">= 1.7.0"
  required_providers {
    aws = { source = "hashicorp/aws", version = "~> 5.60" }
  }
}

provider "aws" {
  region = var.aws_region
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

module "ecr" {
  source = "../../modules/ecr"
  env    = local.env
  tags   = local.tags
}

module "secrets" {
  source                  = "../../modules/secrets"
  env                     = local.env
  recovery_window_in_days = 30 // prod は 30 日論理削除猶予
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

module "observability" {
  source                 = "../../modules/observability"
  env                    = local.env
  ecs_cluster_name       = "sync-files-go-${local.env}"
  ecs_service_name       = "sync-files-go"
  rds_replica_identifier = "sync-files-go-${local.env}-replica-1"
  alert_emails           = var.alert_emails
  tags                   = local.tags
}

module "ecs" {
  source                = "../../modules/ecs"
  env                   = local.env
  image_tag             = var.image_tag
  app_image_url         = module.ecr.app_repository_url
  nginx_image_url       = module.ecr.nginx_repository_url
  public_subnet_ids     = module.network.public_subnet_ids
  ecs_security_group_id = module.network.ecs_security_group_id
  secret_arns           = module.secrets.secret_arns
  db_primary_host       = module.rds.primary_endpoint
  db_replica_host       = module.rds.replica_endpoint
  base_url              = var.base_url
  log_level             = "info"
  log_group_app         = module.observability.log_group_names["app"]
  log_group_nginx       = module.observability.log_group_names["nginx"]
  log_group_cloudflared = module.observability.log_group_names["cloudflared"]
  s3_backend_bucket_arn = module.s3files.bucket_arn

  efs_file_system_id  = var.efs_file_system_id
  efs_access_point_id = var.efs_access_point_id

  tags = local.tags
}

module "batch" {
  source                = "../../modules/batch"
  env                   = local.env
  image_tag             = var.image_tag
  app_image_url         = module.ecr.app_repository_url
  ecs_cluster_arn       = module.ecs.cluster_arn
  public_subnet_ids     = module.network.public_subnet_ids
  ecs_security_group_id = module.network.ecs_security_group_id
  secret_arns           = module.secrets.secret_arns
  db_primary_host       = module.rds.primary_endpoint
  db_replica_host       = module.rds.replica_endpoint
  log_group_batch       = module.observability.batch_log_group_name
  tags                  = local.tags
}
