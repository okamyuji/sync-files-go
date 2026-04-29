// Package network sync-files-go の VPC・サブネット・セキュリティグループ・VPC エンドポイント。
//
// 設計書: docs/09-infrastructure-and-deployment.md §1, §14
//
// 構成:
//   - VPC 10.0.0.0/16
//   - Public Subnets (ECS Fargate タスク用、AZ a / c)
//     ┗ ECS タスクは Public IP 付与で IGW 経由 outbound（NAT Gateway 不採用）
//     ┗ Inbound はセキュリティグループで全 deny（Cloudflare Tunnel が outbound 接続）
//   - Private Subnets (RDS 用、AZ a / c)
//   - VPC エンドポイント: ECR (api + dkr), Secrets Manager, CloudWatch Logs (Interface)
//   - VPC エンドポイント: S3 (Gateway, ECR pull の layer 取得を経由するため必須)

terraform {
  required_version = ">= 1.7.0"
  required_providers {
    aws = { source = "hashicorp/aws", version = "~> 5.60" }
  }
}

data "aws_availability_zones" "available" {
  state = "available"
}

locals {
  azs = slice(data.aws_availability_zones.available.names, 0, 2)
  tags = merge(var.tags, {
    Project = "sync-files-go"
    Env     = var.env
    Module  = "network"
  })
}

resource "aws_vpc" "main" {
  cidr_block           = var.vpc_cidr
  enable_dns_hostnames = true
  enable_dns_support   = true
  tags                 = merge(local.tags, { Name = "sync-files-go-${var.env}-vpc" })
}

resource "aws_internet_gateway" "main" {
  vpc_id = aws_vpc.main.id
  tags   = merge(local.tags, { Name = "sync-files-go-${var.env}-igw" })
}

// === Public Subnets (ECS) ===

resource "aws_subnet" "public" {
  for_each                = { for i, az in local.azs : az => i }
  vpc_id                  = aws_vpc.main.id
  cidr_block              = cidrsubnet(var.vpc_cidr, 8, each.value + 1) // 10.0.1.0/24, 10.0.2.0/24
  availability_zone       = each.key
  map_public_ip_on_launch = true
  tags = merge(local.tags, {
    Name = "sync-files-go-${var.env}-public-${each.key}"
    Tier = "public"
  })
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.main.id
  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.main.id
  }
  tags = merge(local.tags, { Name = "sync-files-go-${var.env}-public-rt" })
}

resource "aws_route_table_association" "public" {
  for_each       = aws_subnet.public
  subnet_id      = each.value.id
  route_table_id = aws_route_table.public.id
}

// === Private Subnets (RDS) ===

resource "aws_subnet" "private" {
  for_each          = { for i, az in local.azs : az => i }
  vpc_id            = aws_vpc.main.id
  cidr_block        = cidrsubnet(var.vpc_cidr, 8, each.value + 11) // 10.0.11.0/24, 10.0.12.0/24
  availability_zone = each.key
  tags = merge(local.tags, {
    Name = "sync-files-go-${var.env}-private-${each.key}"
    Tier = "private"
  })
}

// Private Subnet ルートテーブル（NAT Gateway 不採用なので外向きルート無し。
// VPC エンドポイント経由でしか AWS API に到達しない）
resource "aws_route_table" "private" {
  vpc_id = aws_vpc.main.id
  tags   = merge(local.tags, { Name = "sync-files-go-${var.env}-private-rt" })
}

resource "aws_route_table_association" "private" {
  for_each       = aws_subnet.private
  subnet_id      = each.value.id
  route_table_id = aws_route_table.private.id
}

// === Security Groups ===

// ECS Fargate タスク用 SG。Inbound 全 deny、Outbound のみ許可。
resource "aws_security_group" "ecs" {
  name        = "sync-files-go-${var.env}-ecs"
  description = "ECS Fargate task SG (no inbound, outbound to RDS / Cloudflare / VPCE)"
  vpc_id      = aws_vpc.main.id
  tags        = merge(local.tags, { Name = "sync-files-go-${var.env}-ecs-sg" })
}

resource "aws_vpc_security_group_egress_rule" "ecs_to_rds" {
  security_group_id            = aws_security_group.ecs.id
  description                  = "MySQL to RDS"
  ip_protocol                  = "tcp"
  from_port                    = 3306
  to_port                      = 3306
  referenced_security_group_id = aws_security_group.rds.id
}

resource "aws_vpc_security_group_egress_rule" "ecs_to_vpce" {
  security_group_id            = aws_security_group.ecs.id
  description                  = "HTTPS to VPC endpoints"
  ip_protocol                  = "tcp"
  from_port                    = 443
  to_port                      = 443
  referenced_security_group_id = aws_security_group.vpce.id
}

resource "aws_vpc_security_group_egress_rule" "ecs_to_internet_https" {
  security_group_id = aws_security_group.ecs.id
  description       = "HTTPS to internet (Cloudflare Tunnel + S3 Gateway endpoint via prefix list)"
  ip_protocol       = "tcp"
  from_port         = 443
  to_port           = 443
  cidr_ipv4         = "0.0.0.0/0"
}

resource "aws_vpc_security_group_egress_rule" "ecs_to_internet_http" {
  security_group_id = aws_security_group.ecs.id
  description       = "HTTP for ECR layer download fallback (rare)"
  ip_protocol       = "tcp"
  from_port         = 80
  to_port           = 80
  cidr_ipv4         = "0.0.0.0/0"
}

// RDS SG
resource "aws_security_group" "rds" {
  name        = "sync-files-go-${var.env}-rds"
  description = "RDS MySQL SG (3306 from ECS only)"
  vpc_id      = aws_vpc.main.id
  tags        = merge(local.tags, { Name = "sync-files-go-${var.env}-rds-sg" })
}

resource "aws_vpc_security_group_ingress_rule" "rds_from_ecs" {
  security_group_id            = aws_security_group.rds.id
  description                  = "MySQL from ECS"
  ip_protocol                  = "tcp"
  from_port                    = 3306
  to_port                      = 3306
  referenced_security_group_id = aws_security_group.ecs.id
}

// VPC エンドポイント SG (interface タイプ用)
resource "aws_security_group" "vpce" {
  name        = "sync-files-go-${var.env}-vpce"
  description = "VPC endpoint SG (443 from ECS)"
  vpc_id      = aws_vpc.main.id
  tags        = merge(local.tags, { Name = "sync-files-go-${var.env}-vpce-sg" })
}

resource "aws_vpc_security_group_ingress_rule" "vpce_from_ecs" {
  security_group_id            = aws_security_group.vpce.id
  description                  = "HTTPS from ECS"
  ip_protocol                  = "tcp"
  from_port                    = 443
  to_port                      = 443
  referenced_security_group_id = aws_security_group.ecs.id
}

// === VPC Endpoints ===

data "aws_region" "current" {}

// Gateway endpoint (S3) — ECR layer の取得経路。Public Subnet に紐づける。
resource "aws_vpc_endpoint" "s3" {
  vpc_id            = aws_vpc.main.id
  service_name      = "com.amazonaws.${data.aws_region.current.name}.s3"
  vpc_endpoint_type = "Gateway"
  route_table_ids   = [aws_route_table.public.id, aws_route_table.private.id]
  tags              = merge(local.tags, { Name = "sync-files-go-${var.env}-vpce-s3" })
}

// Interface endpoints
locals {
  interface_endpoints = {
    ecr_api = "ecr.api"
    ecr_dkr = "ecr.dkr"
    secrets = "secretsmanager"
    logs    = "logs"
  }
}

resource "aws_vpc_endpoint" "interface" {
  for_each            = local.interface_endpoints
  vpc_id              = aws_vpc.main.id
  service_name        = "com.amazonaws.${data.aws_region.current.name}.${each.value}"
  vpc_endpoint_type   = "Interface"
  subnet_ids          = [for s in aws_subnet.public : s.id]
  security_group_ids  = [aws_security_group.vpce.id]
  private_dns_enabled = true
  tags                = merge(local.tags, { Name = "sync-files-go-${var.env}-vpce-${each.key}" })
}
