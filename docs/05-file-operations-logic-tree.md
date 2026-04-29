# 05. ファイル操作の MECE ロジックツリー

> 各操作について、起こりうる分岐をすべて列挙する。**「想定外の状態」が決して残らない** ように。

## 0. 共通ガード

すべてのファイル操作リクエストは、入口で次を必ず通過する：

```
[Request]
   │
   ▼
[1] 認証ミドルウェア (Cookie 検証 + セッション照合)
   │
   ▼
[2] CSRF ミドルウェア (POST/PUT/DELETE は double-submit cookie 検証)
   │
   ▼
[3] レート制限ミドルウェア (ユーザ + IP)
   │
   ▼
[4] パス正規化 (Unicode NFC + パストラバーサル拒否 + 長さ制限)
   │
   ▼
[5] 認可チェック (ファイルの owner_id == 認証ユーザ)
   │
   ▼
[操作固有のロジック]
```

これらが 1 つでも失敗したら、それ以降は実行しない。

## 1. アップロード（CREATE / OVERWRITE）

```
[POST /files            -- 新規 (X-File-Path ヘッダで path 指定、If-None-Match: *)
 or PUT /files/{id}      -- 既存上書き (If-Match: <version_id>)]
        │
        ▼
[共通ガード]  ← 失敗時 401/403/429
        │
        ▼
[Precondition ヘッダの解析]
   ├── ヘッダなし
   │     └─→ 428 Precondition Required
   ├── If-None-Match: *      (新規作成の意図)
   │     └─→ §1.A へ
   ├── If-Match: <ver_id>    (上書きの意図)
   │     └─→ §1.B へ
   └── If-Match: *           (強制上書き = 確認モーダル経由のみ)
         └─→ §1.C へ

§1.A 新規作成
   ├── 同名ファイルが active で存在?
   │     ├── YES → 412 Precondition Failed (file exists)
   │     └── NO  → §1.W (実書き込み)
   └── 同名が trashed?
         └── 別のレコードとして新規作成 OK (path UNIQUE は active のみ実質的に必要)

§1.B 上書き (OCC ターゲット指定)
   ├── 対象ファイル存在しない?
   │     └─→ 412 Precondition Failed (no such file)
   ├── 対象が trashed/purged?
   │     └─→ 410 Gone (まずゴミ箱から復元してください)
   ├── current_version_id != If-Match の値?
   │     └─→ 409 Conflict (§04-sync-semantics.md 4.3 のレスポンス)
   └── 一致 → §1.W

§1.C 強制上書き
   ├── 対象が trashed/purged → 410 Gone
   ├── 対象が active        → §1.W (旧版は file_versions / S3 バージョニングで保持)
   └── 対象なし             → 412

§1.W 実書き込み (write phase, immutable versions key)
   ├── version_uuid := uuid.New()
   ├── tmp/{upload_uuid}.part にストリーム書き込み
   │     ├── サイズ超過 (> 2GB) → 413 Payload Too Large
   │     ├── ディスク満杯       → 507 Insufficient Storage
   │     └── 通信切断           → tmp は残置 (7 日 TTL で掃除)
   ├── SHA-256 を計算しながら書き込み (ストリーム)
   ├── 各 AES-GCM チャンクの認証タグ + 末尾の終端タグを検証
   ├── fsync (best-effort)
   ├── 第二 OCC ロック取得 (現行 current_version_id_bin を再確認)
   │     └── ここでも不一致なら 409 (実書き込み中に別端末が更新したケース)
   ├── os.Rename(tmp/{upload_uuid}.part → versions/{file_uuid}/{version_uuid})
   │     ↑ versions 配下は新規キーなので上書きしない、CR-1 修正
   │     └── 失敗時 (まれ): tmp を残し、500 を返す。補正ジョブで処理
   ├── BEGIN
   │     INSERT file_versions (id_bin=version_uuid, storage_key=versions/.../version_uuid, ...)
   │     UPDATE files SET current_version_id_bin = version_uuid, updated_at = NOW(6), sha256, size_bytes ...
   │     INSERT audit_logs ...
   │   COMMIT
   │     └── COMMIT 失敗時: versions/{file_uuid}/{version_uuid} は無参照のまま残る
   │          files.current_version_id_bin は旧版のまま → ユーザ影響ゼロ
   │          補正ジョブが「file_versions に対応行がない versions/*/* キー」を検出 → /_orphan
   ├── Set-Cookie で raw_until=now+5s を発行 (HIGH 修正、後続リクエストの RAW window)
   └── 204 No Content / 201 Created を返す
       ETag: <new_version_id>
       X-File-Version: <number>
```

## 2. ダウンロード / プレビュー

```
[GET /files/{id}]   (認証済みユーザの場合)
   │
   ▼
[共通ガード] ← 失敗時 401/403/429
   │
   ▼
SELECT files WHERE id = $1 AND owner_id = $2 AND state = 'active'
   ├── レコードなし → 404
   ├── state = 'trashed' → 410 Gone (UI: 「ゴミ箱にあります」リンク)
   ├── state = 'purged' or 'gone' → 410 Gone (復元不可)
   └── state = 'active' →
         │
         ▼
   [If-None-Match ヘッダがあれば]
       ├── current_version_id と一致 → 304 Not Modified
       └── 不一致 → 続行
         │
         ▼
   SELECT file_versions WHERE id_bin = files.current_version_id_bin
   open(/var/data/owner-X/versions/{file_uuid}/{version_uuid})
       ├── ENOENT → 500 + アラート (メタとストレージの不整合)
       └── OK →
         │
         ▼
   ストリーム復号 (AES-GCM) → io.Copy(w, decryptedReader)
       └── 途中失敗 → 部分応答が送出済みのため、接続を切るしかない (Trailer ヘッダで通知も検討)
```

### 2.1 公開リンク経由のダウンロード

```
[GET /share/{token}]   (未認証、token は base64url 32 bytes ランダム、サーバ側で SHA-256(token) を share_links.token_hash と照合)
   │
   ▼
[レート制限: IP 単位 30 req/min]
   │
   ▼
SELECT * FROM share_links WHERE id = $1
   ├── なし                          → 404
   ├── revoked_at IS NOT NULL        → 410 Gone
   ├── expires_at < now()            → 410 Gone (UI に「期限切れ」)
   ├── password_hash IS NOT NULL かつ未認証 → /share/{token}/password ページへ 302
   └── アクセス可 →
         │
         ▼
   SELECT files WHERE id = (share_link.file_id)
       ├── deleted_at IS NOT NULL → 410 Gone (auto-revoke 推奨)
       └── active → §2 のダウンロード処理を流用
                    + share_link_accesses への INSERT
                    + view_count / download_count を増分
```

## 3. リネーム / 移動

```
[PATCH /files/{id}]
   body: { "path": "/Reports/Q2 Final.docx" }
   header: If-Match: <version_id>
   │
   ▼
[共通ガード]
   │
   ▼
[新パスの正規化]
   ├── 不正な文字 / 長すぎる → 400
   ├── 移動先フォルダがない  → 400 (フォルダは別途作成しないと使えない)
   └── 移動先に同名 active   → 409 Conflict (UI で別名 / 強制上書き 選択)
   │
   ▼
SELECT files FOR UPDATE WHERE id = $1
   ├── current_version_id != If-Match → 409 Conflict (rename も OCC)
   └── OK →
         BEGIN
           UPDATE files SET path=..., parent_folder_id=..., name=..., updated_at=now()
           INSERT audit_logs (action='file.rename', from, to)
         COMMIT
   │
   ▼
S3 Files 上のキーは変更しない (UUID 固定)
```

## 4. ソフト削除

```
[DELETE /files/{id}]
   │
   ▼
[共通ガード]
   │
   ▼
[UI 上の確認モーダル] ← INV-5 (これはフロント側の責務)
   │
   ▼
SELECT files FOR UPDATE WHERE id = $1
   ├── すでに trashed → 200 OK (冪等)
   ├── purged / gone   → 410 Gone
   └── active →
         │
         ▼
   BEGIN
     UPDATE files SET state='trashed', deleted_at=now(), updated_at=now()
     -- 関連の active な share_link を auto-revoke
     UPDATE share_links SET revoked_at=now()
       WHERE file_id=$1 AND revoked_at IS NULL
     INSERT audit_logs (action='file.delete', target_id=$1)
   COMMIT
   │
   ▼
S3 Files 上のオブジェクトには手をつけない (INV-1)
```

### 4.1 フォルダのソフト削除

```
[DELETE /folders/{id}]
   │
   ▼
[共通ガード]
   │
   ▼
WITH RECURSIVE
  descendants AS (
    SELECT id FROM folders WHERE id = $1
    UNION ALL
    SELECT f.id FROM folders f JOIN descendants d ON f.parent_folder_id = d.id
  )
UPDATE folders SET deleted_at=now() WHERE id IN (SELECT id FROM descendants);
UPDATE files   SET state='trashed', deleted_at=now()
  WHERE parent_folder_id IN (SELECT id FROM descendants) AND state='active';
INSERT audit_logs (action='folder.delete', target_id=$1, details_json=...);
```

サブツリー全体のサイズが大きい場合（>1000 ファイル）は、バックグラウンドジョブに委譲し、UI には「処理中」を表示する（v2 候補）。v1 では同期処理。

## 5. 復元（ゴミ箱から戻す）

```
[POST /files/{id}/restore]
   │
   ▼
[共通ガード]
   │
   ▼
SELECT files FOR UPDATE WHERE id = $1
   ├── すでに active → 200 OK (冪等)
   ├── purged / gone → 410 Gone
   └── trashed →
         │
         ▼
   元の path に同名 active がある?
       ├── YES → 別名で復元
       │         path = path + '(restored YYYY-MM-DD HH-MM)'
       │         INSERT audit_logs (action='file.restore_with_rename')
       └── NO  → そのまま戻す
                  UPDATE files SET state='active', deleted_at=NULL
                  INSERT audit_logs (action='file.restore')
```

## 6. 物理削除（明示操作）

```
[POST /files/{id}/purge]   (明示操作のみ。一覧の「ゴミ箱を空にする」も同じ)
   │
   ▼
[共通ガード]
   │
   ▼
[UI 確認モーダル: 「この操作は元に戻せません」 + パスワード再入力]   ← INV-5
   │
   ▼
SELECT files FOR UPDATE WHERE id = $1
   ├── state != 'trashed' → 400 (ゴミ箱にないものは purge できない)
   └── trashed →
         │
         ▼
   for each fv in file_versions WHERE file_id_bin = $1:
     os.Remove(/var/data/owner-X/versions/{file_uuid}/{fv.id_bin})  -- S3 DeleteMarker 付与
   UPDATE files SET state='purged', updated_at=NOW(6) WHERE id_bin=$1
   INSERT audit_logs (action='file.purge', irreversible=true)
```

## 7. 物理削除（バッチ）と旧版 prune

### 7.1 ゴミ箱の物理削除（30 日経過）

```
[ECS Scheduled Task: every day 03:00 JST]
   │
   ▼
SELECT id_bin AS file_id_bin, owner_id_bin
  FROM files
 WHERE state = 'trashed'
   AND deleted_at < NOW() - INTERVAL 30 DAY
 ORDER BY deleted_at ASC
 LIMIT 1000;
   │
   ▼
for each row:
   try:
     for each fv in file_versions WHERE file_id_bin = row.id_bin:
       os.Remove(/var/data/owner-X/versions/{row.id_bin}/{fv.id_bin})
     UPDATE files SET state='purged' WHERE id_bin=row.id_bin
     INSERT audit_logs (action='file.purge_by_retention', irreversible=true)
   except err:
     log.error
     continue   -- 1 件の失敗で全体を止めない
```

### 7.2 旧版 prune（90 日経過、CR-5 新規対応）

immutable key 設計では S3 の `noncurrent_version_expiration` が機能しないため、アプリ層で明示的に古いバージョンを削除する：

```
[ECS Scheduled Task: every day 04:00 JST]
   │
   ▼
SELECT fv.id_bin AS version_id_bin, fv.file_id_bin, f.owner_id_bin
  FROM file_versions fv
  JOIN files f ON f.id_bin = fv.file_id_bin
 WHERE fv.created_at < NOW() - INTERVAL 90 DAY
   AND fv.id_bin <> COALESCE(f.current_version_id_bin, X'00000000000000000000000000000000')
   AND fv.deleted_by_user = 0
 ORDER BY fv.created_at ASC
 LIMIT 1000;
   │
   ▼
for each fv:
   try:
     os.Remove(/var/data/owner-X/versions/{fv.file_id_bin}/{fv.id_bin})
     DELETE FROM file_versions WHERE id_bin = fv.id_bin
     INSERT audit_logs (action='file_version.prune_by_age', irreversible=true,
                         details_json={ file_id, version_id, age_days })
   except err:
     log.error
     continue
```

設計ポイント：
- `current_version_id_bin` のものは絶対に消さない（最新版は age に関係なく残す）
- `deleted_by_user = 0` のものだけ自動 prune（ユーザが「この版だけ残す」と明示した版は対象外。F-5.3）
- このバッチが落ちると旧版が無限に蓄積する → CloudWatch アラートで「直近 24h で prune 実行 0」を検知

### 7.3 「最大 100 版」上限の維持

書き込み時、新版 INSERT 後に「自分のファイルの version_number 最古から 100 版を超える分」を即時 prune：

```
DELETE fv FROM file_versions fv
 WHERE fv.file_id_bin = ?
   AND fv.id_bin <> ?  -- 新しく作った current
   AND fv.deleted_by_user = 0
   AND fv.version_number <= (
      SELECT MAX(version_number) - 100 FROM file_versions WHERE file_id_bin = ?
   );
-- 対応する S3 オブジェクトもアプリで os.Remove
```

`v2`：物理削除済みファイル全体を `state='gone'` に更新するバッチも実装（90 日後の完全消去確認）。

## 8. 共有リンク作成

```
[POST /files/{id}/share-links]
   body: { "expires_in": "7d", "password": "<optional>" }
   │
   ▼
[共通ガード]
   │
   ▼
SELECT files WHERE id=$1 AND state='active'
   └── なし → 404
   │
   ▼
expires_at の妥当性検証
   ├── 1 hour | 1 day | 7 days | none のいずれか
   └── 任意の DateTime は受け付けない (列挙のみ)
   │
   ▼
password が指定されている場合 Argon2id でハッシュ
   │
   ▼
INSERT INTO share_links (id_bin=UUID_TO_BIN(UUID()), file_id_bin, token_hash, expires_at, ...)
INSERT audit_logs (action='share.create', target_id=share_link.id)
   │
   ▼
return JSON: { "url": "https://example.com/share/{token}", ... }
```

### 8.1 共有リンクの取り消し

```
[DELETE /share-links/{id}]
   │
   ▼
[共通ガード]
   │
   ▼
UPDATE share_links SET revoked_at=now() WHERE id=$1 AND created_by=session.user_id
INSERT audit_logs (action='share.revoke')
```

## 9. ファイル名・タグ検索

```
[GET /search?q=...]
   │
   ▼
[共通ガード] (レート制限: 60 req/min)
   │
   ▼
[クエリ正規化: NFC, trim, 最低 1 文字]
   │
   ▼
-- MySQL 8 FULLTEXT (parser=ngram) を使用 (HIGH 修正)
SELECT id_bin, path, name, updated_at, size_bytes
  FROM files
 WHERE owner_id_bin = ?
   AND state = 'active'
   AND (
     MATCH(name) AGAINST (? IN BOOLEAN MODE)                        -- ファイル名 ngram 検索
     OR EXISTS (
        SELECT 1 FROM file_tags ft
          JOIN tags t ON t.id_bin = ft.tag_id_bin
         WHERE ft.file_id_bin = files.id_bin
           AND t.owner_id_bin = files.owner_id_bin
           AND t.name LIKE CONCAT(?, '%')                            -- タグ前方一致
     )
   )
 ORDER BY updated_at DESC
 LIMIT 50;
   │
   ▼
return HTML 部分テンプレート (HTMX swap)
```

## 10. アクティビティタイムライン

```
[GET /activity]
   │
   ▼
[共通ガード]
   │
   ▼
SELECT *
  FROM audit_logs
 WHERE actor_id = $1 OR target_id IN (SELECT id FROM files WHERE owner_id=$1)
 ORDER BY occurred_at DESC
 LIMIT 50 OFFSET ?;
   │
   ▼
return HTML タイムライン
```

## 11. アンドゥ（リネーム / 移動限定、5 分以内）

```
[POST /undo]
   body: { "audit_log_id_bin": "..." }
   │
   ▼
[共通ガード]
   │
   ▼
SELECT * FROM audit_logs WHERE id_bin=? AND actor_id_bin=session.user_id
   ├── 5 分以上前 → 410 Gone (アンドゥ期限切れ)
   ├── action が 'file.rename' or 'file.move' でない → 400
   └── OK →
         -- MEDIUM 修正: 通常のリネームハンドラに委譲
         逆方向の PATCH /files/{id} を内部呼び出し
           - body: { "path": <details_json.from> }
           - header: If-Match: <現在の version_id>   (OCC 必須)
           - 通常ハンドラの分岐を経由するため:
              * 戻し先に同名 active があれば 409 Conflict (ユーザに選択モーダル)
              * version_id 不一致 (アンドゥ期間中に他端末が触った) → 409
              * trashed/purged → 410
         成功時:
           INSERT audit_logs (action='file.undo', details_json={ undid: <orig_log_id> })
```

「アンドゥだから無条件で戻る」とはしない。アンドゥも通常の rename/move 経路を通り、OCC・同名衝突・状態遷移チェックを必ず通過する（INV-5）。

## 12. 例外一覧と HTTP ステータス対応表

| 状況 | HTTP コード | レスポンス本体 |
|---|---|---|
| 認証なし・期限切れ | 401 | ログインページへリダイレクト or JSON `{kind:"unauthenticated"}` |
| 認可拒否 | 403 | JSON `{kind:"forbidden"}` |
| 対象なし | 404 | JSON `{kind:"not_found"}` |
| 対象が trashed | 410 | JSON `{kind:"gone", restorable_until:"..."}` |
| 対象が purged/gone | 410 | JSON `{kind:"gone", restorable:false}` |
| 同名で衝突 | 409 | JSON `{kind:"name_conflict", suggestions:[...]}` |
| バージョン衝突 | 409 | JSON `{kind:"version_mismatch", current:{...}, options:[...]}` |
| 事前条件なし | 428 | JSON `{kind:"precondition_required"}` |
| 事前条件失敗 | 412 | JSON `{kind:"precondition_failed", reason:"..."}` |
| サイズ超過 | 413 | JSON `{kind:"too_large", limit_bytes:...}` |
| レート制限 | 429 | JSON `{kind:"rate_limited", retry_after_seconds:...}` |
| バリデーション失敗 | 400 | JSON `{kind:"invalid_input", field:"..."}` |
| サーバ内部 | 500 | JSON `{kind:"internal", request_id:"..."}` |
| ストレージ満杯 | 507 | JSON `{kind:"insufficient_storage"}` |

すべての 4xx / 5xx は監査ログには **記録しない**（INSERT only ポリシー）。代わりに CloudWatch にメトリクスとして送る。

## 13. UI からの操作 ↔ HTTP リクエスト対応表

| UI 操作 | HTTP メソッド | パス | 主要ヘッダ |
|---|---|---|---|
| ファイルアップロード | POST または PUT | `/files`（新規）または `/files/{id}`（更新） | `If-None-Match: *` または `If-Match: <ver>` |
| アップロード再開 (tus) | HEAD / PATCH | `/uploads/{id}` | `Tus-Resumable: 1.0.0`, `Upload-Offset` |
| ダウンロード | GET | `/files/{id}` | `If-None-Match: <ver>`（任意） |
| プレビュー | GET | `/files/{id}/preview` | |
| リネーム / 移動 | PATCH | `/files/{id}` | `If-Match: <ver>` |
| ソフト削除 | DELETE | `/files/{id}` | `If-Match: <ver>` |
| 復元 | POST | `/files/{id}/restore` | |
| 物理削除（明示） | POST | `/files/{id}/purge` | パスワード再入力 |
| 公開リンク作成 | POST | `/files/{id}/share-links` | |
| 公開リンク取り消し | DELETE | `/share-links/{id}` | |
| 検索 | GET | `/search` | |
| アクティビティ | GET | `/activity` | |
| アンドゥ | POST | `/undo` | |

すべて HTMX の標準属性（`hx-post`, `hx-delete` など）でバインディング可能。

---

次の章: [`06-data-loss-prevention.md`](./06-data-loss-prevention.md)
