# 07. セキュリティ

> インターネット公開かつ個人専用という制約の下で、現実的に守るべき脅威とその対策を MECE に整理する。

## 1. 脅威モデル（STRIDE）

| カテゴリ | 脅威 | 主な対策 | 節 |
|---|---|---|---|
| **S**poofing | 他人が自分のアカウントになりすます | パスワード + TOTP 2FA / セッション固定攻撃対策 | §3 |
| **T**ampering | ファイル本体・メタデータの改ざん | TLS / AES-GCM 認証タグ / SHA-256 / 監査ログ | §4 |
| **R**epudiation | 自分の操作の否認 | 監査ログ INSERT-only | §6 |
| **I**nformation Disclosure | 認証情報・ファイル流出 | TLS 1.3 / 保存時暗号化 / 最小権限 IAM | §3, §4, §5 |
| **D**enial of Service | 攻撃者によるサービス不能 | Cloudflare の DDoS 緩和 / nginx + アプリのレート制限 | §7 |
| **E**levation of Privilege | 権限昇格 | アプリの権限分離 / IAM 最小権限 / DB ロール分離 | §5, §3 |

## 2. 攻撃面（Attack Surface）

```
[外部から到達可能]
   ├── Cloudflare Edge :443    ← TLS 1.3 終端、Cloudflare DDoS 緩和
   │     └ Tunnel経由 → cloudflared (outbound接続のみ)
   │       └ nginx :8443 (TLS 再終端) → app :8080
   ├── /share/{id}             ← 公開リンク（未認証アクセス、Cloudflare 経由）
   └── (それ以外は認証必須)

[内部 VPC 内のみ]
   ├── ECS Fargate :8080      ← nginx サイドカーからのみ（同一タスク localhost）
   ├── RDS :3306              ← ECS タスクからのみ
   ├── S3 Files (NFS :2049)   ← VPC エンドポイント経由
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
| 格納場所 | サーバ側 MySQL Primary `sessions` テーブル + Cookie |
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

### 3.5 パスワードリセット（MEDIUM 修正：v1 はリカバリコードのみ）

Email 送信基盤（SES / SMTP / 配信ドメイン / DKIM / DMARC・配送失敗の運用）を v1 では立てない判断とし、パスワードリセットは **発行済みリカバリコード（10 個）でのみ** 実行する。

```
[ログイン画面]
   └─ "リカバリコードを使う"
        └─ /recovery
             ├── 5 分以内に 5 回まで
             ├── 入力されたコードを Argon2id で検証
             ├── 一致したら新パスワード設定画面へ
             └── 使用済みコードは即座に無効化（残り 9 個）
```

リカバリコードを 10 個すべて使い切った／紛失した場合、復旧手段は AWS コンソールから RDS にアクセスして手動でパスワードハッシュをリセットする運用 Runbook（[`10-operations.md`](./10-operations.md)）。

Email リセットは v2 候補（SES の本番ドメイン認証完了後）。

## 4. 暗号化（保存時 / 転送時）

### 4.0 二段 TLS

転送時暗号化は 2 段：

- **ユーザブラウザ ↔ Cloudflare Edge**：TLS 1.3（Cloudflare 管理、Let's Encrypt 互換または Cloudflare Universal）
- **Cloudflare ↔ cloudflared (ECS タスク内)**：Cloudflare Tunnel の暗号化トンネル（QUIC ベース）
- **cloudflared ↔ nginx**：localhost ループバック内（同一タスク内）。自己署名 TLS で保護
- **nginx ↔ app**：同上の localhost ループバック（HTTP）
- **app ↔ RDS MySQL**：`tls=true` で TLS 必須
- **app ↔ S3 Files (NFS)**：S3 Files の TLS 機能を有効化
- **app ↔ Secrets Manager**：VPC エンドポイント経由 HTTPS

### 4.1 鍵階層

```
Master Key (MK, 32 bytes)         ← Secrets Manager に保管 (master_key_version 付き)、起動時にメモリへ
   │  AES-Key-Wrap (RFC 3394)
   ▼
Key Encryption Key (KEK, 32 bytes) ← users.kek_enc に保管。kek_id_bin / master_key_version 同行
   │  AES-Key-Wrap (RFC 3394)
   ▼
Data Encryption Key (DEK, 32 bytes) ← file_versions.dek_enc に保管。kek_id_bin 同行
   │  AEAD ストリーム暗号化 (CR-4: 標準ライブラリ直書きを廃止し、検証済みプリミティブを使用)
   ▼
ファイル本体 (S3 Files 上)
```

階層の意義：
- Master Key を回転するときは KEK を再ラップするだけで済む（DEK・ファイル本体は触らない）
- 一部のユーザが侵害されても、他ユーザの KEK・DEK に影響しない
- DEK 漏洩しても影響範囲は 1 バージョンのみ

DB スキーマでの表現は [`03-domain-model.md`](./03-domain-model.md) §4 の `users.kek_enc` / `file_versions.dek_enc` / `file_versions.encryption_scheme` / `file_versions.encryption_header` を参照。

### 4.2 アプリ層暗号化（CR-4 修正：自前 nonce 戦略を廃止）

> **重要**: 設計初版では「12 bytes ベース nonce + 4 bytes チャンク連番」を自前で組み立てていたが、これは GCM の標準 nonce 長と矛盾し、実装ミスで nonce 再利用が起きた瞬間に AEAD が破綻する。本設計では **検証済みのストリーム AEAD プリミティブ**を採用し、自前の nonce 構成は行わない。

採用候補（実装時に確定。`encryption_scheme` 列に記録）：

| スキーム | ライブラリ | 標準ライブラリ縛りからの逸脱 |
|---|---|---|
| **Tink Streaming AEAD (AES-256-GCM-HKDF-1MB)** ← 第一候補 | `github.com/tink-crypto/tink-go/v2` | ◯（追加依存だが、Google 製の堅牢な実装） |
| age (X25519 / ChaCha20-Poly1305 ベース) | `filippo.io/age` | ◯（小さく依存も少ない） |
| libsodium SecretStream | `github.com/jamesruan/sodium` | ◯（C ライブラリ依存、Distroless では工夫が要る） |

「Go 標準ライブラリ中心」という非機能要件はこの一点で緩める：データ暗号化の正しさは個人実装より検証済みライブラリに任せる方が安全（CR-4 の要点）。詳細は [ADR-005](./adr/ADR-005-server-side-encryption-aes-gcm.md)。

呼び出しイメージ（Tink を採った場合）：

```go
import (
    "github.com/tink-crypto/tink-go/v2/aead/streamingaead"
    "github.com/tink-crypto/tink-go/v2/keyset"
)

func encryptStream(dst io.WriteCloser, src io.Reader, dek []byte, aad []byte) error {
    handle, err := keyset.NewHandleFromRawKey(dek, "AES256_GCM_HKDF_1MB")
    if err != nil { return err }
    saead, err := streamingaead.New(handle)
    if err != nil { return err }

    encWriter, err := saead.NewEncryptingWriter(dst, aad)
    if err != nil { return err }
    defer encWriter.Close()

    _, err = io.Copy(encWriter, src)
    return err
}
```

- AAD には `file_id_bin || version_number || owner_id_bin` を入れ、別ファイルとの取り違えを防止する
- Tink の Streaming AEAD は内部で：1MB セグメント、各セグメントに固有 nonce（HKDF 派生）、各セグメントに認証タグ、最終セグメントに終端マーカー
- 途中で切断されたファイルは復号時に検出可能（先頭から順に GMAC を検証するため）
- nonce 再利用やランダム値の管理は Tink が責任を持つ

### 4.3 S3 Files 側の SSE

S3 Files の裏側 EFS は AWS マネージド KMS で暗号化。アプリ層と合わせて多重防御。

### 4.4 転送時暗号化（再掲）

§4.0 で扱った二段 TLS と内部経路の暗号化を実装する。MySQL 接続は go-sql-driver/mysql の `tls=true`（または `tls=preferred` で開発環境）。

## 5. 認可（Authorization）

### 5.1 ファイル単位

- 本システムは個人専用なので、認可は「`files.owner_id == 認証ユーザの id` のみアクセス可」が基本
- 公開リンク経由では `share_links` を通じた限定的アクセスのみ許可

### 5.2 RDS の DB ロール（MySQL）

- アプリは `'sync_app'@'%'` で接続。`SELECT/INSERT/UPDATE/DELETE` のみ。`audit_logs` は `INSERT/SELECT` のみ
- マイグレーションは `'sync_migrate'@'%'` で接続（DDL のみ）
- `rdsadmin` は AWS 経由でしか触らない（普段は使わない）
- Read Replica は MySQL の `--read-only` モードで動作するため、誤って Reader ハンドルから書き込みが流れても DB 側で拒否される（保険）

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

### 7.2 nginx と Cloudflare のレート制限

- Cloudflare 側：標準 DDoS 緩和 + Free プランでも基本のレート制限可
- nginx 側：`limit_req_zone $http_cf_connecting_ip zone=ip:10m rate=300r/m;`（Cloudflare の `CF-Connecting-IP` ヘッダを真の IP として信頼）
- アプリ側：DB 由来のトークンバケット（前述 §7.1）。3 層で防御

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
  "sync-files-go/session/key": "<32 bytes base64>",
  "sync-files-go/cloudflared/token": "<Cloudflare Tunnel auth token>"
}
```

- アプリは起動時に取得、メモリに保持
- 鍵ローテーション手順は [`10-operations.md`](./10-operations.md) §7

### 8.2 鍵ローテーション（一覧）

| 鍵 | 周期 | 並行受入期間 |
|---|---|---|
| マスタ鍵（KEK 暗号化用） | 6 ヶ月 | KEK 再ラップ完了まで |
| セッション署名鍵 / CSRF 鍵 | 3 ヶ月 | 7 日間並行 |
| DB パスワード | 12 ヶ月 | 0（短時間ダウンを許容） |
| Cloudflare Tunnel トークン | 12 ヶ月 / 異常時 | 即時切替 |
| TOTP HMAC 鍵 | 12 ヶ月 | 旧鍵で復号 → 新鍵で再暗号化バッチ |

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
