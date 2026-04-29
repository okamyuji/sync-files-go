#!/usr/bin/env bash
# seed-secrets.sh
#
# Secrets Manager に 6 つのシークレットの初期値を投入する。
# - 4 種のキー (aes_master / totp_hmac / csrf / session): ランダム 32 bytes を base64
# - db_password: 24 文字のランダム英数字（rdsadmin が manage_master_user_password=true で
#   Secrets Manager 直管理の場合は実質不要だが、sync_app ユーザのパスワードは別管理する想定）
# - cloudflared_token: ユーザに対話で入力させる（Cloudflare Zero Trust ダッシュボードで発行）
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
PREFIX="sync-files-go/${ENV}"

random_base64_32() { openssl rand -base64 32 | tr -d '\n'; }
random_alnum_24()  { openssl rand -base64 24 | tr -dc 'A-Za-z0-9' | head -c 24; }

put() {
  local NAME="$1"
  local VALUE="$2"
  local CURRENT=""
  CURRENT=$(aws secretsmanager get-secret-value --region "$REGION" --secret-id "$NAME" \
    --query 'SecretString' --output text 2>/dev/null || true)
  if [[ -n "$CURRENT" && "$CURRENT" != "None" && -z "$FORCE" ]]; then
    echo "[seed-secrets] skip $NAME (already set; use --force to overwrite)"
    return
  fi
  aws secretsmanager put-secret-value --region "$REGION" \
    --secret-id "$NAME" --secret-string "$VALUE" >/dev/null
  echo "[seed-secrets] wrote $NAME"
}

# --- DB password (sync_app 用) ---
put "${PREFIX}/db/password" "$(random_alnum_24)"

# --- 4 keys ---
put "${PREFIX}/aes/master-key" "$(random_base64_32)"
put "${PREFIX}/totp/hmac-key"  "$(random_base64_32)"
put "${PREFIX}/csrf/key"       "$(random_base64_32)"
put "${PREFIX}/session/key"    "$(random_base64_32)"

# --- Cloudflare Tunnel token ---
EXISTING=$(aws secretsmanager get-secret-value --region "$REGION" \
  --secret-id "${PREFIX}/cloudflared/token" --query 'SecretString' --output text 2>/dev/null || true)
if [[ -n "$EXISTING" && "$EXISTING" != "None" && -z "$FORCE" ]]; then
  echo "[seed-secrets] skip cloudflared token (already set)"
else
  echo
  echo "Cloudflare Zero Trust ダッシュボードで Tunnel を作成し、トークンを取得してください。"
  echo "https://one.dash.cloudflare.com/  →  Networks > Tunnels > Create a tunnel"
  echo
  read -r -p "Cloudflare Tunnel token: " TOKEN
  if [[ -z "$TOKEN" ]]; then
    echo "[seed-secrets] empty token; skipping (re-run later with --force)"
  else
    aws secretsmanager put-secret-value --region "$REGION" \
      --secret-id "${PREFIX}/cloudflared/token" --secret-string "$TOKEN" >/dev/null
    echo "[seed-secrets] wrote cloudflared token"
  fi
fi

echo "[seed-secrets] done."
