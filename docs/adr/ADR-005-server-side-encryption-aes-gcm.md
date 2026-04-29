# ADR-005: 保存時暗号化はアプリ層 Streaming AEAD + S3 SSE の多重防御（自前 nonce は不採用）

## ステータス

採択 (2026-04-29)。自己レビュー指摘により、実装プリミティブを「自前 nonce + GCM」から「検証済み Streaming AEAD ライブラリ」に変更。

## コンテキスト

VPS / クラウドにデプロイし外部公開する個人専用システムにおいて、ファイル本体の暗号化を：
- どの層で
- 誰が鍵を管理するか
を決める必要がある。

## 検討した選択肢

### A. クライアント側 E2E 暗号化（WebCrypto）

ブラウザで暗号化してからアップロード。サーバは暗号文しか見ない。

欠点：
- HTMX 主体（NG-3）の方針に対し、ブラウザ JS が必須化
- バックアップ・サムネイル・プレビューがすべて困難
- 鍵紛失で復旧不能（個人用には危険）

### B. アプリ層暗号化 + S3 SSE ← **採択**

ECS Fargate のアプリが書き込み時に AES-256-GCM で暗号化、復号して返却。さらに S3 側でも SSE-S3。

利点：
- HTMX 主体の UI と整合
- Cloudflare / VPS オペレータ / S3 ストレージ事業者に中身が見えない
- プレビュー・サムネイル等の派生処理がサーバ内で可能
- 鍵階層（マスタ → KEK → DEK）でローテーションが容易

欠点：
- ECS タスクが侵害されたら復号鍵もメモリに乗っているので破られる（→ IAM 最小権限・Secrets Manager・タスク隔離で対策）

### C. S3 SSE-S3 のみ

利点：実装最小

欠点：
- AWS S3 / 内部運用者には平文と同等に見える
- 「ファイル損失防止」と直結はしないが、流出時の被害は大きい

### D. S3 SSE-KMS + CMK + キーポリシー

利点：AWS の KMS に鍵管理を委ねる。鍵ローテーション AWS 任せ

欠点：個人用には KMS のコストが過大、IAM 設計が複雑化

## 決定

**選択肢 B（アプリ層 AES-256-GCM + S3 SSE-S3）** を採択。

鍵階層：
```
Master Key (Secrets Manager)
   │ AES-Key-Wrap
   ▼
Key Encryption Key (per user, RDS に暗号化保管)
   │ AES-256-GCM
   ▼
Data Encryption Key (per file version)
   │ AES-256-GCM (チャンク化)
   ▼
ファイル本体 (S3 Files)
```

実装方針：
- ストリーム AEAD は **Tink Streaming AEAD (AES-256-GCM-HKDF-1MB)** を第一候補とする。age / libsodium SecretStream も代替として ADR の対象範囲に含める
- 「自前で 12 bytes ベース nonce + 4 bytes チャンク連番を組み立てる」設計は不採用（GCM の安全性を破壊しうる）
- DEK は `file_versions.dek_enc` として保管（KEK で AES-Key-Wrap）
- KEK は `users.kek_enc` として保管（Master Key で AES-Key-Wrap）
- 暗号化スキームは `file_versions.encryption_scheme` に記録（将来の移行のため）
- AAD には `file_id_bin || version_number || owner_id_bin` を含めて取り違え防止
- Master Key ローテーション時は KEK のみ再ラップ（DEK・本体は触らない）

## 帰結

- 07-security.md §4 に詳細
- 鍵ローテーション手順は 10-operations.md §6
- 復号失敗（GCM 認証タグ不一致）はアラート対象とし、CloudWatch にメトリクス化

## v1 実装の選択（2026-04 update）

Tink Streaming AEAD を第一候補としたが、Distroless arm64 でのビルド可否を実機検証する前に v1 実装を進める都合上、**v1 では以下の chunked AEAD を `internal/crypto/aead.go` で標準ライブラリ実装する**：

- 暗号: `crypto/aes` + `crypto/cipher` (AES-256-GCM)
- フレーム: `[length:4][ciphertext+tag]` の繰り返し
- nonce 構造: **8 bytes random base + 4 bytes big-endian counter**（合計 12 bytes、GCM 標準長と整合）
- 最終チャンクには AAD に `"last:"` プレフィックスを付与（途中切断検出）
- chunk size: 1 MiB
- 暗号化スキーム名: `aead-aes256-gcm-chunked-1mb-v1`（`encryption_scheme` 列に保存）

この実装は ADR-005 が禁じる「自前で 12 bytes 全体を組み立てて nonce 再利用を起こす設計」**ではない**。8 bytes ランダム base が ファイルバージョン単位で固有、4 bytes counter がチャンク単位で固有なので、同じファイル内で nonce が再利用されないことが構造的に保証される。異なるファイル間も base の衝突確率は 2^-64。

将来 Tink Streaming AEAD への移行を行う場合、`encryption_scheme` 列の値で旧版を識別して並行復号できる。スキームの追加は SQL マイグレーション不要。

## リンク

- [`07-security.md`](../07-security.md) §4
- [`10-operations.md`](../10-operations.md) §6
