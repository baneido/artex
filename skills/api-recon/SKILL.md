---
name: api-recon
description: Web サイトの API を収集するときにこのスキルを呼び出します。
---

# API Recon（フロントエンド API の偵察）

**許可を得ている**前提で、次をできる限り完全に見つけます。**バックエンド API**（パス、メソッド、パラメータ、レスポンス本文）、**フロントエンドのルート**、**UI 機能のトリガー**（Tab、ダイアログ、テーブル操作など）。

---

## 境界と禁止（Agent 必読。違反は範囲外）

このスキルは **API とパラメータ面の偵察だけ**を行い、脆弱性の発掘やペネトレーションの利用段階ではありません。

### タスクの境界

| 範囲 | 許可 | 禁止 |
|---|---|---|
| **目標** | path、method、パラメータ、ルート、UI のトリガーを列挙する | SQLi、XSS、権限越え、総当たり、fuzz による脆弱性、パケット改ざん攻撃、破壊的な操作 |
| **認証** | Hook と stub または mock で**クライアント**のログイン門を回避する | ユーザーにアカウントとパスワードを求める、または推測する。実ログインフォームの送信を試す |
| **実行時** | 資格情報が無い状態でインターフェースを hook し、mock 応答で SPA をログイン後の殻まで入れる | 実バックエンドのセッションが無いと続けられない処理 |

### 資格情報の無い動的解析（Phase 3 の既定）

1. `preload.js` と `runtime_harvest.js` で、ログイン、権限、メニューなどの bootstrap インターフェースを**インターセプトして stub** します。
2. 業務の照会インターフェースには、**構造が正しく、業務コードは成功、データは空でもよい** mock body を返します。
3. フロントエンドが、バックエンドが無い、または 401 の環境でもログイン後のページを描画できるようにし、より多くの XHR、fetch、WebSocket を起こします。
4. **空データ、空のテーブル、プレースホルダ UI は想定どおりです**。そのために実ログインや脆弱性テストへ転じてはなりません。

**一文で言うと**、mock でフロントエンドのルートとコンポーネントのマウントを広げ、**outbound リクエストだけを記録します**。バックエンドが何を返すかは重要ではなく、フロントエンドが**まだどのインターフェースを送るか**が重要です。

### 手順上の硬い禁止

| 禁止 | 代わりに行うこと |
|---|---|
| Phase 1 が終わる前に、主 entry の `index-*.js` を grep、curl、Read して API path を取る | `OUTDIR/harvest_static.py` を実行する |
| `extract_apis.py` などを手書きして harvest の代わりにする | `OUTDIR/harvest_static.py` を変えて再実行する |
| 同じ grep やコマンドが 2 回以上失敗しても繰り返す | 戦略を変える。tool_logs を読む、harvest を変える、reference を見る |
| 門 A と B を飛ばし、`scripts/` の原版を直接実行する | OUTDIR へコピーし、目標に合わせて変える |
| 実ユーザー名、パスワード、OTP、OAuth などの認証 | stub と mock（上を参照） |
| 「実データを取る」ことを理由に stub を飛ばし、権限越えやインジェクションのテストをする | outbound だけを記録する。偵察の境界である |
| 削除、機微データのエクスポート、一括書き込みなどの不可逆操作 | coverage のクリックも同じ |
| runtime と動的列挙が終わる前に、全ページと全インターフェースを得たと主張する | 「完了の定義」を見るか、限界を明記する |
| パラメータのトリガー行列と diff が終わる前に、全パラメータを把握したと主張する | Phase 3b の行列と Phase 5 の diff |
| 単一の runtime サンプルから必須と任意を推定する | 複数サンプルの diff、または検証ルールとエラーからの逆算 |

---

## 2 層モデルと実行モード

| 層 | 成果 | 上限 |
|---|---|---|
| **静的**（JS bundle） | 全 endpoint パス、ルートの草案、組み立て箇所のフィールド候補 | HTTP メソッドは無い。パラメータは Phase 1b が必要。実行時に連結する URL は漏れる |
| **実行時**（生きたセッション） | メソッド、body、レスポンス、動的 URL、WS と SSE。複数サンプルの diff でパラメータを補う | ページが実際に描画されないとリクエストは出ない。単一サンプルでは必須と任意を決められない |

| 実行モード | エンジン | 向く用途 |
|---|---|---|
| **depth** | `runtime_harvest.js`（Puppeteer） | API 一覧、METHOD、params、レスポンス本文、WS と SSE、再現できる一括実行 |
| **coverage** | browser と `preload.js` | Tab、ダイアログ、テーブルをクリックし、機能点の被覆を深くする |
| **both** | 先に depth、そのあと coverage | 最も完全で、最も時間がかかる |

**パラメータの方法**（汎用スクリプトは無い）: path は harvest と正規表現を使い、パラメータは **アンカーからの窓拡大、UI のバインド連鎖、複数サンプルの diff、エラーからの逆算**を使います（grep のレシピは [reference.md](reference.md) の J 節）。

---

## 完了の定義

すべて満たしてから、recon 完了と主張できます。

- [ ] **静的**: Phase 1 の harvest が `api_static.txt`、`routes.txt`、`js/` を出す
- [ ] **実行時**: depth か coverage の少なくとも一方。coverage と both は **Hook が有効で、動的列挙の環がある**こと
- [ ] **殻に入る**: 業務 path を開いたとき `/login` ではない（hash ルートに注意）
- [ ] **パラメータ**: coverage と both はパラメータのトリガー行列と `param_samples.json` を終える。Phase 5 は `params_merged.json` をマージする
- [ ] **深さ**（モジュールページが空白のとき）: Phase 4 で権限ツリーを復元して再実行し、**module レベルの API** が出るまで続ける（locale と bootstrap だけではない）
- [ ] **引き渡し**: Phase 5 の成果が揃う（Phase 5 の成果表を参照）。`insert_assets` でサービスとエンドポイントの資産を書く

---

## スクリプトと門

`scripts/` は参考テンプレートにすぎず、**原版を直接実行して最終結果にしてはなりません**。

**規則**: 先に読む、目標に合わせて変える、`OUTDIR`（例: `recon/`）へ書く、`CHANGES.md` に記録する。合わなければ方法に沿って書き直し、構造だけを借ります。

| 門 | いつ | 参考スクリプトから OUTDIR の複製へ | よく必須の変更 |
|---|---|---|---|
| **A（静的）** | Phase 0 のあと、harvest と spider を**最初に**走らせる前 | `harvest_static.py`、`spider_mpa.py` | **多くのサイトは既定の regex でそのまま実行できる**。manifest や方言が合わないときだけ、endpoint の正規表現、webpack と Vite の `publicPath`、MPA の exclude と cookie を変える |
| **B（実行時）** | Phase 2 のあと、depth と coverage を走らせる前 | `runtime_harvest.js`、`preload.js` と `config.json` | Cookie と localStorage のキー、neutralize の成功値、stubs、login の正規表現、api のプレフィックス、hash と history |

**SPA の強制順序**（入れ替え不可。Phase 番号は「先に探索してからスクリプト」より優先する）:

| 手順 | 必須 | 禁止 |
|---|---|---|
| Phase 0 が終わったあと | 次の Bash は `python3 OUTDIR/harvest_static.py <URL> OUTDIR` | 主 entry の `index-*.js`（通常 500KB 超）を curl、grep、Read する |
| 門 A | スクリプトをコピーし、必要なら小さく変え、**すぐ実行する** | 先に手で API を取り出してから harvest するかを決める |
| Phase 1 が終わる前 | `wc -l` で成果を検証する。404 なら harvest を変えて再試行する | extract スクリプトを手書きする。未ダウンロードの URL を繰り返し grep する |
| Phase 1b 以降 | grep は `OUTDIR/js/*.js` だけ | 主 bundle を harvest の代わりにする |

- ✅ `harvest_static.py` をコピーし、（任意で）regex を変え、**すぐ実行する**
- ❌ 主 bundle を curl し、何度も grep し、一時 extract を書き、最後になって harvest する
- **MPA**: Phase 0 のあとの次の Bash は `python3 OUTDIR/spider_mpa.py ...`

---

## ツールと出力の制約

| 制約 | 説明 |
|---|---|
| 大きいファイル | 100KB を超える `index-*.js` をコンテキストへ Read や grep してはならない。OUTDIR のスクリプトでバッチ処理する |
| grep の出力 | `\| head -20` か `-m 5` が必須。対話には path の要約だけを残し、bundle の断片を貼らない |
| 検証 | `wc -l` と `ls \| wc -l` を使う。ディレクトリ全体を Read しない |
| regex の初探 | 任意、1 回まで、50KB 以下の小さい chunk か HTML だけ。正式な静的解析は harvest を基準にする |
| reference | レシピ、テンプレート、切り分けは [reference.md](reference.md)。全文をここに繰り返さない |

---

## 実行の道筋

```
Phase 0 分類 + OUTDIR
  → 門 A → Phase 1 harvest（★ すぐ実行 ★）
  → Phase 1b パラメータの逆解析
  → Phase 2 認証の 3 つの門 → config.json
  → 門 B → Phase 3 実行時 + パラメータ行列
  → Phase 4 権限ツリー（必要なとき）→ Phase 3 を再実行
  → Phase 5 レポートのマージ + insert_assets で、発見したサービスとエンドポイント API 資産を一括投入する。投入時に、発見済みの資産を落としてはならない
```

順にチェックします。**前の項目が終わるまで次の Phase へ入ってはなりません**。

1. [ ] **Phase 0**: SPA か MPA かを見る。`OUTDIR` を作る → [Phase 0](#phase-0-分類)
2. [ ] **門 A + Phase 1**: スクリプトをコピーし、**すぐ** harvest し、`wc -l` で検証する → [Phase 1](#phase-1-静的)
3. [ ] **Phase 1b**: アンカーからの窓拡大とバインド層 → `param_candidates.json` → [Phase 1b](#phase-1b-パラメータの逆解析)
4. [ ] **Phase 2**: 認証の 3 つの門 → `config.json` → [Phase 2](#phase-2-認証の-3-つの門)
5. [ ] **門 B**: runtime スクリプトを調整する → [Phase 3](#phase-3-実行時)
6. [ ] **Phase 3**: depth、coverage、both。殻に入ったことを確認する。パラメータのトリガー行列 → `param_samples.json`
7. [ ] **Phase 4**（必要なとき）: 権限ツリー → stub を patch → Phase 3 を再実行 → [Phase 4](#phase-4-権限ツリーの復元)
8. [ ] **Phase 5**: 成果のマージ、レポート、`insert_assets` → [Phase 5](#phase-5-マージとレポート)

---

## Phase 0 分類

入口の HTML を取り、**`OUTDIR` を作ります**（スキル内の `scripts/` は変えないでください）。

- **SPA**: 空の殻、`<div id=app>`、chunk → Phase 1 から 5
- **MPA**: SSR、`<form>`、endpoint bundle が無い → 門 A のあと:

```bash
python3 recon/spider_mpa.py <BASE_URL> <OUTDIR> [--cookie "session=..."] [--max 300] [--depth 5] [--exclude "logout|delete|destroy"]
```

成果は `forms.txt`、`links.txt`、`api_inline.txt` です。SPA で forms がほぼ 0 なら Phase 1 へ切り替えます。

---

## Phase 1 静的

[スクリプトと門](#スクリプトと門) と [ツールと出力の制約](#ツールと出力の制約) に従います。

```bash
python3 recon/harvest_static.py <BASE_URL> <OUTDIR>
```

harvest は、HTML の script を解析し、webpack と Vite の manifest を読み、lazy chunk をすべてダウンロードし、`js/`、`api_static.txt`、`routes.txt`、`chunkmap.txt` を出します。

```bash
wc -l OUTDIR/api_static.txt OUTDIR/routes.txt
ls OUTDIR/js | wc -l
```

- chunk 数と manifest を比べます。404 は harvest を変えて再試行し、chunk を 1 つずつ手で curl しません。
- `api_static.txt` が少なすぎるときは、OUTDIR 内の endpoint 正規表現を緩めて再実行します（reference を参照）。

### Phase 1b パラメータの逆解析

path は Phase 1 から来ます。パラメータのフィールドは別に recon します。grep の規則は [ツールと出力の制約](#ツールと出力の制約) を見てください。

**完了基準**: 重要なインターフェースについて、フィールド名、伝送位置、型の推定、必須かどうか、サンプル値、確信度を答えられることです。

#### 1b.0 伝送の形

| 形 | パラメータの場所 | 静的で先に見るもの |
|---|---|---|
| REST JSON | body と query | path アンカーの隣の `(params\|data\|body)\s*:\s*\{` |
| GraphQL | `variables` | gql テンプレート、`$page: Int` |
| 従来の form | urlencoded | `<form>`、`FormData` |
| ファイルアップロード | multipart | `FormData.append` |
| パスパラメータ | `/user/:id` | ルート表と `useParams`、`$route.params` |
| 暗号化と署名 | `sign` や `data` に包む | 暗号関数の引数を Hook する（reference の D 節） |

成果: インターフェースごとに `transport: query|json|form|graphql|encrypted` を付けます。

#### 1b.1 アンカーからの窓拡大

既知の path をアンカーにし、窓を広げて組み立てオブジェクトを探します。

```bash
grep -n '"/api/user/list"' OUTDIR/js/*.js | head -20
grep -rhoaE '.{0,120}("/api[^"]+").{0,200}' OUTDIR/js/*.js | head -20
grep -rhoaE '(params|data|body|payload)\s*:\s*\{' OUTDIR/js/*.js | head -20
```

| 包みの層 | パラメータの手がかり |
|---|---|
| axios インスタンス | `data`、`params` |
| 統一 request | インターセプタが全体フィールドを注入する |
| OpenAPI クライアント | 生成された method のシグネチャ |
| React Query、SWR | hook の第 2 引数 |
| Vue composable | composable の引数 |

型の残り: `yup`、`zod`、rules、`Form.Item name=`、埋め込み Swagger。

→ `param_candidates.json`: `{ path, fields[], source: "static-callsite", confidence }`

#### 1b.2 バインド層

```
Form field → onFinish/handleSubmit → transform → API payload
```

| バインド元 | やり方 |
|---|---|
| フォームの submit | submit から transform、API へ追う |
| テーブル検索 | `getFieldsValue()` から `params` |
| ルート | `:id`、`?tab=` |
| インターセプタ | 全体の `tenantId`、ページング、sign |
| 列挙の select | `options` から API の列挙値 |

DevTools の call stack で、`fetch` と `XHR.send` から上へ組み立て関数を追います。

#### 1b.3 組み立ての 3 つの問い（Phase 2 の認証 3 門とは別）

| 問い | 答えること |
|---|---|
| **組み立て** | payload をどこで build するか、transform の痕跡 |
| **検証** | required、pattern、enum |
| **伝送** | path、query、body、multipart、ヘッダ |

インターセプタの門（Phase 2）では、ついでに全体注入フィールド（Authorization、`X-Tenant-Id`、sign）を読みます。

#### 1b.4 Phase 3 との接続

候補フィールドは静的層とバインド層から来ます。**必須、任意、条件付き依存**は、Phase 3 のパラメータ行列、diff、Phase 5 のエラーからの逆算が必要です。

---

## Phase 2 認証の 3 つの門

`OUTDIR/js/` を grep し（`head` 付き）、`config.json` へ書きます（レシピは reference）。

| 門 | 問い | キーワード |
|---|---|---|
| **描画の門** | ログイン済みをどう判定するか | `isLogin`、`getToken`、Cookie、localStorage |
| **インターセプタの門** | 何が `/login` への遷移を起こすか | `response_code`、`errno`、axios interceptor |
| **内容の門** | メニューと権限はどこから来るか | `menu`、`permission`、`role`、`acl`、`routes` |

localStorage のキー名を資格情報だとみなしてはなりません。chunk とリクエスト連鎖で確認する必要があります。

**出口は門 B です**。結論を `config.json` に落とし、`OUTDIR/runtime_harvest.js` と `preload.js` を変えます。

### Phase 2b API の観察（任意）

OUTDIR 内の `preload.js` で、セッションのキー名、Authorization、入れ子の API URL を確認します。

| 設定 | 成果 |
|---|---|
| `recordDetail: true` | `__API_RECON_DETAIL__` |
| `observe.xhrHeaders: true` | headers の観察 |
| `extractUrlsFromResponse: true` | レスポンス内の子 API |
| `observe.storageReads/cookieReads: true` | config への書き戻し |
| `neutralizeVueRouter: true` | `__API_RECON_ROUTES__` |

coverage の各ラウンドで出すもの: `__API_RECON_LOG__`、`__API_RECON_DETAIL__`、`__API_RECON_ROUTES__`、`__API_RECON_OBSERVE__`。

---

## Phase 3 実行時

門 B を通過している必要があります。[境界と禁止](#境界と禁止agent-必読違反は範囲外) と、資格情報の無い mock 方針に従います。

`config.json` に `"runtimeMode": "depth" | "coverage" | "both"` を設定します（テンプレートは reference）。

### Hook と stub（depth と coverage で共通）

| 層 | 範囲 | 目的 |
|---|---|---|
| L1 正確 | auth、権限、bootstrap の stub | 最初の画面の認証を通す |
| L2 負の修正 | すべての JSON レスポンス | 未ログインのコードを成功へ変える |
| L3 フォールバック | L1 に当たらない `/api` など | 空の成功 body で UI を広げる |

- **depth**: fake auth、`forward` で業務コードを変え、`stubs`。`routes`（hash と history）を巡回する。成果は `runtime_api.json`
- **coverage**: **document-start** で `preload.js` を注入する（CDP の `addScriptToEvaluateOnNewDocument`、または Userscript）

確認: `window.__API_RECON_PRELOAD__` があること。業務 path が `/login` へ戻らないこと。

```bash
cd recon && sh ensure-runtime.sh
node runtime_harvest.js config.json
```

### 3b coverage の動的列挙（必須）

1. 主ナビゲーションとサイドバー。各項目をクリックし、ネットワークを 1 から 3 秒待つ
2. Tab。`role=tab`、`.ant-tabs-tab`
3. テーブル。先頭行の表示、編集、詳細
4. ツールバー。エクスポート、絞り込み、新規（**不可逆な削除は避ける**）
5. モジュールに入るたびに API とルートをマージする
6. SPA では、`routes.txt` が覆っていない path へ制御した `pushState` をする（MPA では禁止）

**パラメータのトリガー行列**（必須）: モジュールごとに操作の種類ごとへ 1 回記録し、**複数サンプルを diff** します。

| 操作 | 通常増えるパラメータ |
|---|---|
| 一覧の初回画面 | ページングと既定の絞り込み |
| 検索をクリック | keyword、filter |
| 高度な絞り込み | 追加の optional |
| 新規と編集 | 完全な entity |
| 一括、エクスポート、並び替え | `ids[]`、`exportType`、`sortField` |

**stub の下でも outbound の body と headers は本物です**。リクエストを基準にしてください。記録先は `scan_raw.json`、`param_samples.json`、`api_detail.json` です。

- **Vue**: `neutralizeVueRouter: true` と document-start の preload
- **React**: `routes.txt`、サイドバーのクリック、`pushState`
- **both**: 先に 3a の depth、そのあと 3b の coverage

---

## Phase 4 権限ツリーの復元

**起動条件**: モジュールページが空白、または各ルートが bootstrap（locale など）だけ。内容の門を通っていません。

| 現象 | 意味 |
|---|---|
| 殻への入場は成功 | 描画の門とインターセプタの門は通過済み |
| サイドバーに項目が無い、またはクリックが空白 | stub の形、または権限コードが不足 |
| 各ルートの API が同じで極めて少ない | `v-if permission` を通っていない |
| `routes.txt` が bundle よりはるかに少ない | auth モジュールから補う必要がある |

```bash
grep -rhoaE '"/api[^"]*(permission|perm|role|menu|acl)[^"]*"' OUTDIR/js/*.js | sort -u | head -30
grep -rhoaE 'userRouteAuth|getResultTree|routeMap|routeLink|menuList|authList' OUTDIR/js/*.js | head -20
```

典型的な連鎖: `role_permissions`（flat codes）と `permissions/all`（tree）から `getResultTree`、さらに `userRouteAuth[CODE].url`。

```bash
python3 recon/extract_route_map.py recon/js recon/
python3 recon/build_perm_tree.py recon/js recon/ --config recon/config.json
```

中間成果: `route_map.json`、`userRouteAuth.json`、`permissions_tree.json`、`*_stub.json`、`perm_codes_all.txt`。

stub の確認: 外側の `response_code` がインターセプタの門と一致すること。flat codes と tree が揃うこと。`routes` が `route_map` の link をすべて覆うこと。

`config.json` を更新したあと **Phase 3 を再実行**します。大きい SPA では `waitUntil`、`routeTimeout`、`perRouteMs` を調整できます（reference の A3 と I 節）。

---

## Phase 5 マージとレポート

### 成果表

| ファイル | 段階 | 内容 |
|---|---|---|
| `js/`、`api_static.txt`、`routes.txt`、`chunkmap.txt` | 1 | 静的 bundle と path |
| `param_candidates.json` | 1b | 静的なパラメータフィールドの候補 |
| `config.json` | 2 | 3 つの門と runtime 設定 |
| `runtime_api.json` | 3a | depth の詳細記録（WS と SSE を含む） |
| `param_samples.json`、`scan_raw.json`、`api_detail.json` | 3b | 複数サンプル、クリックログ、detail |
| `route_map.json` など | 4 | 権限ツリーの中間ファイル（実行したとき） |
| `params_merged.json` | 5 | マージしたパラメータフィールドと確信度 |
| `api_merged.txt` | 5 | `METHOD /path [params] [static\|runtime\|both]` |
| `site_map.json` | 5 | ルート、API、params、機能点、限界 |
| **insert_assets** | 5 | すべてのサービスとエンドポイント資産を資産庫へ書く |

### 5b パラメータのマージ

`param_samples.json` を diff します。**汎用のマージスクリプトはありません**。確信度の規則は reference の J7（高、中、低、未トリガー）です。

### 5c エラーからの逆算

許可の範囲で、不完全なリクエストを送って 400 を読めます（**パラメータ recon であり、脆弱性テストではありません**）。`field 'x' is required` や列挙エラーなどです。`data` の包み、`variables`、暗号化前の `bizData` に注意します。

レポートには、runtimeMode、静的 API 数と実行時 API 数、パラメータの確信度、覆っていないモジュール、参考スクリプトに対する `CHANGES.md` の要約を書きます。

`site_map.json` の推奨構造:

```json
{
  "site": "https://example.com",
  "runtimeMode": "both",
  "appType": "vue-spa",
  "routeGuardStrategy": ["nav-neutralize", "L1-auth", "L2-patch", "forward"],
  "apisFromStatic": [],
  "apisFromRuntime": [],
  "apis": [],
  "params": [{ "method": "POST", "path": "/api/user/list", "transport": "json", "fields": [] }],
  "frontendRoutes": [],
  "routesVerifiedByClick": [],
  "featuresTriggered": [],
  "limitations": ""
}
```

追加のフィールドと grep のレシピは [reference.md](reference.md) です。

---

## 共通の説明

- **フレームワークに依存しません**: webpack、Vite、Angular の lazy load は同じ方法です
- **伝送**: REST と JSON、GraphQL、WebSocket、SSE。gRPC-web は範囲外です
- **SSR**: クライアントの fetch は記録できます。RSC と Server Actions は完全には列挙できません
- **死角**: JSVMP、WASM、HMAC と mTLS の強い検証。静的解析と限界の明記に戻ります
- **パラメータの死角**: 条件連動、hidden params、WASM の組み立て。「未トリガー」または「到達不能」とします
- **静的は安全網です**: runtime が止められても、静的解析は endpoint を列挙できます

---

## 追加の資料

- Grep のレシピ、`config.json` のテンプレート、切り分け、Hook、パラメータ逆解析の J 節、site_map のテンプレート: **[reference.md](reference.md)**
- 参考スクリプトのパスは [スクリプトと門](#スクリプトと門) の表です


### Runtime dependency setup

Copy `ensure-runtime.sh`, `package.json`, `package-lock.json`, and `.npmrc` with the runtime scripts. In the built image, `ensure-runtime.sh` verifies the absolute `ARTEX_PUPPETEER_PATH` and fails if it is unavailable; it does not fall back to a network install. Outside the image, the helper explicitly installs from the copied official lockfile with lifecycle scripts disabled.
