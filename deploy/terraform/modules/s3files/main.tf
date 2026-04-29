// Package s3files バックエンド S3 バケット + ライフサイクル。
//
// 設計書: docs/09-infrastructure-and-deployment.md §7
//
// 注: S3 Files (file system + access point) 自体は AWS provider 未対応のため、
// `deploy/scripts/setup-s3files.sh` を使った CLI 手動作成。このモジュールは
// バックエンド S3 バケットとそのライフサイクル・暗号化のみを管理する。

terraform {
  required_version = ">= 1.7.0"
  required_providers {
    aws = { source = "hashicorp/aws", version = "~> 5.60" }
  }
}

data "aws_caller_identity" "current" {}

locals {
  tags = merge(var.tags, {
    Project = "sync-files-go"
    Env     = var.env
    Module  = "s3files"
  })
  bucket_name = "sync-files-go-${var.env}-data-${data.aws_caller_identity.current.account_id}"
}

resource "aws_s3_bucket" "data" {
  bucket        = local.bucket_name
  force_destroy = var.env != "prod"
  tags          = merge(local.tags, { Name = local.bucket_name })
}

resource "aws_s3_bucket_versioning" "data" {
  bucket = aws_s3_bucket.data.id
  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "data" {
  bucket = aws_s3_bucket.data.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
    bucket_key_enabled = true
  }
}

resource "aws_s3_bucket_public_access_block" "data" {
  bucket                  = aws_s3_bucket.data.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_lifecycle_configuration" "data" {
  bucket = aws_s3_bucket.data.id

  // 1) 不完全 multipart upload は 7 日で abort
  rule {
    id     = "cleanup-incomplete-multipart"
    status = "Enabled"
    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
    filter {}
  }

  // 2) DeleteMarker 配下の noncurrent version を 90 日で完全消去
  //    immutable key 設計により S3 自体の noncurrent_version_expiration では消えない旧版は
  //    アプリ層 (cmd/batch prune-old-versions) が削除する。S3 側は「物理削除で付いた DeleteMarker」のみ。
  rule {
    id     = "expire-deleted-objects-noncurrent"
    status = "Enabled"
    noncurrent_version_expiration {
      noncurrent_days = 90
    }
    filter {
      prefix = "owner-"
    }
  }

  // 3) tmp/ ディレクトリは 7 日で消す（アップロード途中の捨てファイル対策）
  rule {
    id     = "expire-tmp"
    status = "Enabled"
    expiration {
      days = 7
    }
    filter {
      and {
        prefix = "owner-"
        tags = {
          tmp = "true"
        }
      }
    }
  }
}
