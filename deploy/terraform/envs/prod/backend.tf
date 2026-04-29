// prod 環境の Terraform state 配置先。
// state バケットは dev と分離（誤操作防止）。
terraform {
  backend "s3" {
    bucket         = "sync-files-go-tfstate-prod"
    key            = "envs/prod/terraform.tfstate"
    region         = "ap-northeast-1"
    dynamodb_table = "sync-files-go-tfstate-lock"
    encrypt        = true
  }
}
