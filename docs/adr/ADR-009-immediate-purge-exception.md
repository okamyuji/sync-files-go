# ADR-009: 即時物理削除（INV-1 の例外）の運用 CLI のみ提供

## ステータス

採択 (2026-04-29)。

## コンテキスト

- INV-1 は「active からの即時物理削除を禁止し、trashed → 30 日経過 or 明示 purge を必須とする」と定める
- しかし個人情報保護法・GDPR・プライバシー文脈で「即時に痕跡を残さず削除したい」要求が現実にはあり得る（個人専用とはいえ、自分自身が「今すぐ消したい」場面）
- これは UI 通常フローには出さないが、運用 CLI として提供する必要がある
- `01-requirements.md §N-10.2` および `06-data-loss-prevention.md §E-1` で言及があるが、INV-1 との関係を ADR で明示する

## 決定

**INV-1 の例外として、運用 CLI `sync-files-admin force-purge` を限定的に提供する**。

利用条件：

1. **UI からは到達不能**（http ルートを切らない）。実行は ECS Exec 経由でのみ
2. 実行時は **2 重認証**：操作者は (a) AWS コンソールで ECS Exec を有効化、(b) CLI 起動時にユーザのパスワードを再入力
3. 監査ログには `actor_kind='system'` `action='file.force_purge'` `irreversible=true` で必ず記録
4. 対象を限定（特定 file_id のみ、全件一括は禁止）
5. 実行前に「この操作は復旧不能。S3 バケットバージョニングからも消えます」のテキスト確認

CLI 実装：

```
sync-files-admin force-purge --user <id> --file <id> [--include-versions all]
  -- INV-1 の例外。UI からは到達不能。
  -- 1. files をハード DELETE
  -- 2. file_versions を全件ハード DELETE
  -- 3. S3 上の versions/{file_uuid}/* を s3api delete-object --version-id で完全消去
     （バケットバージョニングからも消える）
  -- 4. audit_logs に irreversible=true で記録
```

## 帰結

- INV-1 の例外として ADR-009 を発行（このドキュメント）
- 設計書全体としては「INV-1 は守る。force-purge は明示の例外で運用ツールのみ」と一貫させる
- v1 では実装可能（CLI のみ）。テスト戦略 §6 のアンチパターンテストには「http ルートとして force-purge が公開されていないこと」を追加
- v2 で「ユーザ自身が UI から force-purge を実行する必要があるか」を再検討（今のところ不要）

## リンク

- [`01-requirements.md`](../01-requirements.md) §N-10.2
- [`06-data-loss-prevention.md`](../06-data-loss-prevention.md) §E-1
- [`04-sync-semantics.md`](../04-sync-semantics.md) INV-1
- [`10-operations.md`](../10-operations.md) §4.3（運用 CLI）
