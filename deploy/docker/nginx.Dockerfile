# syntax=docker/dockerfile:1.7
#
# nginx サイドカー
#  - cloudflared から localhost:8443 で受け、app:8080 へ proxy
#  - 設計書 ADR-007 準拠: TLS 終端、Cloudflare CF-Connecting-IP 信頼、SSE 透過、レート制限
#
# read_only_root_filesystem 下で動かすため、cache / pid / tmp は volume で受ける（タスク定義側）

FROM nginx:1.27-alpine

# 自己署名証明書（cloudflared ↔ nginx 内部接続用、外部には出ない）
# 本番では deploy/scripts/gen-internal-cert.sh で生成して COPY するが、Phase 1 ではダミー。
# nginx 起動時に存在確認するため、ビルド時に空で作成しておき、entrypoint で再生成する手もある。
RUN apk add --no-cache openssl curl \
 && mkdir -p /etc/nginx/internal \
 && openssl req -x509 -newkey rsa:2048 -nodes \
        -days 3650 -subj "/CN=localhost" \
        -keyout /etc/nginx/internal/key.pem \
        -out    /etc/nginx/internal/cert.pem \
 && chmod 644 /etc/nginx/internal/cert.pem \
 && chmod 600 /etc/nginx/internal/key.pem

COPY deploy/docker/nginx.conf /etc/nginx/nginx.conf

# nonroot ユーザを作成 (UID/GID は ECS タスクと揃える)
RUN adduser -D -H -u 65532 nonroot \
 && chown -R nonroot:nonroot /etc/nginx /var/cache/nginx /var/log/nginx /run

USER 65532:65532

EXPOSE 8443

# health check 用の curl を内蔵（タスク定義の healthCheck から呼ぶ）
HEALTHCHECK --interval=15s --timeout=5s --retries=3 --start-period=20s \
  CMD wget -qO- --no-check-certificate https://127.0.0.1:8443/healthz || exit 1

CMD ["nginx", "-g", "daemon off;"]
