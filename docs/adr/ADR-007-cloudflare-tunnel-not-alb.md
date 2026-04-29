# ADR-007: 外部公開は Cloudflare Tunnel + サイドカー nginx。ALB を不採用

## ステータス

採択 (2026-04-29)。当初 ALB 採択だったがコスト過大とユーザ指示に基づき変更。

## コンテキスト

ECS Fargate のアプリを外部に公開する手段として、4 通りを検討：

1. ALB（Application Load Balancer）
2. NLB + サイドカー nginx
3. **Cloudflare Tunnel + サイドカー nginx** ← 採択
4. EC2 1 台に nginx + アプリを同居（Fargate を諦める）

## 検討内容

### 選択肢 A: ALB

利点：
- AWS 内で完結
- SSL 終端（ACM）・WAF 連携が宣言的
- ヘルスチェックや AutoScaling 連携が定型

欠点：
- 月額 $20 + LCU。個人用には過大
- Public Subnet が必要、IGW 経由で外部公開

### 選択肢 B: NLB + サイドカー nginx

利点：
- ALB より $4 程度安い
- nginx で柔軟に L7 制御

欠点：
- それでも月額 $16 + 課金。個人用には依然として大きい
- TLS 終端は nginx で（証明書管理が要る）

### 選択肢 C: Cloudflare Tunnel + サイドカー nginx ← **採択**

`cloudflared` コンテナを ECS タスク内に同居させ、Cloudflare 側にアウトバウンド接続でトンネルを張る。Cloudflare DNS が外部リクエストを受け、トンネル経由で nginx → アプリへ。

利点：
- **ロードバランサ料金ゼロ**
- TLS 終端は Cloudflare（証明書管理不要）
- DDoS 対策が Cloudflare 側で自動
- VPC は Public Subnet 不要、ECS タスクは Outbound のみ
- IP が変動する Fargate でも問題なし（cloudflared が外向きに接続するため）

欠点：
- Cloudflare に依存（Cloudflare 障害時は到達不能 → R-05 として明示）
- メタデータ（ホスト名 / リクエスト時刻 / IP）が Cloudflare を経由（中身はアプリ層暗号化により無関係 → R-06）
- Free プランの制限（2GB 上限以下のリクエストなら問題ないと想定、要実測 → 13§3.4 で確認）

### 選択肢 D: EC2 1 台ホスト

利点：最安

欠点：Fargate / RDS / S3 Files の設計から離れ、再設計コスト。Multi-AZ も犠牲

## 決定

**選択肢 C（Cloudflare Tunnel + サイドカー nginx）** を採択。

具体構成：
- ECS Fargate タスク内に 3 コンテナ：
  - `app` (Go バックエンド、:8080)
  - `nginx` （ローカル proxy、ヘッダ整形、レート制限の補助、:8443 -> :8080）
  - `cloudflared` （Cloudflare Tunnel コネクタ、設定は Tunnel ID + 認証トークン）
- VPC: Single Public Subnet（タスクは Outbound のみ、Inbound は SG で全 deny）
- Cloudflare DNS: `sync.example.com` を Tunnel に紐づけ
- TLS：Cloudflare ↔ ユーザブラウザは TLS 1.3（Cloudflare 管理）。Cloudflare ↔ cloudflared はトンネル内で TLS 暗号化
- 認証：cloudflared の認証トークンは Secrets Manager から取得

緊急時の代替経路（Cloudflare 障害時）：
- Runbook に「ALB を一時的に立てる」手順を残す
- アプリは IP を意識しない構成なので、ALB を後付け可能

## 帰結

- 02-architecture.md と 09-infrastructure-and-deployment.md を改訂
- ALB / Route 53 / ACM / Public Subnet の構成を削除
- Cloudflare Tunnel のセットアップ手順を 10-operations.md に追加
- ヘルスチェックは ECS タスクヘルスチェックのみ（ALB ヘルスチェックは無いため）
- nginx の設定例：

```nginx
worker_processes auto;
events { worker_connections 1024; }
http {
  proxy_buffering on;
  proxy_request_buffering on;
  client_max_body_size 2200m;   # 2GB + マージン

  # レート制限（補助）
  limit_req_zone $http_x_forwarded_for zone=ip:10m rate=300r/m;

  server {
    listen 8443 ssl http2;
    ssl_certificate     /etc/nginx/cert.pem;
    ssl_certificate_key /etc/nginx/key.pem;
    ssl_protocols       TLSv1.3;

    # Cloudflare 経由のため、X-Forwarded-For を信頼するのは Cloudflare の IP のみ
    set_real_ip_from 173.245.48.0/20;
    set_real_ip_from 103.21.244.0/22;
    # ...（Cloudflare の IP レンジを反映）
    real_ip_header CF-Connecting-IP;

    location /healthz {
      proxy_pass http://127.0.0.1:8080/healthz;
    }

    location / {
      limit_req zone=ip burst=200 nodelay;

      proxy_pass http://127.0.0.1:8080;
      proxy_set_header Host              $host;
      proxy_set_header X-Real-IP         $remote_addr;
      proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
      proxy_set_header X-Forwarded-Proto https;

      # SSE のため
      proxy_buffering           off;
      proxy_read_timeout        300s;
      proxy_set_header Connection '';
      chunked_transfer_encoding off;
    }
  }
}
```

cloudflared の設定は Cloudflare Zero Trust ダッシュボードで作成し、認証トークンを Secrets Manager に保管。

## リンク

- [`02-architecture.md`](../02-architecture.md)
- [`09-infrastructure-and-deployment.md`](../09-infrastructure-and-deployment.md)
- [Cloudflare Tunnel 公式](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/)
