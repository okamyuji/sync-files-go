# 04. 同期セマンティクス（最重要）

> 本書のすべての章のうち、**ここが最も重要**。同期で「何を優先するか」「どこで人間に判断を委ねるか」を厳密に定義する。

## 1. 出発点となる問い

「Web UI で複数端末から同じファイルを操作したとき、何が起きるべきか」を、設計のあらゆる場面で次の 3 つの問いで点検する：

1. **意図せぬデータ消失は起きないか？**（最優先）
2. **意図せぬ自動上書きは起きないか？**
3. **その状態は復元可能か？**

## 2. 同期モデル（What is synchronization here?）

本システムの「同期」は次の通り定義する。

> **同期とは、サーバを介して片方向に書き込みが累積する過程である。** 端末間の双方向自動マージは含まない。

つまり：

- 「A 端末で更新 → サーバに反映 → B 端末がそれを取得」までが同期の範囲
- 「A 端末で更新 → サーバ → B 端末で別の更新 → サーバ → 結果を A・B でマージ」のうち、**マージ部分は人間が行う**

これは Dropbox の "conflicted copy" 戦略を継承する考え方で、自動マージの罠を構造的に回避する。([参考: Dropbox の conflicted copy](https://help.dropbox.com/organize/conflicted-copy))

## 3. 5 つの不変条件（再掲）

| ID | 不変条件 | 意味 |
|---|---|---|
| INV-1 | 物理削除は二段階 | 「削除」は常にソフト削除。物理削除は (a) ユーザの明示操作 OR (b) 30 日経過 の AND |
| INV-2 | 書き込みは累積 | 上書きでも S3 バージョニングで旧版が必ず残る |
| INV-3 | 自動マージ禁止 | サーバはバイトレベルでも文字列レベルでもマージしない |
| INV-4 | 未完了は本番に影響しない | 一時領域 → 完了時にだけ確定 |
| INV-5 | 明示的同意なき破壊的操作の禁止 | UI で確認モーダル必須 |

## 4. OCC（楽観的同時実行制御）

「後勝ち」「先勝ち」のどちらでもなく、**衝突を検出して人間に委ねる**方式を取る。これが本設計の中心戦略。

### 4.1 アップロード時の OCC プロトコル

クライアント（HTMX フォーム）は次のヘッダのいずれかを必ず送る：

| ヘッダ | 場面 | 意味 |
|---|---|---|
| `If-None-Match: *` | 新規作成のみを意図 | 同名ファイルがあったら 412 で失敗させる |
| `If-Match: <version_id>` | 既存ファイルへの上書きを意図 | サーバの現行版がこの version_id と一致するときだけ上書き |
| `If-Match: *` | 強制上書き | 競合検出をスキップ（ユーザが確認モーダルで明示選択した場合のみ） |

ヘッダなしのアップロードは **HTTP 428 Precondition Required** を返して拒否する。

### 4.2 サーバ側ロジック（疑似コード）

```go
func handleUpload(ctx, w, r) {
    sess := authn(r)
    pre  := parsePrecondition(r)         // If-Match / If-None-Match を解釈
    path := canonicalPath(r.URL.Path)    // NFC 正規化、パストラバーサル拒否
    body := io.LimitReader(r.Body, MaxUploadSize)

    tx, _ := db.BeginTx(ctx)
    defer tx.Rollback()

    cur, _ := tx.QueryRow(ctx,
        `SELECT id, current_version_id, state
           FROM files
          WHERE owner_id = $1 AND path = $2 AND state = 'active'
          FOR UPDATE`,
        sess.UserID, path,
    )

    switch {
    case cur == nil && pre.IfNoneMatch == "*":
        // 新規作成 OK
    case cur == nil && pre.IfMatch != "":
        return http.Error(w, "no such file", 412)
    case cur != nil && pre.IfNoneMatch == "*":
        return http.Error(w, "file exists", 412)
    case cur != nil && pre.IfMatch == "*":
        // 強制上書き（UI から明示同意済み）
    case cur != nil && pre.IfMatch == cur.CurrentVersionID:
        // 通常の上書き OK
    case cur != nil && pre.IfMatch != cur.CurrentVersionID:
        // ★ 競合検出
        return writeConflictResponse(w, cur)
    default:
        return http.Error(w, "precondition required", 428)
    }

    // ここから実書き込み
    enc, err := encryptStream(body, kek(ctx, sess.UserID))
    tmpKey := tmpKeyFor(sess.UserID, uploadUUID)
    if err := writeWithFsync(tmpKey, enc); err != nil {
        return // tmp は削除される
    }

    finalKey := finalKeyFor(sess.UserID, fileUUID)
    if err := os.Rename(tmpKey, finalKey); err != nil {
        return
    }

    versionID := uuid.New()
    versionNumber := nextVersionNumber(tx, fileID)

    tx.Exec(ctx, `INSERT INTO file_versions ...`, ...)
    tx.Exec(ctx, `UPDATE files SET current_version_id = $1, updated_at = now() ...`, versionID)
    tx.Exec(ctx, `INSERT INTO audit_logs ...`, ...)

    if err := tx.Commit(); err != nil {
        // 重要: ファイルはすでに current/ にある。
        // 補正ジョブが「メタデータがない孤児ファイル」を検出して /_orphan に隔離する
        return
    }

    w.Header().Set("ETag", versionID.String())
    w.WriteHeader(204)
}
```

### 4.3 競合レスポンス（HTTP 409）

サーバは次の JSON を返し、HTMX 側で動的にモーダルを描画する。

```http
HTTP/1.1 409 Conflict
Content-Type: application/json
HX-Trigger: openConflictModal

{
  "kind": "version_mismatch",
  "file": {
    "id": "8a3f...",
    "path": "/Reports/Q2.docx",
    "current_version_id": "fa12...",
    "current_modified_at": "2026-04-29T05:32:11Z",
    "current_modified_by_session": "sess-iphone-x",
    "size_bytes": 84211
  },
  "options": [
    { "id": "view_server",      "label": "サーバ版を確認",                "method": "GET",  "url": "/files/8a3f..." },
    { "id": "save_as_copy",     "label": "別名で保存",                     "method": "POST", "url": "/files/8a3f.../save-as-copy" },
    { "id": "force_overwrite",  "label": "上書き（旧版は30日復元可）",     "method": "PUT",  "url": "/files/8a3f...", "headers": { "If-Match": "*" }, "warn": true },
    { "id": "cancel",           "label": "キャンセル" }
  ]
}
```

### 4.4 コンフリクトコピーの命名規則

「別名で保存」を選んだ場合：

```
原ファイル名:  Q2 Report.docx
コンフリクトコピー: Q2 Report (conflict 2026-04-29 14-32 device-Pixel).docx
```

規則：
- `<basename> (conflict YYYY-MM-DD HH-MM <device-label>).<ext>`
- device-label はクライアントのセッション情報から推定（ユーザに後で書き換え可能）
- 同名のコンフリクトコピーがすでにあれば、末尾に連番を付ける

## 5. 同期で扱う 7 つの主要ケース

### 5.1 ケース 1: 新規アップロード（同名なし）

```
Client → Server: POST /files
                 If-None-Match: *
Server → Client: 201 Created
                 ETag: <new_version_id>
```

### 5.2 ケース 2: 上書き（OCC 一致）

```
Client → Server: PUT /files/{id}
                 If-Match: <stored_version_id>
                 (body)
Server → Client: 204 No Content
                 ETag: <new_version_id>
```

### 5.3 ケース 3: 上書き（OCC 不一致 = 競合）

```
Client → Server: PUT /files/{id}
                 If-Match: <stale_version_id>
                 (body)
Server → Client: 409 Conflict
                 (JSON 詳細 § 4.3)
                 → HTMX が選択モーダルを描画
                 → ユーザが選択するまで何も起きない（INV-5）
```

### 5.4 ケース 4: 削除

```
Client → Server: DELETE /files/{id}
Server → Client: 200 OK
  (UI に「ゴミ箱に移動しました。30 日以内なら復元できます」)

  内部:
    UPDATE files SET state='trashed', deleted_at=now() WHERE id=...
    INSERT audit_logs (...)
```

S3 Files 上のオブジェクトは無傷（INV-1）。

### 5.5 ケース 5: 復元（ゴミ箱から戻す）

```
Client → Server: POST /files/{id}/restore
Server → Client: 200 OK

  if 元の path にすでに同名ファイルあり:
    → 別名復元 (conflict 命名規則)
    → audit_logs に 'file.restore_with_rename' を記録
  else:
    UPDATE files SET state='active', deleted_at=NULL WHERE id=...
```

### 5.6 ケース 6: リネーム / 移動

```
Client → Server: PATCH /files/{id}
                 If-Match: <version_id>     -- リネームでも OCC を厳格に
                 { "path": "/Reports/Q2 Final.docx" }
Server → Client: 200 OK

  if 移動先に同名がある: 409 Conflict
  if version_id 不一致: 409 Conflict
  else:
    UPDATE files SET path=..., updated_at=now()
    INSERT audit_logs (action='file.rename', details_json={ from, to })
```

リネーム / 移動は S3 上のオブジェクトキー（UUID 固定）を変更しない。

### 5.7 ケース 7: 物理削除（バッチ）

ECS Scheduled Task が日次で次を実行：

```
SELECT id, owner_id, storage_key
  FROM files
 WHERE state = 'trashed'
   AND deleted_at < now() - INTERVAL '30 days'
 ORDER BY deleted_at ASC
 LIMIT 1000;

for each row:
  os.Remove(storage_key)        -- S3 DeleteMarker が付く（バージョニング ON）
  UPDATE files SET state='purged' WHERE id = ...
  INSERT audit_logs (action='file.purge', irreversible=true, ...)
```

**90 日経過後の最終消去**は S3 のライフサイクルポリシーで実行される（[`09-infrastructure-and-deployment.md`](./09-infrastructure-and-deployment.md) §6 参照）。

## 6. アップロード途中の中断

これは特に重要なので独立節を立てる。

### 6.1 何が起きうるか

| シナリオ | 発生頻度 | 望ましい挙動 |
|---|---|---|
| ブラウザがネットワーク切れ | 高 | 中断・本番ファイル無傷・続きから再開可 |
| ユーザがタブを閉じた | 高 | 中断・本番ファイル無傷・7 日後に tmp 自動削除 |
| サーバ側 Fargate タスクが OOM | 低 | 中断・本番ファイル無傷・他タスクで再開可 |
| S3 Files が一時的に応答遅延 | 中 | リトライ後成功 or 失敗時はクライアント側で再試行 |
| OCC 検査後、書き込み中に他端末が上書き | 低 | 検査時点の version_id でリネーム前の最終チェック → 不一致なら abort |

### 6.2 設計上の対策

1. **tus.io 互換のレジューム**：5MB 以上は tus プロトコルで PATCH を反復。クライアントは中断時に HEAD でオフセット問い合わせ → 続きから PATCH。
2. **一時ファイル分離**：`/var/data/.../tmp/{upload_uuid}.part` に書き、完了時に `os.Rename` で原子的に確定。
3. **二重 OCC**：書き込み開始前と最終確定前の 2 回、`SELECT FOR UPDATE` で version_id を再確認。
4. **upload_sessions の TTL**：7 日経過した未完成セッションはバッチで掃除。
5. **fsync の限界を認める**：S3 Files / NFS v4.1+ では `fsync(2)` が必ずしも完全な耐久性を保証しない場合があるので、**書き込み後に SHA-256 で検証**（書き込み直後に `Read` してハッシュを再計算し、リクエスト時計算値と比較）する。

## 7. ダウンロード時の整合性

### 7.1 単純ダウンロード

クライアントは `Accept: application/octet-stream` で `GET /files/{id}` し、サーバは現在の `current_version_id` の本体をストリーム返却する。レスポンスヘッダ：

```
ETag: "<current_version_id>"
Last-Modified: <updated_at>
X-File-Version: <version_number>
Content-Length: <size_bytes>
Content-Type: <content_type>
Content-Disposition: attachment; filename*=UTF-8''<encoded>
```

### 7.2 条件付きダウンロード（304 Not Modified）

クライアントが `If-None-Match: <known_version_id>` を送ると、現在の `current_version_id` が一致する場合 304 を返す。これにより HTMX SSE で「変更通知 → 必要なものだけ再取得」が効率化される。

### 7.3 ダウンロード中の上書き

ダウンロードはストリーム開始時にオープンしたファイルディスクリプタから読み続ける。同時に他端末が上書きしても、`os.Rename` で原子的に切り替わるため、**読みかけのバイト列は壊れない**（古い inode 経由で読み続ける、Linux 標準挙動）。

ただし NFS では「open 中のファイルが unlink された後の挙動」がローカル FS と異なる場合があるので、検証項目として [`13-risks-and-open-questions.md`](./13-risks-and-open-questions.md) §3 に記載する。

## 8. 通知 SSE と「自動同期」の境界線

### 8.1 何を通知するか

```
イベント                        SSE タイプ
file.uploaded (他端末から)     'file_changed'
file.updated  (他端末から)     'file_changed'
file.deleted  (他端末から)     'file_deleted'
file.restored (他端末から)     'file_restored'
share.created                   'share_created'
```

### 8.2 通知を受けても、勝手に何もしない

```
[Server] ──SSE: 'file_changed' { id, path, by_session }── [Browser]
                                                           │
                                                           ▼
                                        HTMX が画面の右上に「変更あり (3)」バッジを描画
                                        ユーザがクリックして初めて再取得
```

**ローカルブラウザに開きかけのプレビューや未保存の編集は強制的に置き換えない。** これは INV-3 と INV-5 の遵守。

### 8.3 自分の操作ループ防止

自分のブラウザがアップロードを完了した直後、その通知が SSE で自分自身にも届く。この時：

- 通知の `by_session` が現在の自分のセッション ID と一致したら、UI は無視する
- 監査ログには記録される（自分の操作なので）

## 9. なぜ「先勝ち」「後勝ち」を採らないか

| 戦略 | 利点 | 致命的な欠点 |
|---|---|---|
| 後勝ち（LWW） | 実装が単純、UI もシンプル | 古い版で上書きされても気づかない（A の作業が静かに消える） |
| 先勝ち | データロス回避 | 「アップロードがブロックされる」UX が混乱する。何が"先"かは時計依存 |
| **OCC + コンフリクトコピー** | **データロスが構造的に発生しない** | UI に選択モーダルが要る（実装コスト中） |

本設計が OCC を採るのは、「個人専用」という前提でも **同じユーザが複数端末を持つ限り、競合は十分起きうる** こと、そして「衝突したことに気づかない」ことが最悪の事態であるためである。

ADR は [`adr/ADR-003-occ-not-last-write-wins.md`](./adr/ADR-003-occ-not-last-write-wins.md) を参照。

## 10. テストすべき同期シナリオ

| シナリオ | 期待結果 |
|---|---|
| 端末 A・B が同じ旧版を持ち、同時に上書き | 1 つは成功、もう 1 つは 409。コンフリクトコピーで両方残る |
| 端末 A がアップロード中に B が削除 | A の `os.Rename` 完了前なら、B の delete を許可。A は完了時に `state` が `trashed` であることを検出して abort（または別パスで復活） |
| 端末 A が削除した直後、B が同名アップロード | B は `If-None-Match: *` で 201 created 成功（同名ファイルがアクティブに存在しないため） |
| 31 日経過 → バッチで物理削除 | S3 上に DeleteMarker。`state = purged`。バージョニングは残る |
| 物理削除後、ユーザが「やっぱり戻したい」 | UI からは不可。運用ツール（CLI）で S3 旧版から復元可能（手順は [`10-operations.md`](./10-operations.md)） |
| ネットワーク切断中の PATCH | 再開時に HEAD でオフセット取得 → 続きから PATCH |
| ファイルアップロード中にサーバ Fargate タスク再起動 | upload_session が残る。クライアントが再開可能（HEAD で確認） |

詳細は [`11-testing-strategy.md`](./11-testing-strategy.md) §5。

---

次の章: [`05-file-operations-logic-tree.md`](./05-file-operations-logic-tree.md)
