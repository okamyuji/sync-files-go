// Package ec2 単一 EC2 (Amazon Linux 2023, arm64) + Elastic IP + IAM instance profile + user_data。
//
// 設計: ADR-007 (NLB/ALB/Cloudflare Tunnel いずれも不採用、EC2 1 台 + nginx + Let's Encrypt)
//
// セキュリティ:
//   - SSH 港は閉じる（SG で 22/tcp は許可しない）。OS 操作は SSM Session Manager 経由
//   - EBS は KMS で暗号化（gp3）
//   - IMDSv2 強制 (http_tokens=required) で SSRF→IMDS 漏洩を防ぐ
//   - unattended-upgrades と CloudWatch agent を user_data で有効化
//   - Instance profile: Secrets read / S3 backup write / SSM / CloudWatch のみ最小権限

terraform {
  required_version = ">= 1.7.0"
  required_providers {
    aws = { source = "hashicorp/aws", version = "~> 5.60" }
  }
}

data "aws_region" "current" {}
data "aws_caller_identity" "current" {}

// Amazon Linux 2023 arm64 の最新 AMI
data "aws_ami" "al2023_arm64" {
  most_recent = true
  owners      = ["amazon"]
  filter {
    name   = "name"
    values = ["al2023-ami-2023.*-arm64"]
  }
  filter {
    name   = "architecture"
    values = ["arm64"]
  }
  filter {
    name   = "virtualization-type"
    values = ["hvm"]
  }
}

locals {
  tags = merge(var.tags, {
    Project = "sync-files-go"
    Env     = var.env
    Module  = "ec2"
  })
  hostname = "sync-files-go-${var.env}"
}

// === IAM: Instance profile ===

data "aws_iam_policy_document" "assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "instance" {
  name               = "sync-files-go-${var.env}-ec2"
  assume_role_policy = data.aws_iam_policy_document.assume.json
  tags               = local.tags
}

// SSM Session Manager
resource "aws_iam_role_policy_attachment" "ssm_managed" {
  role       = aws_iam_role.instance.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

// CloudWatch agent
resource "aws_iam_role_policy_attachment" "cloudwatch_agent" {
  role       = aws_iam_role.instance.name
  policy_arn = "arn:aws:iam::aws:policy/CloudWatchAgentServerPolicy"
}

// Secrets Manager 読み取り（5 シークレット）+ S3 backup 書き込み
data "aws_iam_policy_document" "app_runtime" {
  statement {
    actions   = ["secretsmanager:GetSecretValue"]
    resources = values(var.secret_arns)
  }
  statement {
    actions = [
      "s3:GetObject",
      "s3:PutObject",
      "s3:DeleteObject",
      "s3:ListBucket",
    ]
    resources = [
      var.s3_backup_bucket_arn,
      "${var.s3_backup_bucket_arn}/*",
    ]
  }
}

resource "aws_iam_role_policy" "app_runtime" {
  name   = "app-runtime"
  role   = aws_iam_role.instance.id
  policy = data.aws_iam_policy_document.app_runtime.json
}

resource "aws_iam_instance_profile" "instance" {
  name = "sync-files-go-${var.env}-ec2"
  role = aws_iam_role.instance.name
  tags = local.tags
}

// === EC2 instance ===

// user_data: cloud-init で nginx / certbot / Go バイナリ実行ディレクトリ / systemd unit を準備
locals {
  user_data = templatefile("${path.module}/user_data.sh.tftpl", {
    env               = var.env
    region            = data.aws_region.current.name
    domain_name       = var.domain_name
    letsencrypt_email = var.letsencrypt_email
    db_primary_host   = var.db_primary_host
    db_replica_host   = var.db_replica_host
    db_password_arn   = var.secret_arns["db_password"]
    aes_master_arn    = var.secret_arns["aes_master"]
    totp_hmac_arn     = var.secret_arns["totp_hmac"]
    csrf_arn          = var.secret_arns["csrf"]
    session_arn       = var.secret_arns["session"]
    base_url          = var.base_url
    log_level         = var.log_level
    max_upload_bytes  = var.max_upload_bytes
    s3_backup_bucket  = var.s3_backup_bucket_id
  })
}

resource "aws_instance" "main" {
  ami                         = data.aws_ami.al2023_arm64.id
  instance_type               = var.instance_type
  subnet_id                   = var.public_subnet_id
  vpc_security_group_ids      = [var.ec2_security_group_id]
  iam_instance_profile        = aws_iam_instance_profile.instance.name
  user_data                   = local.user_data
  user_data_replace_on_change = false // user_data 変更で破棄しない（in-place 更新は SSM で）
  associate_public_ip_address = false // EIP を使うので AutoAssign は不要
  monitoring                  = true  // 詳細モニタリング有効

  // IMDSv2 強制
  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 2
  }

  // 暗号化された gp3 ルートボリューム
  root_block_device {
    volume_type           = "gp3"
    volume_size           = var.root_volume_gb
    encrypted             = true
    delete_on_termination = false // 誤削除に備えて termination で残す
    tags                  = merge(local.tags, { Name = "${local.hostname}-root" })
  }

  // /var/data 用の追加 EBS（暗号化）。daily スナップショット → S3 バックアップ
  ebs_block_device {
    device_name           = "/dev/xvdf"
    volume_type           = "gp3"
    volume_size           = var.data_volume_gb
    encrypted             = true
    delete_on_termination = false
    tags                  = merge(local.tags, { Name = "${local.hostname}-data" })
  }

  tags = merge(local.tags, {
    Name     = local.hostname
    Hostname = local.hostname
  })

  lifecycle {
    // EBS マッピングのドリフトを許容（手動でボリューム拡張など）
    ignore_changes = [ami]
  }
}

resource "aws_eip" "main" {
  domain   = "vpc"
  instance = aws_instance.main.id
  tags     = merge(local.tags, { Name = "${local.hostname}-eip" })
}

// === Daily EBS snapshot lifecycle policy (Data Lifecycle Manager) ===

data "aws_iam_policy_document" "dlm_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["dlm.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "dlm" {
  name               = "sync-files-go-${var.env}-dlm"
  assume_role_policy = data.aws_iam_policy_document.dlm_assume.json
  tags               = local.tags
}

resource "aws_iam_role_policy_attachment" "dlm" {
  role       = aws_iam_role.dlm.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AWSDataLifecycleManagerServiceRole"
}

resource "aws_dlm_lifecycle_policy" "daily_snapshot" {
  description        = "sync-files-go ${var.env} daily EBS snapshot (root + data)"
  execution_role_arn = aws_iam_role.dlm.arn
  state              = "ENABLED"

  policy_details {
    resource_types = ["VOLUME"]
    target_tags = {
      Project = "sync-files-go"
      Env     = var.env
    }
    schedule {
      name = "daily-keep-${var.snapshot_retention_days}"
      create_rule {
        interval      = 24
        interval_unit = "HOURS"
        times         = ["19:00"] // UTC 19:00 = JST 04:00
      }
      retain_rule {
        count = var.snapshot_retention_days
      }
      copy_tags = true
      tags_to_add = {
        Snapshot = "daily"
      }
    }
  }

  tags = local.tags
}
