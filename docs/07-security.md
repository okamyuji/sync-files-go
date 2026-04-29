# 07. セキュリティ

> インターネット公開かつ個人専用という制約の下で、現実的に守るべき脅威とその対策を MECE に整理する。

## 1. 脅威モデル（STRIDE）

| カテゴリ | 脅威 | 主な対策 | 節 |
|---|---|---|---|
| **S**poofing | 他人が自分のアカウントになりすます | パスワード + TOTP 2FA / セッション固定攻撃対策 | §3 |
| **T**ampering | ファイル本体・メタデータの改ざん | TLS / AES-GCM 認証タグ / SHA-256 / 監査ログ | §4 |
| **R**epudiation | 自分の操作の否認 | 監査ログ INSERT-only | §6 |
| **I**nformation Disclosure | 認証情報・ファイル流出 | TLS 1.3 / 保存時暗号化 / 最小権限 IAM | §3, §4, §5 |
| **D**enial of Service | 攻撃者によるサービス不能 | レート制限 / WAF（v2）/ ALB の保護 | §7 |
| **E**levation of Privilege | 権限昇格 | アプリの権限分離 / IAM 最小権限 / DB ロール分離 | §5, §3 |

## 2. 攻撃面（Attack Surface）

```
[外部から到達可能]
   ├── ALB :443 (TLS)         ← TLS 1.3、ACM 証明書、ALB セキュリティポリシー
   ├── /share/{id}            ← 公開リンク（未認証アクセス）
   ├── /healthz, /readyz       ← ステータスエンドポイント (内部からのみアクセス可)
   └── (それ以外は認証必須)

[内部 VPC 内のみ]
   ├── ECS Fargate :8080      ← ALB からのみ受信
   ├── RDS :5432              ← ECS タスクからのみ
   ├── S3 Files (NFS)         ← VPC エンドポイント経由
   └── Secrets Manager        ← VPC エンドポイント経由

[管理面]
   ├── AWS Console            ← ハードウェア MFA 必須
   ├── ECS Exec               ← 緊急時のみ。CloudTrail で記録
   └── SSH                    ← 不採用（Bastion なし、ECS Exec で代替）
```

## 3. 認証 / セッション

### 3.1 パスワード保管

- アルゴリズム: **Argon2id**
  - メモリ: 64 MiB
  - 反復: 3
  - 並列度: 2
  - ソルト: 16 bytes ランダム
- ハッシュは `$argon2id$v=19$m=65536,t=3,p=2$<salt>$<hash>` 形式で保存
- 強度チェック: zxcvbn ベースのスコアリングで「6 文字以上 + 推測困難」を要求

### 3.2 二要素認証（TOTP）

- **必須**（個人専用とはいえ、インターネット公開のため）
- アルゴリズム: HMAC-SHA1（RFC 6238 互換、6 桁、30 秒）
- 共通秘密鍵は 20 bytes ランダム
- 共通秘密鍵は **DB 保存時に AES-GCM で暗号化** （マスタ鍵は Secrets Manager から）
- バックアップコード: 10 個。各コードは Argon2id で個別ハッシュ。1 度使ったら無効化

### 3.3 セッション管理

| 項目 | 値 |
|---|---|
| 格納場所 | サーバ側 PostgreSQL `sessions` テーブル + Cookie |
| Cookie 名 | `__Host-sync_session` |
| Cookie 属性 | `HttpOnly; Secure; SameSite=Lax; Path=/` |
| 値の構造 | `<session_id_uuid>.<HMAC-SHA256(server_key, session_id)>` |
| 有効期限 | 7 日 |
| 自動延長 | リクエストごとに `last_seen_at` 更新、24 時間経過で `expires_at` を再計算 |
| 失効条件 | (1) ログアウト (2) パスワード変更 (3) 期限切れ (4) ユーザによる「全セッション失効」 |

#### Cookie 偽造対策（HMAC 検証）

```go
type Session struct {
    ID  uuid.UUID
    SignKey []byte // Secrets Manager から
}

func (s Session) Encode() string {
    sig := hmac.New(sha256.New, s.SignKey)
    sig.Write(s.ID[:])
    return s.ID.String() + "." + base64.RawURLEncoding.EncodeToString(sig.Sum(nil))
}

func (s Session) Decode(raw string) (uuid.UUID, error) {
    parts := strings.SplitN(raw, ".", 2)
    if len(parts) != 2 { return zero, errInvalid }
    id := uuid.MustParse(parts[0])
    expected := hmacSHA256(s.SignKey, id[:])
    got, _ := base64.RawURLEncoding.DecodeString(parts[1])
    if !hmac.Equal(expected, got) { return zero, errInvalid }
    return id, nil
}
```

### 3.4 ログイン失敗時の挙動

```
入力ミスごとに失敗カウント (アカウント / IP の両方を別々にカウント)
- 失敗 5 回 / 15 分:        15 分ロック (アカウント単位)
- 失敗 30 回 / 15 分 (IP):  IP を 1 時間レート 0
- ロック中は「メールで通知」(将来)
```

### 3.5 パスワードリセット

v1 では「自分しか使わない」前提なので、Email リンクで安全にリセット：

```
[POST /password-reset]
   ├── 5 分以内に 5 回まで
   ├── トークン: 32 bytes ランダム、HMAC で署名
   ├── 有効期限: 30 分
   └── トークン消費後はそのトークン値の再利用不可 (DB で消費フラグ)
```

## 4. 暗号化（保存時 / 転送時）

### 4.1 鍵階層

```
Master Key (MK)                   ← Secrets Manager に保管。アプリ起動時に取得し、メモリに保持
   │  AES-Key-Wrap (RFC 3394)
   ▼
Key Encryption Key (KEK)          ← ユーザ単位で 1 個。RDS の users.kek_enc に暗号化保管
   │  AES-256-GCM
   ▼
Data Encryption Key (DEK)         ← ファイルバージョン単位で 1 個。file_versions.dek_enc に暗号化保管
   │  AES-256-GCM
   ▼
ファイル本体 (S3 Files 上)
```

階層の意義：
- マスタ鍵を回転（rotate）するときは KEK を再ラップするだけで済む（DEK・ファイル本体は触らない）
- 一部のユーザの侵害があっても、他ユーザの鍵に影響しない
- DEK 漏洩しても影響範囲は 1 バージョンのみ

### 4.2 アプリ層暗号化（書き込み）

```go
func encryptStream(plain io.Reader, dek [32]byte) (io.Reader, error) {
    block, _ := aes.NewCipher(dek[:])
    aead, _ := cipher.NewGCM(block)
    nonce := make([]byte, aead.NonceSize())
    rand.Read(nonce)

    // ストリーム化のため、本実装では chunked AEAD（例: 1MB ごとに認証タグ）を採用
    // 単純な aead.Seal はメモリ全量必要。Go 標準ライブラリだけでは扱いにくいので、
    // 1MB チャンク + 各チャンクに連番 nonce + 各チャンク末尾に GCM タグ、を直書きする。
    return &chunkedEncryptingReader{src: plain, key: dek, nonce: nonce}, nil
}
```

実装詳細：
- チャンクサイズ: 1 MiB
- 各チャンクの nonce: ベース nonce 12 bytes + 4 bytes チャンク連番（big-endian）
- ファイル末尾に「総チャンク数」と「終端タグ」を記録（途中で切られても改ざん検出可）

### 4.3 S3 Files 側の SSE

S3 Files の裏側 EFS は AWS マネージド KMS で暗号化。アプリ層と合わせて多重防御。

### 4.4 転送時暗号化

- ALB: TLS 1.3 必須、TLS 1.2 は ALB セキュリティポリシー `ELBSecurityPolicy-TLS13-1-2-2021-06` で許容
- ALB → ECS: ALB が TLS 終端、内部は HTTP（プライベートサブネット内）
- ECS → RDS: TLS で接続（`sslmode=require`）
- ECS → S3 Files: NFS over TLS（S3 Files の機能を有効化）
- ECS → Secrets Manager: VPC エンドポイントで HTTPS

## 5. 認可（Authorization）

### 5.1 ファイル単位

- 本システムは個人専用なので、認可は「`files.owner_id == 認証ユーザの id` のみアクセス可」が基本
- 公開リンク経由では `share_links` を通じた限定的アクセスのみ許可

### 5.2 RDS の DB ロール

- アプリは `sync_app` ロールで接続
- マイグレーションは `sync_migrate` ロールで接続（DDL のみ）
- root ロールは AWS 経由でしか触らない（普段は使わない）

### 5.3 IAM ロール

| ロール | 権限 |
|---|---|
| `ecsTaskExecutionRole` | ECR pull, CloudWatch Logs put, Secrets Manager GetSecretValue |
| `ecsTaskRole` (アプリ用) | S3 Files の特定アクセスポイントのみ、Secrets Manager の特定シークレットのみ |
| `terraformDeployRole` | アプリインフラのみ。請求や IAM 全体には触れない |

## 6. CSRF / XSS / その他 OWASP

### 6.1 CSRF

- **Double-Submit Cookie + SameSite=Lax**
- HTML レンダー時に hidden field `csrf_token`、Cookie に同じ値（HttpOnly なし）
- POST/PUT/DELETE で両者を比較
- HTMX のデフォルト `X-CSRF-Token` ヘッダ送信パスも併用

### 6.2 XSS

- HTML テンプレートはすべて Go の `html/template`（自動エスケープ）
- ユーザ入力（ファイル名・タグ・パス）は表示時に必ずテンプレートを経由
- CSP nonce 方式：

```
Content-Security-Policy:
  default-src 'self';
  script-src 'self' 'nonce-{RANDOM}';
  style-src 'self' 'unsafe-inline';
  img-src 'self' data:;
  font-src 'self';
  connect-src 'self';
  frame-src 'none';
  object-src 'none';
  base-uri 'self';
  form-action 'self';
  upgrade-insecure-requests;
```

`'unsafe-inline'` を style に許容しているのは HTMX の hx-* 属性が動的 style を要求する場面があるため。スクリプト側は nonce 必須。

### 6.3 SQLi

- 100% パラメータ化クエリ（`database/sql` の `?` または `$1` プレースホルダ）
- 文字列連結禁止
- ORM は不採用なので、誤って動的 SQL を組まないようコードレビューで担保

### 6.4 ファイルアップロードの既知の落とし穴

- `Content-Type` の信用禁止：必ずサーバ側で magic-bytes チェック（最低 256 bytes）
- ZIP / TAR 等の解凍は v1 ではしない
- アップロード時のファイル名はそのまま保存しない（NFC 正規化 + パストラバーサル拒否）
- `os.Rename` の対象先はサーバ側で組み立てた path のみ（`{file_uuid}` 固定）

### 6.5 その他のヘッダ

```
Strict-Transport-Security: max-age=31536000; includeSubDomains; preload
X-Content-Type-Options: nosniff
X-Frame-Options: DENY
Referrer-Policy: strict-origin-when-cross-origin
Permissions-Policy: camera=(), microphone=(), geolocation=()
Cross-Origin-Resource-Policy: same-origin
Cross-Origin-Opener-Policy: same-origin
Cross-Origin-Embedder-Policy: require-corp
```

## 7. レート制限・DoS 対策

### 7.1 アプリ層トークンバケット

```
ルート別の制限:
- /login                 : ユーザ単位 5 / 15min, IP 単位 30 / 15min
- /password-reset        : ユーザ単位 5 / 5min
- /uploads/*             : ユーザ単位 100 / 1min
- /files (一覧/検索)     : ユーザ単位 60 / 1min
- /share/* (公開リンク)  : IP 単位 30 / 1min
- 全体デフォルト         : ユーザ単位 1000 / 1min, IP 単位 300 / 1min
```

実装：`rate_limit_buckets` テーブルに（key, tokens, refilled_at）を持つ。ECS タスク数が増えても DB を経由するため整合的。

### 7.2 ALB のリクエスト制限

- ALB の同時接続上限を設定（v1 は AWS デフォルト）
- `X-Forwarded-For` を利用して IP レート制限を実装
- AWS Shield Standard はデフォルトで有効

### 7.3 アップロードサイズ制限

- `http.MaxBytesReader` で 1 リクエストあたり 2 GB を上限
- tus.io のチャンクは 5 MB を推奨（クライアント設定）

## 8. シークレット管理

### 8.1 Secrets Manager の構造

```json
{
  "sync-files-go/db/password": "<random>",
  "sync-files-go/aes/master-key": "<32 bytes base64>",
  "sync-files-go/totp/hmac-key": "<32 bytes base64>",
  "sync-files-go/csrf/key": "<32 bytes base64>",
  "sync-files-go/session/key": "<32 bytes base64>"
}
```

- アプリは起動時に取得、メモリに保持
- 鍵ローテーション手順は [`10-operations.md`](./10-operations.md) §7

### 8.2 鍵ローテーション

- マスタ鍵: 6 ヶ月ごとに手動ローテーション
- セッション署名鍵 / CSRF 鍵: 3 ヶ月ごと（古い鍵を 7 日間並行受け入れ）
- DB パスワード: 12 ヶ月ごと

## 9. ログ・監査

### 9.1 アプリログ

- 構造化（slog → JSON）
- 機密情報（パスワード・TOTP コード・暗号鍵・JWT・Cookie 値）は **絶対にログに出力しない**
- 各リクエストに `request_id` を付与し、エラー時に返す

### 9.2 監査ログ

- `audit_logs` テーブル（[`03`](./03-domain-model.md) §2.8）
- INSERT のみ、UPDATE / DELETE 不可（DB ロール分離で物理的に保証）
- v2: 別 AWS アカウントの S3 にエクスポートして改ざん防止

### 9.3 CloudTrail

- 全リージョン有効
- ログを `audit-logs-bucket` （別バケット、Object Lock 有効）に集約
- 重要操作（IAM 変更、シークレット読み取り）は SNS でアラート

## 10. ECS 内のセキュリティ

### 10.1 イメージ

- Distroless または scratch ベース（[`09`](./09-infrastructure-and-deployment.md) §3）
- Trivy / Grype でビルド時にスキャン（CI に組み込む）
- 脆弱性 Critical があったらビルド失敗

### 10.2 ランタイム

- ROOT ユーザでは実行しない（`USER 1000` を Dockerfile で）
- `read_only_root_filesystem: true`（タスク定義）
- `linuxParameters.capabilities.drop: ["ALL"]`
- 例外的に書き込みが必要なボリュームは `mountPoints` で個別マウント（`/var/data` のみ）

### 10.3 ECS Exec の制限

- 平時は無効
- 緊急時のみ `aws ecs update-service --enable-execute-command` で一時有効化
- 利用後は無効化、CloudTrail でレビュー

## 11. 公開リンクのセキュリティ

| 項目 | 設計 |
|---|---|
| URL | `/share/<UUID v4>` (推測困難) |
| パスワード | Argon2id で保管、検証は定数時間比較 |
| 期限 | 1h / 1d / 7d / なし。Long-lived（>7d）は推奨しない（UI で警告） |
| アクセス制限 | IP 単位 30 req/min |
| ログ | `share_link_accesses` に IP/UA を記録（90 日保持） |
| 取り消し | UI から即座に `revoked_at` 付与 |
| 元ファイル削除時 | 自動的に `revoked_at` 付与 |
| HSTS | 本サービス全体で有効 |

## 12. プライバシー・データ最小化

- アクセスログの IP は 90 日後に切り捨て（最後のオクテットを 0 に）
- 監査ログの `user_agent` は同様に 90 日後に粗化
- 個人専用なので「他人の個人情報を持たない」が原則

## 13. インシデント対応

### 13.1 検知

- ECS 内で 5xx 率 > 1% / 5min → SNS アラート
- ログイン失敗 > 100 / 1min → SNS アラート
- S3 Files / RDS 接続失敗 → SNS アラート
- CloudTrail 上の異常（IAM 変更、root 操作）→ SNS アラート

### 13.2 応答

[`10-operations.md`](./10-operations.md) §6 の Runbook 参照。

### 13.3 通信

- インシデント時の通信先: 自分のメール + SMS（SNS）
- 公開ステータスページは不要（個人）

## 14. ペネトレーションテスト

- v1 リリース時に OWASP ZAP の自動スキャンを実行
- 主要な OWASP Top 10 を CI に組み込む（gosec, staticcheck, govulncheck）
- 手動ペネトレーションは v2 で（コスト対効果による）

---

次の章: [`08-frontend-design.md`](./08-frontend-design.md)
