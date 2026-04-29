// Package network 単一 EC2 + RDS Multi-AZ のための最小 VPC。
//
// 構成:
//   - VPC 10.0.0.0/16
//   - Public Subnet × 1 (EC2、IGW 経由 outbound)
//   - Private Subnet × 2 (RDS Multi-AZ + Replica。RDS subnet group は最低 2 AZ 必須)
//   - 3 SG: ec2 (80/443 from 0.0.0.0/0)、rds (3306 from ec2)、（vpce 不要、IGW 経由で AWS API へ到達）
//
// 設計判断: VPC エンドポイントは廃止（EC2 1 台 + IGW で十分。コスト 1 endpoint $7/月 × 4 = $28/月 を削減）。
// AWS API への通信は IGW 経由で TLS 1.3、IAM 認証なので機密性問題なし。

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

// === Public Subnet (EC2) ===

resource "aws_subnet" "public" {
  vpc_id                  = aws_vpc.main.id
  cidr_block              = cidrsubnet(var.vpc_cidr, 8, 1) // 10.0.1.0/24
  availability_zone       = local.azs[0]
  map_public_ip_on_launch = true
  tags = merge(local.tags, {
    Name = "sync-files-go-${var.env}-public"
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
  subnet_id      = aws_subnet.public.id
  route_table_id = aws_route_table.public.id
}

// === Private Subnets (RDS Multi-AZ; subnet group requires >=2 AZ) ===

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

// Private Subnet ルートテーブル（外向きルートなし。RDS は VPC 内通信のみ）
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

// EC2 SG: 80/443 はインターネットから許可、SSH は閉じる (SSM Session Manager 経由)
resource "aws_security_group" "ec2" {
  name        = "sync-files-go-${var.env}-ec2"
  description = "EC2 instance SG (HTTP 80 + HTTPS 443 from internet, no SSH)"
  vpc_id      = aws_vpc.main.id
  tags        = merge(local.tags, { Name = "sync-files-go-${var.env}-ec2-sg" })
}

resource "aws_vpc_security_group_ingress_rule" "ec2_http" {
  security_group_id = aws_security_group.ec2.id
  description       = "HTTP from internet (Let's Encrypt ACME http-01 + redirect)"
  ip_protocol       = "tcp"
  from_port         = 80
  to_port           = 80
  cidr_ipv4         = "0.0.0.0/0"
}

resource "aws_vpc_security_group_ingress_rule" "ec2_https" {
  security_group_id = aws_security_group.ec2.id
  description       = "HTTPS from internet"
  ip_protocol       = "tcp"
  from_port         = 443
  to_port           = 443
  cidr_ipv4         = "0.0.0.0/0"
}

// outbound: 全許可（RDS / S3 / Secrets / OS update / ACME 検証 etc）
resource "aws_vpc_security_group_egress_rule" "ec2_outbound" {
  security_group_id = aws_security_group.ec2.id
  description       = "Outbound all (RDS / AWS APIs / OS updates / ACME)"
  ip_protocol       = "-1"
  cidr_ipv4         = "0.0.0.0/0"
}

// RDS SG: 3306 from EC2 SG only
resource "aws_security_group" "rds" {
  name        = "sync-files-go-${var.env}-rds"
  description = "RDS MySQL SG (3306 from EC2 only)"
  vpc_id      = aws_vpc.main.id
  tags        = merge(local.tags, { Name = "sync-files-go-${var.env}-rds-sg" })
}

resource "aws_vpc_security_group_ingress_rule" "rds_from_ec2" {
  security_group_id            = aws_security_group.rds.id
  description                  = "MySQL from EC2"
  ip_protocol                  = "tcp"
  from_port                    = 3306
  to_port                      = 3306
  referenced_security_group_id = aws_security_group.ec2.id
}
