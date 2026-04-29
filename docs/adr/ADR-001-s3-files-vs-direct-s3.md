# ADR-001: ストレージ層は S3 Files (NFS) を採用、S3 SDK 直接利用は最小化

## ステータス

採択 (2026-04-29)

## コンテキスト

ファイル本体の保存先として、AWS の S3 系ストレージを 3 通りの使い方で検討した：

1. **S3 SDK 直接**：`PutObject` / `GetObject` / マルチパート / 署名付き URL を全面使用
2. **S3 Files (NFS マウント)**：ECS タスクからファイルシステムとしてマウント、`os` パッケージで read/write
3. **CAS 型自前実装**：Content-Defined Chunking + ハッシュキー命名（Dropbox 流）

## 検討した選択肢

### 選択肢 A: S3 SDK 直接

利点：
- 細かい制御（マルチパート / 署名付き URL）
- マウント不要、ECS 側の依存最小

欠点：
- Go コードが SDK 専用になり、ローカル開発で MinIO 互換が必要
- 「標準ライブラリ中心」の方針からずれる
- ストリーム操作（特に大容量）が SDK 越しでやや煩雑

### 選択肢 B: S3 Files (NFS マウント) ← **採択**

利点：
- Go の `os` / `io` パッケージだけで完結（標準ライブラリ志向に合う）
- ECS タスクの `s3files_volume_configuration` で宣言的にマウント可
- 内部は EFS バックエンドで~1ms レイテンシ、POSIX 互換、ファイルロック対応
- 同一 S3 バケットを直接 SDK でも触れるので、運用ツール（CLI 復元等）は SDK で実装可能

欠点：
- Terraform AWS provider 側で `aws_s3files_file_system` リソースが 2026-04 時点で開発中（PR pending）→ 一部 CLI 手動が必要
- 課金モデル（書き込み + 同期リクエスト）を理解する必要がある
- NFS の特性（fsync の弱さ、open 中削除）を考慮した実装が必要

### 選択肢 C: CAS 型自前実装

利点：
- 重複排除・差分転送が効率的（バイナリ大容量で帯域節約）

欠点：
- 実装コストが大きい（CDC・チャンクストア・参照カウンタ）
- v1 の規模（500GB / 2GB 上限）では費用対効果が低い
- 障害解析が複雑になる

## 決定

**選択肢 B (S3 Files NFS マウント)** を採択する。S3 SDK 直接利用は次の場面に限定する：

- 運用 CLI（旧版復元、孤児クリーンアップ）
- バックエンドでのライフサイクル設定確認
- バッチ処理のうち、S3 API でないと表現しにくいもの

## 帰結

- 「サイズが大きいファイルのアップロード時、S3 マルチパート + 署名付き URL でブラウザから直接 S3 へ」という最適化は v1 では採用しない（NG-7 に記載）。アップロードはサーバ経由 + tus.io レジューム
- ECS タスクのマウント要件として S3 Files Access Point の作成手順を運用 Runbook に記載
- ローカル開発では `hostPath` ボリュームで POSIX FS を模擬。MinIO は使わない

## リンク

- [S3 Files 公式発表](https://aws.amazon.com/blogs/aws/launching-s3-files-making-s3-buckets-accessible-as-file-systems/)
- [`02-architecture.md`](../02-architecture.md) §3.5
- [`03-domain-model.md`](../03-domain-model.md) §5
