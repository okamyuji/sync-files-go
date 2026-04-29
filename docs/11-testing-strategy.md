# 11. テスト戦略

> 「機能要件を満たす」だけでなく、**不変条件（INV-1〜INV-5）を継続的に証明し続ける** ためのテスト戦略。

## 0. 二段階のテスト実施

本システムは **ローカル Docker** での反復可能なテストを主軸にし、最終的に **Terraform で本番（または staging）にデプロイした実環境で実ブラウザ統合テスト** を必須とする。

```
┌──────────────────────────────────────────────────────────────┐
│ ステージ A: ローカル Docker 内テスト（反復・速い）            │
│   - 単体テスト（Go test）                                     │
│   - 統合テスト（testcontainers-go: MySQL Primary+Replica）    │
│   - E2E（Playwright）はローカル docker compose 起動で実行    │
└──────────────────────────────────────────────────────────────┘
                          ↓ all green
┌──────────────────────────────────────────────────────────────┐
│ ステージ B: 本番/staging Terraform デプロイ後の実ブラウザ統合 │
│   - terraform apply で ECS Fargate / RDS / S3 Files / cloudflared をデプロイ │
│   - 実ブラウザ（Playwright headed もしくは手動）で必須シナリオ実行 │
│   - smoke + 損失防止クリティカルシナリオが pass で初めてリリース可 │
└──────────────────────────────────────────────────────────────┘
```

「ローカルだけで通った」状態をリリース基準にしない。実 AWS 上の S3 Files / Cloudflare Tunnel / RDS Multi-AZ / Read Replica 経由で確認するまでは未完了。

## 1. テストピラミッド

```
                ┌──────────┐
                │   E2E    │  ← Playwright (local docker) +  実ブラウザ (本番) で確認
                │  (slow)  │
                ├──────────┤
                │  統合    │  ← ローカル Docker (testcontainers-go) で MySQL Primary+Replica + FS
                │  (medium)│
                ├──────────┤
                │  単体    │  ← Go test。〜500 ケース、80% カバレッジ
                │  (fast)  │
                └──────────┘
```

下に行くほど数が多く実行が速く、上に行くほど少なく遅い。**統合・E2E はまずローカル Docker で完結させ、最後に本番環境で再走させる**のがポリシー。

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

- 実 MySQL（Docker、Primary 1 + Replica 1）に対するリポジトリ層
- 実ファイルシステム（一時ディレクトリ）に対するストレージ層
- HTTP ハンドラ（`net/http/httptest`）+ DB 接続
- マイグレーション実行
- DBRouter のルーティング判断（Reader / Writer / RAW window / 縮退運転）

### 3.2 ツール

- `testcontainers-go`（推奨）で MySQL を起動。docker-compose で Primary + Replica 構成も組める
- ただしテスト時間とのトレードオフを見て、make target で `make test-integration` を分離

### 3.2.1 DBRouter テスト（必須）

ユーザの参考記事の指針に従い、DBRouter のルーティング判断は単独で必ずテストする：

```go
func TestDBRouter_Routing(t *testing.T) {
    primary := openTestDB(t, "primary")
    replica := openTestDB(t, "replica")
    r := &DBRouter{primary: primary, replica: replica}

    ctx := context.Background()
    require.Equal(t, primary, r.Writer(ctx), "Writer は常に Primary")
    require.Equal(t, replica, r.Reader(ctx), "通常時 Reader は Replica")

    raw := WithReadAfterWrite(ctx)
    require.Equal(t, primary, r.Reader(raw), "RAW window 中は Primary")

    r.replicaDegraded.Store(true)
    require.Equal(t, primary, r.Reader(ctx), "Replica 縮退中は Primary")
}
```

加えて：
- アップロード直後の取得は Primary を使うことを HTTP レベルで検証（インテグレーションテスト）
- ファイル一覧表示は Replica を使うことを検証
- Replica が落ちたら自動的に縮退運転になることを検証

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
    versionID := w.Header().Get("ETag")  // version_uuid
    contents := readFromStorage(t, fs, "owner-user-1/versions/"+fileID+"/"+versionID)
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

    // S3 Files 上のバージョンは無傷 (INV-1)
    require.True(t, fileExistsInStorage(t, fs, "owner-user-1/versions/"+fileID+"/"+versionID))
}
```

## 4. E2E テスト

### 4.1 ツール

- Playwright（Node.js ベース、TypeScript で記述）
- 主要シナリオ 30 程度
- CI でブラウザバージョンを固定
- **実行環境を 2 つ持つ**：
  - ローカル：`docker compose up` で起動した一式（mysql + app + nginx）に対する Playwright（headless）。CI で常時実行。
  - 本番/staging：`terraform apply` 完了後、実ブラウザで（Playwright headed もしくは手動）必須シナリオを再走。Cloudflare Tunnel・S3 Files・RDS Multi-AZ・Read Replica 経由の挙動を確認する。

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
| MySQL Primary 接続プール枯渇 | リクエストはキューイング、タイムアウト時 503。Reader 側に逃がせる読み取りは Replica へ |
| MySQL Replica 大遅延 | DBRouter が自動で縮退運転（全 read を Primary に寄せる）。重い管理画面 SQL は context.WithTimeout で守る |
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

## 12. CI / CD パイプライン（2 段ゲート）

### 12.1 PR ゲート（ローカル Docker 内で完結）

```
PR open
  ↓
1. setup go + node
  ↓
2. go vet, staticcheck, gosec, govulncheck (parallel)
  ↓
3. unit test + coverage check
  ↓
4. integration test (testcontainers-go で mysql:8.0 Primary+Replica)
  ↓
5. build docker images (app, nginx)
  ↓
6. trivy image scan (両方のイメージ)
  ↓
7. local docker e2e test (docker compose up + Playwright headless)
  ↓
8. report: lint + coverage + e2e screenshots
```

ここまでが green でなければマージ不可。

### 12.2 リリースゲート（本番/staging に terraform apply 後の実環境テスト）

```
main へマージ
  ↓
1. terraform plan を投稿（人間レビュー）
  ↓
2. 承認 → terraform apply（staging or prod）
  ↓
3. ECS が Rolling Update / cloudflared が Tunnel 再接続
  ↓
4. smoke test（curl / 外部 BASE_URL）
  ↓
5. 実ブラウザ E2E（Playwright headed もしくは手動）
   - ログイン → アップロード（小） → ダウンロード（小）
   - アップロード（500MB tus.io レジューム）
   - 同名上書き OCC：成功と 409 衝突モーダル
   - 削除 → ゴミ箱 → 復元
   - 公開リンク（期限・パスワード）
   - SSE 通知の動作（別ブラウザでアップロード → 通知バッジ）
   - 物理削除（時計操作テストは local のみ。本番では mock 不可なため省略）
  ↓
6. 必須シナリオ全 pass で初めてリリース完了
```

「ローカルで通った」を**リリース基準にしない**。本番環境の Cloudflare Tunnel・S3 Files・RDS Multi-AZ + Read Replica の挙動は local docker と異なる場合があるため、必ず実環境で確認する。

## 13. 受け入れ基準（Definition of Done）

ある機能が「できた」と言える条件：

- [ ] 単体テストでカバー（カバレッジ低下なし）
- [ ] 統合テストでカバー（必要な場合）
- [ ] **ローカル Docker での E2E が green**（ユーザ向け機能の場合）
- [ ] **本番/staging へ Terraform デプロイし、実ブラウザで対応シナリオを通す**（リリースゲート）
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
