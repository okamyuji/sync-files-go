// dev 環境のエントリーポイント。
//
// 構成 (ADR-007 改訂後):
//   network → secrets → s3files (バックアップバケット) → rds → ec2 → observability
//
// 適用順:
//   1. terraform init
//   2. terraform apply -target=module.network -target=module.secrets -target=module.s3files
//   3. deploy/scripts/seed-secrets.sh dev で 5 シークレットに値を入れる
//   4. terraform apply で rds + ec2 + observability を反映 (RDS 起動に 10〜20 分)
//   5. terraform output elastic_ip を取得 → ユーザの DNS プロバイダで A レコード登録
//   6. DNS 浸透後、SSM Session Manager で EC2 に入り `sudo certbot --nginx -d <domain>` 再実行
//   7. ローカルから `make deploy ENV=dev` で Go バイナリを SCP + systemctl restart

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
      Env       = "dev"
      ManagedBy = "terraform"
    }
  }
}

locals {
  env  = "dev"
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
  recovery_window_in_days = 0
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
  source                = "../../modules/ec2"
  env                   = local.env
  instance_type         = var.instance_type
  public_subnet_id      = module.network.public_subnet_id
  ec2_security_group_id = module.network.ec2_security_group_id
  secret_arns           = module.secrets.secret_arns
  s3_backup_bucket_id   = module.s3files.bucket_id
  s3_backup_bucket_arn  = module.s3files.bucket_arn
  db_primary_host       = module.rds.primary_endpoint
  db_replica_host       = module.rds.replica_endpoint
  domain_name           = var.domain_name
  letsencrypt_email     = var.letsencrypt_email
  base_url              = var.base_url
  log_level             = "debug"
  tags                  = local.tags
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
