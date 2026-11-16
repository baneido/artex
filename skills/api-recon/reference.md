# api-recon の参考

Grep のレシピ、`config.json` のテンプレート、切り分けです。grep はすべて `js/` ディレクトリに対して実行します。bundle が 1 行のときは、先に `js-beautify` するか `sed 's/}/}\n/g'` します。通常は、文脈の窓を付けた raw grep で足ります。

## スクリプトの説明

`scripts/` のファイルはすべて**参考テンプレート**です。実行前に対象サイトへ合わせなければなりません。典型的な変更点です。

| スクリプト | よく調整する項目 |
|---|---|
| `harvest_static.py` | endpoint の正規表現、webpack と Vite の manifest 解析、マイクロフロントエンドの publicPath、再試行と同時実行 |
| `runtime_harvest.js` | neutralize のフィールド名と成功値、stub の一致規則と body の構造、routes の出所、WS の記録、`waitUntil`、`routeTimeout`、`proxy` |
| `preload.js` | `loginPathRe`、L1 stubs、`neutralize.fields`、`apiPattern`、L3 を有効にするか、`recordDetail`、`observe.*`、`neutralizeVueRouter` |
| `spider_mpa.py` | `--exclude` の破壊的リンク、cookie、depth と max、同一オリジンの絞り込み |
| `extract_route_map.py` | `routeMap` と `routeLink` の正規表現、KEY の命名パターン |
| `build_perm_tree.py` | `userRouteAuth` の解析、`ROOTS` と `PREFIX_PARENT` の階層ヒューリスティック、stub の外側フィールド名 |
| `config.json` | 上の、サイト固有パラメータすべての統一入口 |

調整したファイルはタスクの作業ディレクトリ（例: `recon/`）へ置くことを勧めます。レポートには、参考スクリプトに対する具体的な変更を書きます。

---

## A. 逆解析する 3 つの門

### A1. 描画の門。「ログイン済みをどう判定するか」

```bash
grep -rhoaE '.{0,40}(isLogin|isAuthenticated|loggedIn|hasLogin|requireAuth)\b.{0,80}' js | head
grep -rhoaE 'function (getUser|getToken|getAuth)[0-9]?\([^)]*\)\{.{0,200}' js | head
grep -rhoaE '(localStorage|sessionStorage)\.getItem\("[^"]+"\)' js | sort -u
grep -rhoaE '(Cookies?|cookie)\.(get|load)\("[^"]+"\)' js | sort -u
grep -rhoaE '\batob\(|JSON\.parse\(|jwt|decode' js | head
```

連鎖 `isLogin = f(getUser())` から `getUser = decode(storage.read(KEY))` を探し、**保存キー**、**入れ物**（Cookie か localStorage か）、**エンコード**を決めます。

| エンコード | config での偽造の仕方 |
|---|---|
| 平文の文字列、`"1"`、token | `"value": "anything-truthy"` |
| `JSON.parse(x)` | `"value": "json:{\"id\":1,\"username\":\"admin\"}"` |
| `JSON.parse(atob(x))` | `"value": "b64json:{\"id\":1,\"username\":\"admin\"}"` |
| JWT | 署名無し、または `alg:none` の JWT。あるいは bundle 内の鍵での署名 |
| 暗号化（SM2、AES、RSA） | ハードコードされた鍵を探す。描画の門は、デコードできる blob があれば forge できる。そうでなければ静的なフォールバック |

→ `cookies` と `localStorage` へ書きます。

### A2. インターセプタの門。「何が /login への遷移を起こすか」

```bash
grep -rhoaE '.{0,60}(interceptors\.response|axios|request\.use).{0,120}' js | head
grep -rhoaE '.{0,40}(response_code|errcode|errno|\bcode\b|\bret\b|\bstatus\b)\s*[=!]==?\s*[\-0-9]{1,4}.{0,60}' js | head -20
grep -rhoaE '.{0,40}(未登录|请重新登录|登录已过期|unauthorized|登录失效|授权|token.{0,10}invalid).{0,40}' js | head
grep -rhoaE '.{0,30}(location\.href|router\.(push|replace)|navigate)\([^)]*login[^)]*\)' js | head
```

**フィールド名**、**成功値**（通常は `0` か `200`）、**遷移を起こす失敗値**を決めます。junk session で確認します。

```bash
curl -sk -X POST -H 'Cookie: <fakekey>=junk' https://target/api/<protected> -d '{}' -H 'Content-Type: application/json'
```

→ `neutralize.fields` と `neutralize.success` へ書きます。

### A3. 内容の門。「メニューと権限はどこから来るか」

```bash
grep -rhoaE '"/api[^"]*(permission|perm|role|menu|acl|resource|nav)[^"]*"' js | sort -u
grep -rhoaE '.{0,30}(menus|permissions|menuList|routeList|authList|role_permissions)\b.{0,120}' js | head
grep -rhoaE 'userRouteAuth|getResultTree|routeMap|routeLink|hasPermission|checkAuth' js | head
grep -rhoaE '([A-Z_][A-Z0-9_]*):\{name:"[^"]*",link:"/[^"]+"\}' js | head
```

**2 層のデータ**（企業の管理画面でよくある形）:

| API | 典型的な payload | 消費側 |
|---|---|---|
| `.../role_permissions` | `{ permissions: string[], role_type }` | ルートガード、ボタン単位の ACL |
| `.../permissions/all` | `tree[{ code, position, children }]` | サイドバーメニューの描画 |
| bundle 内の `userRouteAuth` | `{ CODE: { url, name? } }` | code からフロントエンド path |
| bundle 内の `routeMap` | `{ KEY: { name, link } }` | 別名の解決（webpack の `o.DASHBOARD`） |

消費側のコードを読み、`getResultTree(tree, permissions)` がどう絞るか、`v-if` と `hasAuth(code)` がどのフィールドを見るかを確認します。

**手作業の forge**（小さいサイト）: 許可の広い payload を組み、`stubs` へ入れます。

**権限ツリーの完全な復元**（大きいサイトで、サイドバーや子モジュールがまだ空白）: **I 節**を見てください。

---

## B. config.json のテンプレート

```json
{
  "baseUrl": "https://target/",
  "runtimeMode": "both",
  "chromium": "/usr/bin/chromium",

  "cookies": [
    { "name": "auth", "value": "b64json:{\"id\":1,\"username\":\"admin\",\"role\":\"admin\",\"func\":{},\"permissions\":[\"*\"]}" }
  ],
  "localStorage": { "token": "faketoken", "isLogin": "1" },

  "neutralize": {
    "fields": ["response_code", "code", "errno", "ret", "status"],
    "success": 0,
    "flags": { "success": true, "message": "ok" }
  },
  "forward": true,
  "loginUrlPattern": "/login",
  "apiPattern": "/api/|/rest/|/graphql",

  "mockTier": "L1+L2",
  "recordDetail": true,
  "observe": {
    "storageReads": false,
    "cookieReads": false,
    "xhrHeaders": true
  },
  "neutralizeVueRouter": true,
  "stubs": [
    {
      "match": "permissions/all|/menu|role_permissions",
      "body": {
        "response_code": 0, "code": 0,
        "data": {
          "permissions": ["*"],
          "menus": [
            { "name": "dashboard", "path": "/dashboard", "show": true, "children": [] },
            { "name": "alert", "path": "/alert", "show": true, "children": [] }
          ]
        }
      }
    }
  ],

  "explore": {
    "clickTabs": true,
    "clickTables": true,
    "pushStateFallback": true,
    "maxMenuItems": 50
  },

  "routes": ["/dashboard", "/alert", "/asset", "/device", "/report", "/config", "/system"],
  "waitMs": 1500, "perRouteMs": 900, "headless": true,
  "waitUntil": "domcontentloaded",
  "routeTimeout": 12000,
  "proxy": "",

  "captureResponses": true, "recordWs": true, "respMax": 600
}
```

フィールドの説明です。
- `runtimeMode`: `depth`（Puppeteer）、`coverage`（browser MCP）、`both`
- `cookies[].value` の接頭辞: `b64json:` は base64(JSON)。`json:` は生の JSON。接頭辞が無ければリテラル
- `forward: true` は実リクエストを転送し、コードフィールドを書き換える。`false` は完全にオフラインの stub
- `mockTier`: coverage モードの preload が有効にする層。例は `L1+L2`、`L1+L2+L3`
- `routes` は `routes.txt` から来る。メニューを forge したあと、harness が `<a href>` を自動で足す
- `captureResponses` と `recordWs` は depth モードだけで有効
- `waitUntil`: 大きい SPA は `domcontentloaded` を使い、`networkidle2` で止まらないようにする
- `routeTimeout`: 1 ルートの `page.goto` のタイムアウト（ミリ秒）
- `proxy`: Puppeteer の `--proxy-server`。`HTTP_PROXY` と `HTTPS_PROXY` も設定できる

### B1. 2 つの stub のテンプレート（role_permissions と permissions/all）

```json
"stubs": [
  {
    "match": "role_permissions",
    "body": {
      "response_code": 0,
      "data": {
        "permissions": ["MONITOR", "MONITOR_ALERT", "THREAT", "ASSETS_RISK"],
        "role_type": "SUPER_ADMIN"
      }
    }
  },
  {
    "match": "permissions/all",
    "body": {
      "response_code": 0,
      "data": [
        {
          "code": "MONITOR",
          "position": 1,
          "children": [
            { "code": "MONITOR_ALERT", "position": 1, "children": [] }
          ]
        }
      ]
    }
  }
]
```

外側のフィールド名（`response_code`、`code`、`data`）は A2 のインターセプタの門と一致しなければなりません。`permissions` は tree のすべての leaf code を覆わなければなりません。

---

## C. coverage モードの preload 設定

`scripts/preload.js` 先頭の `CONFIG` オブジェクトを編集するか、CDP 注入の前に置き換えます。

```javascript
const CONFIG = {
  loginPathRe: /\/(login|signin)(\/|$|\?)/i,
  mockTier: 'L1+L2',
  forward: true,
  recordDetail: true,
  extractUrlsFromResponse: true,
  neutralizeVueRouter: true,
  observe: { storageReads: false, cookieReads: false, xhrHeaders: true },
  neutralize: { fields: ['response_code', 'code'], success: 0 },
  stubs: [ /* config.json の stubs と同じ */ ],
  apiPattern: /\/(api|apis|v\d+|dev|internal|graphql)\//i,
};
```

確認: `window.__API_RECON_PRELOAD__ === true` で、pathname が安定していること。

記録結果の取り出し:

```javascript
JSON.stringify({
  apis: [...window.__API_RECON_LOG__],
  detail: window.__API_RECON_DETAIL__,
  routes: [...(window.__API_RECON_ROUTES__ || [])],
  observe: window.__API_RECON_OBSERVE__,
}, null, 2)
```

---

## D. preload と runtime の Hook 能力

preload（coverage）と runtime_harvest（depth）に組み込まれたブラウザ Hook の能力と、覆う範囲です。

| Hook の能力 | API 発見への価値 | 被覆 |
|---|---|---|
| fetch と XHR.open を Hook | リクエスト URL とメソッドを記録する | ✅ `recordDetail` と `__API_RECON_LOG__` |
| XHR.setRequestHeader を Hook | Authorization などのヘッダを見つける | ✅ `observe.xhrHeaders` |
| localStorage と cookie の読み取りを Hook | セッションのキー名を確認する | ⚠️ 任意の `observe.storageReads/cookieReads` |
| Vue からルートを取る | frontendRoutes を補う | ✅ `__API_RECON_ROUTES__`（読み込み済みのルート） |
| Vue のルートガードの無効化と、ログイン遷移の遮断 | モジュールを広げて API を起こす | ✅ `neutralizeVueRouter` と、ネイティブ遷移の無効化 |
| React からルートを取る | ルートを補う | ⚠️ 静的とクリック。専用 Hook は無い |
| ページ遷移の遮断（ログイン path） | ページに残って解析する | ⚠️ ログイン path だけを遮断し、業務のナビゲーションは止めない |
| 暗号ライブラリを Hook（CryptoJS、SM など） | 暗号パラメータから平文の API body へ | ❌ 暗号関数の引数を手で Hook する必要がある。結論は config へ書く |
| アンチデバッグの bypass | 無いと runtime は API を記録できない | ❌ 手で扱う必要がある。静的解析はまだ使える |

---

## E. Endpoint 抽出の正規表現（静的が少なすぎるとき）

`harvest_static.py` の `extract_endpoints` を緩めるか、手で実行します。

```bash
grep -rhoaE '"/[a-z][A-Za-z0-9_/\-]{3,}"' js | sort -u
grep -rhoaE '/api/[a-zA-Z0-9_./-]+' js | sort -u
```

---

## F. 切り分け

| 現象 | 原因と対処 |
|---|---|
| 静的 API がとても少ない | endpoint の方言が合わない。正規表現を緩める（D 節） |
| chunk 数が manifest よりはるかに少ない | CSS だけ、または未デプロイの chunk。404 は再試行済み |
| runtime がまだログインページを出す | 描画の門が誤っている。A1 を見直す。キー名、入れ物、エンコード、domain |
| 殻には入ったがモジュールが空白 | 内容の門。メニューを forge する（A3）。`routes` の path が違うことがある |
| 各ルートが bootstrap か locale だけ | 権限コードが不足。I 節の権限ツリー復元。`role_permissions` と `permissions/all` の 2 stub を確認する |
| サイドバーに項目はあるが子ページが空白 | tree に中間ノードが無い、または code が `userRouteAuth` と一致しない |
| どの API もログインへ飛ぶ | インターセプタの門。`neutralize` を確認する。入れ子フィールドは walk を広げる必要がある |
| WS フレームが 0 | ユーザー操作のあとで subscribe する。`perRouteMs` を延ばす |
| レスポンス本文が空 | 実レスポンスがあるのは `forward: true` のときだけ |
| Chromium が無い | chromium を入れるか、`config.chromium` か `CHROMIUM` を設定する |
| Mock が多いのにログインへ戻る | Hook が遅いか、`location.href` の setter が無い。document-start と preload |
| 一覧がすべて空 | L3 の空配列は正常。Tab、設定、詳細を続けてクリックする |
| Redux action をルートと誤解した | get、set、change、clear、toggle、upload を含む内部 path を除く |
| Vue がまだログインへ飛ぶ | preload が document-start ではない。注入のタイミングを変える。または `neutralizeVueRouter: false` のときはガードを手で外す |
| レスポンスに URL があるのに log に入らない | `extractUrlsFromResponse` をオンにする。または `__API_RECON_DETAIL__` から手で取る |
| Authorization のヘッダ名が分からない | `observe.xhrHeaders` をオンにするか、DevTools でリクエストヘッダを見る |
| runtime が極端に遅い、またはタイムアウト | `waitUntil: domcontentloaded` に変える。`routeTimeout` を下げる。`networkidle2` は使わない |
| プロキシ接続に失敗 | `proxy` と環境変数を確認する。Puppeteer と curl のプロキシポートを揃える |

---

## G. 堅い目標

サーバーがセッションを段階的に検証するとき（偽造できない署名 cookie、サーバー描画で stub できないメニュー）、runtime は殻で止まります。想定する振る舞いです。

- **静的解析は endpoint の列挙に足ります**。モジュールの path はコードの中にあります
- 許可が許すなら、**実セッション**で同じ harness を走らせます。`forward: true`、neutralize は不要で、実メソッド、params、responses を捕捉します

---

## H. 1 回のタスクの一覧

1. 許可の範囲を確認する
2. `scripts/harvest_static.py` を**読み**、目標に合わせて調整し、実行し、`api_static.txt` と `routes.txt` を見る
3. **Phase 1b**: path アンカーからの窓拡大とバインド層 → `param_candidates.json`（J 節）
4. A1、A2、A3 を逆解析し、サイト固有の `config.json` を書く
5. `runtime_harvest.js` と `preload.js` を**読んで調整してから**実行する
6. `runtimeMode=depth`: `npm install` のあと、調整した harvest スクリプトを実行する
7. `runtimeMode=coverage/both`: document-start で調整した preload を注入し、browser MCP で動的列挙と**パラメータのトリガー行列**を行う
8. モジュールが描画されない → **I 節の権限ツリー復元** → stub を patch → 再実行
9. パラメータの複数サンプル diff とエラーからの逆算 → `params_merged.json`
10. マージして `site_map.json` と `api_merged.txt` にし、被覆、欠落、スクリプトの変更点を正直に書く

---

## I. 権限ツリーの復元（Phase 4 を深める）

単純な `menus: [{ path, show: true }]` の forge が効かず、子モジュールがまだ mount しないときに使います。

### I1. auth モジュールを見つける

```bash
grep -l 'userRouteAuth' js/*.js
grep -l 'routeMap\|routeLink' js/*.js
grep -rhoaE 'getResultTree|role_permissions|permissions/all' js | head
```

記録するもの: **権限 API の path**、**レスポンスのフィールド名**、**消費する chunk のファイル名**。

### I2. routeMap を取り出す

```bash
python3 scripts/extract_route_map.py recon/js recon/
# 成果 recon/route_map.json
```

`[!] no routeMap pattern found` のときは、`extract_route_map.py` の正規表現を緩めるか、手で grep します。

```bash
grep -rhoaE '([A-Z_][A-Z0-9_]*):\{name:"[^"]*",link:"/[^"]+"\}' js | head -20
```

### I3. 権限ツリーと stub を作る

```bash
python3 scripts/build_perm_tree.py recon/js recon/ --config recon/config.json
```

スクリプトの論理です。
1. `userRouteAuth={MONITOR:{url:...},...}` を解析する（webpack 別名 `He=o.DASHBOARD` を含む）
2. `route_map.json` で alias を実 path に解決する
3. code の接頭辞から parent を推定する（`MONITOR_ALERT` から `MONITOR`）
4. `permissions_tree.json`、`permissions_all_stub.json`、`role_permissions_stub.json` を出す
5. `--config` のとき、`config.json` の `stubs` と拡張した `routes` を自動で書く

**目標に合わせて調整する**（スクリプト先頭）:
- `DEFAULT_ROOTS`: 最上位モジュールの code 一覧
- `DEFAULT_PREFIX_PARENT`: `PREFIX_` から parent への対応
- `DEFAULT_EXTRA_PARENT`: 接頭辞関係ではない orphan ノード

### I4. stub の一致を検証する

```bash
# permissions の数は userRouteAuth の件数にほぼ等しいはず
wc -l recon/perm_codes_all.txt
# routes は route_map の link をすべて覆うはず
python3 -c "import json; m=json.load(open('recon/route_map.json')); r=set(json.load(open('recon/config.json'))['routes']); print('missing', [v['link'] for v in m.values() if v['link'] not in r])"
```

### I5. runtime を再実行して比べる

```bash
node recon/runtime_harvest.js recon/config.json
# forge の前後で runtime_api.json の件数を比べる。/attack や /asset などにモジュール API が出るかを見る
```

| forge の前 | forge のあと（成功） |
|---|---|
| 各ルートが同じ 3 から 5 件の bootstrap | ルートごとに違う module API が起こる |
| `/api/locale/language` だけ | `/api/web/...` のモジュール endpoint が出る |
| `routes.txt` のルートは一桁 | `routes` は route_map から 80 から 110 以上 |

### I6. まだ失敗するとき

- **coverage モード**: サイドバーと Tab をクリックする。権限の gating は操作のあとでリクエストすることがある
- **stub のフィールド**: 実 API（curl と実 session）と stub の入れ子を比べる
- **追加のガード**: `hasPermission|checkRole|func.` などのボタン単位チェックを grep し、`role_permissions.permissions` を広げる
- **静的なフォールバック**: モジュール API の path は `api_static.txt` に残る。runtime が補うのは METHOD と body だけ。パラメータは `param_candidates.json` と、記録済みサンプルを残す

---

## J. パラメータの逆解析（Phase 1b、5b、5c）

**方法であり、汎用スクリプトではありません。** path は正規表現で探し、パラメータはアンカーからの窓拡大、UI のバインド連鎖、複数サンプルの diff、エラーからの逆算で探します。

### J1. アンカーからの窓拡大。path から組み立てオブジェクトを探す

```bash
# Phase 1 で既知の path をアンカーにする
grep -n '"/api/user/list"' js/*.js
grep -rhoaE '.{0,120}("/api[^"]+").{0,200}' js | head
grep -rhoaE '(params|data|body|payload)\s*:\s*\{' js | head
grep -rhoaE '(get|post|put|delete|patch)\([^,]+,\s*\{' js | head
```

### J2. 包みの層と伝送の形

```bash
# axios と統一 request
grep -rhoaE '(axios|request)\.(get|post|put|delete|patch)\(' js | head
grep -rhoaE 'interceptors\.(request|response)' js | head

# GraphQL
grep -rhoaE '(query|mutation)\s+\w+|gql`|graphql\(' js | head
grep -rhoaE '\$[a-zA-Z_]+\s*:\s*(Int|String|Boolean|\[)' js | head

# FormData と multipart
grep -rhoaE 'FormData|\.append\(' js | head

# パスパラメータ
grep -rhoaE 'path:\s*"/[^"]*:[^"]+"' js | head
grep -rhoaE 'useParams|route\.params|\$route\.params' js | head
```

### J3. 検証の門。必須、形式、列挙

```bash
grep -rhoaE '(required|message|pattern|enum|validator)\s*:' js | head
grep -rhoaE 'yup\.|zod\.|async-validator|Form\.Item|a-form-item|el-form-item' js | head
grep -rhoaE 'rules\s*:\s*\[|name:\s*["\'][a-zA-Z_]+["\']' js | head
grep -rhoaE 'label.*value|options\s*:\s*\[' js | head
```

### J4. バインド層。フォームから API へ

```bash
grep -rhoaE 'onFinish|handleSubmit|getFieldsValue|validateFields' js | head
grep -rhoaE '(pick|omit|transform|dayjs|moment)\(' js | head
```

runtime での補完: DevTools、Network、リクエスト、**開始元**（call stack）で、`fetch` と `send` から上へ組み立て関数を追います。

### J5. 暗号パラメータ

```bash
grep -rhoaE 'encrypt|decrypt|sign|CryptoJS|sm2|sm3|sm4|RSA|AES' js | head
```

**暗号文の上でフィールドを推測しないでください。** 暗号関数の**引数**を Hook し、暗号化する前に平文 payload を記録します。結論は `config.json` と `param_candidates.json` へ書きます。

### J6. パラメータのトリガー行列（Phase 3 で必須）

モジュールごとに操作ごとへ 1 回記録し、リクエストの body と query を diff します。

| 操作 | 見るもの |
|---|---|
| 一覧の初回画面 | ページングの既定値 |
| 検索 | keyword、filters |
| 高度な絞り込み | optional フィールド |
| 新規と編集 | 完全な entity |
| 一括とエクスポート | `ids[]`、`exportType` |
| 並び替えとページ移動 | `sortField`、`order` |

成果 `param_samples.json`: `[{ "path", "method", "action": "search", "body", "query", "headers" }]`

### J7. 確信度の規則

| 確信度 | 条件 |
|---|---|
| **高** | 静的 callsite があり、runtime のサンプルが 2 件以上一致する |
| **中** | 静的だけ、または runtime が 1 回だけ |
| **低** | レスポンスやエラーからの逆算で、再検証していない |
| **未トリガー** | 静的にはフィールドが既知だが、UI や権限がそこまで走っていない |

### J8. 場面ごとの短い手順

| 場面 | 順序 |
|---|---|
| REST の一覧ページ | J1 の組み立てオブジェクト → J6 の 4 回の diff → J3 の rules |
| 新規と編集のフォーム | J3 の Form name → J4 の submit 連鎖 → runtime で送信し、意図的に空にして 400 を見る |
| GraphQL | J2 の variables 宣言 → runtime で各 operation の variables を記録する |
| 暗号 body | J5 で引数を Hook → 暗号化前のフィールドが実 params |

### J9. api-recon の段階との対応

| api-recon | パラメータ recon |
|---|---|
| Phase 1 静的 | J1 アンカーからの窓拡大 |
| Phase 2 A2 インターセプタ | 全体注入フィールド（tenantId、sign） |
| Phase 3 runtime | J6 のトリガー行列と `param_samples.json` |
| Phase 4 権限ツリー | モジュールごとにフォームが違う。権限が足りて初めて全フィールドが起こる |
| Phase 5 マージ | `params_merged.json` と確信度。単一サンプルで必須を決めない |

### J10. 切り分け

| 現象 | 対処 |
|---|---|
| 静的にはフィールド名があるが runtime に一度も出ない | 「未トリガー」と書く。権限ツリーを補う、高度な絞り込みをクリックする、連動 select の各 option を試す |
| 同じ path で body の形が違う | 正常。`action` ごとに分けて記録し、schema を無理にマージしない |
| stub の応答は偽物だが params を見たい | **outbound リクエスト**の body と headers を見る。stub の応答から逆算しない |
| 400 が nested field を言う | 外側の包み `data`、`bizData`、`variables` に注意する |
| GraphQL で operation 名しか見えない | `variables` の JSON を広げる。静的側で `$var: Type` を探す |

---
