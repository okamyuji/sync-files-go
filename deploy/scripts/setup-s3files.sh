#!/usr/bin/env bash
# setup-s3files.sh
#
# Phase 6 で Terraform AWS provider が S3 Files をまだサポートしないため、
# AWS CLI で「永続データボリューム」を手動作成する。
#
# 当面は EFS を使う実装にしているので、デフォルトでは EFS file system + access point を作成する。
# 将来 S3 Files が利用可能になったら、後半の S3 Files セクションのコメントアウトを外す。
#
# 使い方:
#   ./setup-s3files.sh dev
#   ./setup-s3files.sh prod
#
# 出力: 環境変数として TF_VAR_efs_file_system_id / TF_VAR_efs_access_point_id を export するブロックを stdout に出す。

set -euo pipefail

ENV="${1:-}"
if [[ -z "$ENV" || ( "$ENV" != "dev" && "$ENV" != "prod" ) ]]; then
  echo "usage: $0 <dev|prod>" >&2
  exit 2
fi

REGION="${AWS_REGION:-ap-northeast-1}"
NAME="sync-files-go-${ENV}-data"

echo "[setup-s3files] env=${ENV} region=${REGION}"

# ===== 1) VPC とサブネットを Terraform output から取得 =====
TF_DIR="$(cd "$(dirname "$0")/../terraform/envs/${ENV}" && pwd)"
echo "[setup-s3files] reading terraform outputs from ${TF_DIR}"
if ! VPC_ID=$(cd "$TF_DIR" && terraform output -raw vpc_id 2>/dev/null); then
  echo "ERROR: terraform output vpc_id がありません。先に network module を apply してください。" >&2
  exit 3
fi
SUBNETS=$(cd "$TF_DIR" && terraform output -json | jq -r '.private_subnet_ids // empty')
if [[ -z "$SUBNETS" ]]; then
  # private_subnet_ids が outputs に無いなら network module の output を直で取る
  SUBNETS=$(aws ec2 describe-subnets --region "$REGION" \
    --filters "Name=vpc-id,Values=$VPC_ID" "Name=tag:Tier,Values=private" \
    --query 'Subnets[*].SubnetId' --output json)
fi
echo "[setup-s3files] vpc=${VPC_ID} subnets=${SUBNETS}"

# ===== 2) EFS file system =====
EXISTING=$(aws efs describe-file-systems --region "$REGION" \
  --query "FileSystems[?Tags[?Key=='Name' && Value=='${NAME}']].FileSystemId" --output text || echo "")

if [[ -n "$EXISTING" && "$EXISTING" != "None" ]]; then
  FS_ID="$EXISTING"
  echo "[setup-s3files] reuse existing EFS FileSystemId=${FS_ID}"
else
  echo "[setup-s3files] creating EFS file system ${NAME}"
  FS_ID=$(aws efs create-file-system --region "$REGION" \
    --performance-mode generalPurpose \
    --throughput-mode bursting \
    --encrypted \
    --tags "Key=Name,Value=${NAME}" "Key=Project,Value=sync-files-go" "Key=Env,Value=${ENV}" \
    --query 'FileSystemId' --output text)
  echo "[setup-s3files] waiting for available state..."
  while true; do
    STATE=$(aws efs describe-file-systems --region "$REGION" --file-system-id "$FS_ID" \
      --query 'FileSystems[0].LifeCycleState' --output text)
    [[ "$STATE" == "available" ]] && break
    sleep 5
  done
fi

# ===== 3) Mount targets を private subnet 全部に作成 =====
SG_ID=$(aws ec2 describe-security-groups --region "$REGION" \
  --filters "Name=vpc-id,Values=$VPC_ID" "Name=group-name,Values=sync-files-go-${ENV}-rds" \
  --query 'SecurityGroups[0].GroupId' --output text)
# EFS には ECS SG からの 2049/tcp を許可する SG が必要
EFS_SG_NAME="sync-files-go-${ENV}-efs"
EFS_SG_ID=$(aws ec2 describe-security-groups --region "$REGION" \
  --filters "Name=vpc-id,Values=$VPC_ID" "Name=group-name,Values=${EFS_SG_NAME}" \
  --query 'SecurityGroups[0].GroupId' --output text 2>/dev/null || echo "None")
if [[ "$EFS_SG_ID" == "None" || -z "$EFS_SG_ID" ]]; then
  EFS_SG_ID=$(aws ec2 create-security-group --region "$REGION" \
    --group-name "$EFS_SG_NAME" --description "EFS NFS access from ECS" --vpc-id "$VPC_ID" \
    --query 'GroupId' --output text)
  ECS_SG_ID=$(aws ec2 describe-security-groups --region "$REGION" \
    --filters "Name=vpc-id,Values=$VPC_ID" "Name=group-name,Values=sync-files-go-${ENV}-ecs" \
    --query 'SecurityGroups[0].GroupId' --output text)
  aws ec2 authorize-security-group-ingress --region "$REGION" \
    --group-id "$EFS_SG_ID" --protocol tcp --port 2049 --source-group "$ECS_SG_ID" >/dev/null
fi

for SUBNET in $(echo "$SUBNETS" | jq -r '.[]'); do
  EXISTS=$(aws efs describe-mount-targets --region "$REGION" --file-system-id "$FS_ID" \
    --query "MountTargets[?SubnetId=='${SUBNET}']" --output text)
  if [[ -z "$EXISTS" ]]; then
    echo "[setup-s3files] creating mount target on $SUBNET"
    aws efs create-mount-target --region "$REGION" \
      --file-system-id "$FS_ID" --subnet-id "$SUBNET" --security-groups "$EFS_SG_ID" >/dev/null
  fi
done

# ===== 4) Access Point =====
AP_ID=$(aws efs describe-access-points --region "$REGION" \
  --query "AccessPoints[?Tags[?Key=='Name' && Value=='${NAME}-ap']].AccessPointId" --output text)
if [[ -z "$AP_ID" || "$AP_ID" == "None" ]]; then
  AP_ID=$(aws efs create-access-point --region "$REGION" \
    --file-system-id "$FS_ID" \
    --posix-user "Uid=65532,Gid=65532" \
    --root-directory '{"Path":"/data","CreationInfo":{"OwnerUid":65532,"OwnerGid":65532,"Permissions":"0755"}}' \
    --tags "Key=Name,Value=${NAME}-ap" "Key=Project,Value=sync-files-go" "Key=Env,Value=${ENV}" \
    --query 'AccessPointId' --output text)
fi

cat <<EOF

=== setup-s3files complete ===
FileSystemId : ${FS_ID}
AccessPointId: ${AP_ID}

terraform.tfvars に以下を入れてください:

  efs_file_system_id  = "${FS_ID}"
  efs_access_point_id = "${AP_ID}"

または環境変数として export:

  export TF_VAR_efs_file_system_id=${FS_ID}
  export TF_VAR_efs_access_point_id=${AP_ID}
EOF
