# 11. テスト戦略

> 「機能要件を満たす」だけでなく、**不変条件（INV-1〜INV-5）を継続的に証明し続ける** ためのテスト戦略。

## 1. テストピラミッド

```
                ┌──────────┐
                │   E2E    │  ← Playwright で UI 駆動。〜30 シナリオ
                │  (slow)  │
                ├──────────┤
                │  統合    │  ← 実 Postgres + ローカル FS。〜200 シナリオ
                │  (medium)│
                ├──────────┤
                │  単体    │  ← Go test。〜500 ケース、80% カバレッジ
                │  (fast)  │
                └──────────┘
```

下に行くほど数が多く実行が速く、上に行くほど少なく遅い。

## 2. 単体テスト

### 2.1 対象

- ピュアな業務ロジック（`internal/domain/*`）
- 暗号化・認証（`internal/crypto/*`）
- パス正規化・バリデーション
- OCC ロジック（`internal/sync/occ.go`）
- 競合命名規則（`internal/sync/conflict.go`）

### 2.2 ツール

- `go test`（標準）
- `testify/require`（最小限の assertion ヘルパとして許容）
- テーブル駆動テスト

### 2.3 例

```go
func TestOCC_PreconditionResolve(t *testing.T) {
    cases := []struct {
        name    string
        cur     *file
        ifMatch string
        ifNone  string
        want    occOutcome
    }{
        {"new file with If-None-Match: *", nil, "", "*", outcomeAccept},
        {"new file with If-Match", nil, "v1", "", outcomePreconditionFailed},
        {"existing with matching If-Match", &file{verID: "v1"}, "v1", "", outcomeAccept},
        {"existing with stale If-Match", &file{verID: "v2"}, "v1", "", outcomeConflict},
        {"existing with If-Match: *", &file{verID: "v2"}, "*", "", outcomeForceAccept},
        {"existing with If-None-Match: *", &file{verID: "v1"}, "", "*", outcomePreconditionFailed},
        {"no headers", &file{verID: "v1"}, "", "", outcomeNeedPrecondition},
    }
    for _, tc := range cases {
        t.Run(tc.name, func(t *testing.T) {
            got := resolveOCC(tc.cur, tc.ifMatch, tc.ifNone)
            if got != tc.want { t.Fatalf("want %v, got %v", tc.want, got) }
        })
    }
}
```

### 2.4 カバレッジ

- 目標 80%（`go test -cover`）
- ユーティリティ・テンプレート・main は対象外
- カバレッジは「下げない」を CI でチェック

## 3. 統合テスト

### 3.1 対象

- 実 PostgreSQL（Docker）に対するリポジトリ層
- 実ファイルシステム（一時ディレクトリ）に対するストレージ層
- HTTP ハンドラ（`net/http/httptest`）+ DB 接続
- マイグレーション実行

### 3.2 ツール

- `dockertest` または `testcontainers-go` を使用検討（最終選定は実装時）
- ただしテスト時間とのトレードオフを見て、make target で `make test-integration` を分離

### 3.3 例

```go
func TestUploadDelete_RoundTrip(t *testing.T) {
    db := setupTestDB(t)
    fs := setupTempFS(t)
    app := newApp(db, fs)

    body := strings.NewReader("hello world")
    req := httptest.NewRequest("POST", "/files", body)
    req.Header.Set("If-None-Match", "*")
    req.Header.Set("X-File-Path", "/test.txt")

    w := httptest.NewRecorder()
    app.ServeHTTP(w, withSession(t, req, "user-1"))
    require.Equal(t, 201, w.Code)
    fileID := w.Header().Get("X-File-Id")
    versionID := w.Header().Get("ETag")

    // ファイルが S3 Files (ローカル FS) にあるか
    contents := readFromStorage(t, fs, "owner-user-1/current/"+fileID)
    require.Equal(t, "hello world", decryptForTest(t, contents))

    // ソフト削除
    req2 := httptest.NewRequest("DELETE", "/files/"+fileID, nil)
    req2.Header.Set("If-Match", versionID)
    w2 := httptest.NewRecorder()
    app.ServeHTTP(w2, withSession(t, req2, "user-1"))
    require.Equal(t, 200, w2.Code)

    // メタデータでは trashed
    var state string
    db.QueryRow("SELECT state FROM files WHERE id=$1", fileID).Scan(&state)
    require.Equal(t, "trashed", state)

    // S3 Files 上のファイルは無傷 (INV-1)
    require.True(t, fileExistsInStorage(t, fs, "owner-user-1/current/"+fileID))
}
```

## 4. E2E テスト

### 4.1 ツール

- Playwright（Node.js ベース、TypeScript で記述）
- 主要シナリオ 30 程度
- CI でブラウザバージョンを固定

### 4.2 主要シナリオ

| シナリオ | 検証内容 |
|---|---|
| サインアップ → ログイン → ログアウト | 認証フロー |
| TOTP 設定 → 再ログイン → TOTP 入力 | 2FA |
| 単純ファイルアップロード（< 5MB） | 成功・進捗バー |
| 大容量ファイルアップロード（500MB） + 中断 + 再開 | tus.io レジューム |
| 同名ファイルの上書き（OCC 一致） | 旧版が残ること |
| 同名ファイルの上書き（OCC 不一致） | 409 + 選択モーダル |
| コンフリクトコピー保存を選択 | 命名規則どおりの新ファイル |
| 強制上書き → 旧版から復元 | 旧版が UI に出る |
| 削除 → ゴミ箱で復元 | 戻る |
| 削除 → 31 日後（時計を進める）→ 物理削除バッチ | purged 状態に |
| 公開リンク作成 → 期限内ダウンロード | OK |
| 公開リンク作成 → 期限切れ後 | 410 Gone |
| 公開リンクをパスワード保護 → 入力で閲覧 | OK |
| ファイル名検索 | 結果が出る |
| アクティビティタイムライン | 主要操作が記録 |
| アクセシビリティ：Tab だけで全機能を操作 | 通る |
| ダーク・ライトモード切替 | 両方が崩れない |

### 4.3 時計の進め方

物理削除バッチ・期限切れリンクなどは「31 日後」「7 日後」を待てない。サーバ側に環境変数 `CLOCK_OFFSET_SECONDS` を導入し、テスト時のみ `time.Now()` を `realNow().Add(offset)` にする実装にする（本番では 0）。

## 5. 同期競合シナリオテスト（重点）

```
[テスト 1: 並行アップロード]
   - 2 つのクライアント A, B が同じ初版を取得
   - A が PUT (If-Match: v1) → 成功 (v2 が正)
   - B が PUT (If-Match: v1) → 409 Conflict
   - B が「別名で保存」を選択 → コンフリクトコピーが作られる
   - 最終的にファイル一覧に元 + コピー の 2 つ

[テスト 2: 削除と上書きの競合]
   - A が GET → ローカル編集
   - B が DELETE → trashed
   - A が PUT (If-Match: v1) → ファイルが trashed なので 410 Gone
   - A は「ゴミ箱から復元」もしくは「新規作成として再アップロード」を選ぶ

[テスト 3: 二重 OCC のレース]
   - A が PUT 開始（OCC 通過、書き込み中）
   - B が PUT 完了（v2 が正）
   - A の書き込みが完了して二重 OCC で再確認 → v1 != v2 で 409
   - A の tmp/ ファイルは破棄される

[テスト 4: アップロード中断・再開]
   - A が 1GB ファイルの PATCH を開始（500MB アップロード後にネットワーク切断）
   - 30 秒後に再接続
   - HEAD で Upload-Offset: 524288000 を取得
   - 続きから PATCH → 完了
   - 最終 SHA-256 が一致

[テスト 5: SSE 通知のループ防止]
   - A がアップロード完了 → 自分のセッション宛にも SSE 通知
   - クライアント側で by_session を見て自分の通知を無視
   - UI に「変更あり」バッジが出ない
```

これらは E2E と統合テストの両方で検証。

## 6. アンチパターンテスト

[`06-data-loss-prevention.md`](./06-data-loss-prevention.md) §9 で挙げた「やってはいけない」パターンに対し、CI で防御テストを設置：

```go
func TestNeverPhysicalDeleteWithoutPurge(t *testing.T) {
    // ソフト削除 → 翌日 → state は trashed のまま
    // S3 Files 上のオブジェクトが残っていること
}

func TestNeverAcceptUploadWithoutPrecondition(t *testing.T) {
    // If-Match / If-None-Match なしでアップロード → 428
}

func TestAuditLogIsImmutable(t *testing.T) {
    // INSERT → SELECT で見える
    // UPDATE / DELETE を試みる → 権限エラー
}

func TestRenameDoesNotMoveStorageObject(t *testing.T) {
    // ファイル PUT → リネーム
    // S3 Files 上のキーが変わっていないこと（O(1) リネーム）
}
```

## 7. 障害シナリオテスト

| シナリオ | 期待 |
|---|---|
| RDS 一時切断（5 秒） | アプリは 503 を返し、復旧後自動再接続 |
| S3 Files マウント解除 | アプリは `/readyz` を 503 にし、ユーザには明示的なエラー |
| 暗号鍵欠損 | アプリは起動を拒否し、CloudWatch にエラー |
| Postgres 接続プール枯渇 | リクエストはキューイング、タイムアウト時 503 |
| アップロード中の OOM | tmp/ にゴミが残るが補正ジョブで掃除される |

これらは Chaos Engineering 手法ではないが、`make test-failure` で意図的に注入できる仕組みを用意。

## 8. セキュリティテスト

### 8.1 静的解析

- `go vet`
- `staticcheck`
- `gosec` (`-quiet -severity high`)
- `govulncheck`（脆弱性 DB）
- `golangci-lint` の標準ルール

CI で必須。

### 8.2 動的テスト

- OWASP ZAP の自動スキャン（v1 リリース時に 1 回、年次で実行）
- E2E にいくつかの典型的な攻撃を組み込む（SQLi 試行、XSS 試行 → どちらも 400 か 403 を返すこと）

### 8.3 ペネトレーション項目

- パストラバーサル（`../`）
- NULL バイト（`\x00`）
- Unicode 等価性（NFC/NFD）の差分攻撃
- セッション固定攻撃
- CSRF 試行
- 公開リンクの推測攻撃
- Cookie HMAC の改ざん

## 9. 性能テスト

### 9.1 ツール

- k6（JavaScript シナリオ）
- 単一プロセスでの負荷生成

### 9.2 シナリオ

```js
// アップロードスループット
import http from 'k6/http';
import { check, sleep } from 'k6';

export const options = {
  vus: 5,
  duration: '5m',
};

export default function () {
  const data = open('./testfile-10mb.bin', 'b');
  const res = http.post('https://sync.example.com/files', data, {
    headers: {
      'If-None-Match': '*',
      'Content-Type': 'application/octet-stream',
      'X-File-Path': `/load-${__VU}-${__ITER}.bin`,
      'Cookie': `__Host-sync_session=${__ENV.SESSION}`,
    },
  });
  check(res, { 'status is 201': (r) => r.status === 201 });
  sleep(1);
}
```

期待: 5 VU / 10MB / VU で 1 分あたり 50 アップロードを完了 (RDS / S3 Files / アプリすべて p95 < 5s)。

## 10. アクセシビリティテスト

- axe-core を Playwright に組み込み（`@axe-core/playwright`）
- 各ページ訪問時に scan、Critical / Serious 違反があれば失敗
- スクリーンリーダー手動確認（年次）

## 11. テストデータ

### 11.1 シードデータ

```
3 ユーザ:
  - alice@example.local (パスワード強度 strong, TOTP 有効)
  - bob@example.local   (パスワード strength weak, TOTP 無効)
  - carol@example.local (ロック中)

ファイル:
  - 各ユーザに 50 ファイル
  - サイズ分布: 1KB - 100MB
  - 一部に複数バージョン
  - 一部 trashed
```

シード生成スクリプトは `tests/seed.go`。

### 11.2 機密データの扱い

テストデータには本物のような個人情報を入れない（合成データ）。テスト用の暗号鍵は固定値（コードに含めて OK、本物ではないことを明示）。

## 12. CI パイプライン

```
PR open
  ↓
1. setup go + node
  ↓
2. go vet, staticcheck, gosec, govulncheck (parallel)
  ↓
3. unit test + coverage check
  ↓
4. integration test (with postgres + minio container)
  ↓
5. build docker image
  ↓
6. trivy image scan
  ↓
7. e2e test (subset)  ← merge 前必須
  ↓
8. report: lint + coverage + e2e screenshots
  ↓
ブランチ保護でマージ可能になる

main へマージ後:
  - terraform plan を投稿（人間レビュー）
  - 承認 → terraform apply
```

## 13. 受け入れ基準（Definition of Done）

ある機能が「できた」と言える条件：

- [ ] 単体テストでカバー（カバレッジ低下なし）
- [ ] 統合テストでカバー（必要な場合）
- [ ] E2E シナリオに追加（ユーザ向け機能の場合）
- [ ] 不変条件 INV-1〜INV-5 と矛盾しないことを点検
- [ ] アクセシビリティ axe-core で違反なし
- [ ] gosec / govulncheck でクリティカル違反なし
- [ ] ログに機密情報が出ない
- [ ] ドキュメント（このディレクトリ）の該当章を更新
- [ ] 関連 ADR を新規作成または更新

## 14. テストできないもの

設計上、自動テストでは検証しきれない領域：

- 「ユーザが UI を見て直感的に分かるか」 → 手動チェック + フィードバック
- 「攻撃者が 0day を使ったらどうなるか」 → 監査・依存スキャン・ログ監視で間接的に
- 「リージョン障害」 → DR リハーサルで部分的に
- 「AWS のサービス内部障害」 → 監視と Runbook

これらは [`13-risks-and-open-questions.md`](./13-risks-and-open-questions.md) §5 で「許容済みリスク」として明示。

---

次の章: [`12-roadmap.md`](./12-roadmap.md)
