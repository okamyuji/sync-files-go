#!/usr/bin/env bash
# seed-secrets.sh
#
# Secrets Manager に 5 つのシークレットの初期値を投入する。
# - 4 種のキー (aes_master / totp_hmac / csrf / session): ランダム 32 bytes を base64
# - db_password: 24 文字のランダム英数字（sync_app ユーザのパスワード。RDS rdsadmin は別管理）
#
# 使い方:
#   ./seed-secrets.sh dev
#   ./seed-secrets.sh prod
#
# 既に値が入っているシークレットは UPDATE しない（誤上書き防止）。--force で上書き。

set -euo pipefail

ENV="${1:-}"
if [[ -z "$ENV" || ( "$ENV" != "dev" && "$ENV" != "prod" ) ]]; then
  echo "usage: $0 <dev|prod> [--force]" >&2
  exit 2
fi
FORCE=""
[[ "${2:-}" == "--force" ]] && FORCE="1"

REGION="${AWS_REGION:-ap-northeast-1}"
PROFILE="${AWS_PROFILE:-sync-admin}"
PREFIX="sync-files-go/${ENV}"

random_base64_32() { openssl rand -base64 32 | tr -d '\n'; }
random_alnum_24()  { openssl rand -base64 24 | tr -dc 'A-Za-z0-9' | head -c 24; }

put() {
  local NAME="$1"
  local VALUE="$2"
  local CURRENT=""
  CURRENT=$(aws --profile "$PROFILE" secretsmanager get-secret-value --region "$REGION" --secret-id "$NAME" \
    --query 'SecretString' --output text 2>/dev/null || true)
  if [[ -n "$CURRENT" && "$CURRENT" != "None" && -z "$FORCE" ]]; then
    echo "[seed-secrets] skip $NAME (already set; use --force to overwrite)"
    return
  fi
  aws --profile "$PROFILE" secretsmanager put-secret-value --region "$REGION" \
    --secret-id "$NAME" --secret-string "$VALUE" >/dev/null
  echo "[seed-secrets] wrote $NAME"
}

# DB password (sync_app 用)
put "${PREFIX}/db/password" "$(random_alnum_24)"

# 4 keys
put "${PREFIX}/aes/master-key" "$(random_base64_32)"
put "${PREFIX}/totp/hmac-key"  "$(random_base64_32)"
put "${PREFIX}/csrf/key"       "$(random_base64_32)"
put "${PREFIX}/session/key"    "$(random_base64_32)"

echo "[seed-secrets] done."
