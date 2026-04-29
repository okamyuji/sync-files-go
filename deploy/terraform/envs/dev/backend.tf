// dev 環境の Terraform state 配置先。
// 初回 init 前に S3 バケットと DynamoDB テーブルを手動作成しておく必要がある（chicken-and-egg）。
// 詳細は docs/PHASE6_OPS.md §1。
terraform {
  backend "s3" {
    bucket         = "sync-files-go-tfstate-dev"
    key            = "envs/dev/terraform.tfstate"
    region         = "ap-northeast-1"
    dynamodb_table = "sync-files-go-tfstate-lock"
    encrypt        = true
  }
}
