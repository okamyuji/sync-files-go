// Package ecr 2 つの ECR リポジトリ (app, nginx)。
//
// 設計書: docs/09-infrastructure-and-deployment.md §11
//
// - image_tag_mutability = IMMUTABLE（タグ書き換え不可で再現性を担保）
// - scan_on_push = true（CVE スキャン）
// - lifecycle: 直近 30 タグだけ保持

terraform {
  required_version = ">= 1.7.0"
  required_providers {
    aws = { source = "hashicorp/aws", version = "~> 5.60" }
  }
}

locals {
  tags = merge(var.tags, {
    Project = "sync-files-go"
    Env     = var.env
    Module  = "ecr"
  })
  repos = {
    app   = "sync-files-go/app"
    nginx = "sync-files-go/nginx"
  }
}

resource "aws_ecr_repository" "this" {
  for_each             = local.repos
  name                 = each.value
  image_tag_mutability = "IMMUTABLE"

  image_scanning_configuration {
    scan_on_push = true
  }

  encryption_configuration {
    encryption_type = "AES256"
  }

  tags = merge(local.tags, { Name = each.value })
}

// 直近 30 タグのみ保持。`<sha>-<ts>` 形式のタグを古い順に消す。
resource "aws_ecr_lifecycle_policy" "this" {
  for_each   = aws_ecr_repository.this
  repository = each.value.name

  policy = jsonencode({
    rules = [
      {
        rulePriority = 1
        description  = "Keep last 30 tagged images"
        selection = {
          tagStatus      = "tagged"
          tagPatternList = ["*"]
          countType      = "imageCountMoreThan"
          countNumber    = 30
        }
        action = { type = "expire" }
      },
      {
        rulePriority = 2
        description  = "Delete untagged after 7 days"
        selection = {
          tagStatus   = "untagged"
          countType   = "sinceImagePushed"
          countUnit   = "days"
          countNumber = 7
        }
        action = { type = "expire" }
      }
    ]
  })
}
