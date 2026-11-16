package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// assetInterceptCandidates は、挿入予定の資産入力 1 件から、ドメイン、IP、URL の候補文字列を取り、資産インターセプトの照合に使う。
// URL の host は分けて分類する。「URL だけ」のサービスやエンドポイントの資産も、ドメインや IP のルールに当たるようにする。
func assetInterceptCandidates(item assetInputItem) (domains, ips, urls []string) {
	add := func(dst *[]string, s string) {
		if s = strings.TrimSpace(s); s != "" {
			*dst = append(*dst, s)
		}
	}
	add(&domains, item.Domain)
	for _, d := range item.BoundDomains {
		add(&domains, d)
	}
	add(&ips, item.IP)
	add(&ips, item.ServiceIP)
	add(&urls, item.URL)
	if item.URL != "" {
		if u, err := url.Parse(item.URL); err == nil {
			if h := u.Hostname(); h != "" {
				if net.ParseIP(h) != nil {
					add(&ips, h)
				} else {
					add(&domains, h)
				}
			}
		}
	}
	return domains, ips, urls
}

// assetInputLabel は、挿入予定の資産 1 件の短い識別子を返す。インターセプトの説明メッセージに使う。
func assetInputLabel(item assetInputItem) string {
	typ := strings.TrimSpace(item.Type)
	var target string
	switch {
	case strings.TrimSpace(item.Domain) != "":
		target = strings.TrimSpace(item.Domain)
	case strings.TrimSpace(item.URL) != "":
		target = strings.TrimSpace(item.URL)
	case strings.TrimSpace(item.IP) != "":
		target = strings.TrimSpace(item.IP)
	case strings.TrimSpace(item.ServiceIP) != "":
		target = strings.TrimSpace(item.ServiceIP)
	default:
		target = "(不明)"
	}
	if typ != "" {
		return fmt.Sprintf("[%s] %s", typ, target)
	}
	return target
}

// =====================================================================
// Unified asset insertion tools
// =====================================================================

// SetAssetStore wires the asset store and company store onto this ToolSet
// so the insert_assets, add_company_scope, and list_assets tools are active.
func (t *ToolSet) SetAssetStore(as *db.AssetStore, cs *db.CompanyStore) {
	t.as = as
	t.cs = cs
}

// assetInputItem is one element of the insert_assets "assets" array.
type assetInputItem struct {
	Type string `json:"type"` // root_domain|ip|subdomain|app|service|endpoint

	// ---- root_domain / subdomain ----
	Domain      string   `json:"domain"`
	ICP         string   `json:"icp"`
	RecordType  string   `json:"record_type"`
	RecordValue []string `json:"record_value"`

	// ---- ip ----
	IP           string           `json:"ip"`
	BoundDomains []string         `json:"bound_domains"`
	OpenPorts    []db.PortService `json:"open_ports"`

	// ---- app ----
	AppName     string `json:"app_name"`
	BundleID    string `json:"bundle_id"`
	Category    string `json:"category"`
	Description string `json:"description"`
	AppICP      string `json:"app_icp"`
	CompanyID   *int64 `json:"company_id"` // explicit company link (app only; others auto-attribute via scope)

	// ---- service (http) ----
	URL           string           `json:"url"`
	Technologies  []string         `json:"technologies"`
	StatusCode    *int             `json:"status_code"`
	ContentLength *int64           `json:"content_length"`
	PageTitle     string           `json:"page_title"`
	FaviconMMH3   string           `json:"favicon_mmh3"`
	Auth          []map[string]any `json:"auth"`
	ServiceName   string           `json:"service_name"`
	ServiceIP     string           `json:"service_ip"` // optional enrichment IP

	// ---- service (other) ----
	Port  int    `json:"port"`
	Proto string `json:"proto"`

	// ---- endpoint ----
	Method string           `json:"method"`
	Params []map[string]any `json:"params"`
}

// insertAssets is the unified insert_assets agent tool.
func (t *ToolSet) insertAssets() actool.CoreTool {
	return writeTool(
		"insert_assets",
		"新しく見つけた資産をまとめて登録します。1 回で複数の種類を混在させられます（type は列挙値を参照）。\n種類ごとの必須フィールド：root_domain→domain；ip→ip（IPv4/IPv6 であること。ホスト名は不可）；subdomain→domain；app→app_name；service(HTTP)→url；service(非 HTTP)→service_name+port（ip/domain のどちらかは必須）；endpoint→url+method。その他のフィールドの意味は、それぞれの説明を参照してください。\nauth/technologies/params は追記して結合(append)し、元の値を上書きしません。\n戻り値：{results:[{index,id,type}], errors:[{index,error}]}",
		obj(map[string]any{
			// task_id はモデルへ出さない。worker がどの task に属するかは、プログラムが SetTaskID で権威を持って入れる（handler を参照）。
			"assets": map[string]any{
				"type":        "array",
				"description": "資産の配列。各要素が 1 件の資産記録に対応します",
				"items": obj(map[string]any{
					"type": map[string]any{
						"type":        "string",
						"enum":        []string{"root_domain", "ip", "subdomain", "app", "service", "endpoint"},
						"description": "資産の種類",
					},
					// root_domain / subdomain
					"domain":      str("ルートドメインまたはサブドメイン（root_domain/subdomain では必須）"),
					"icp":         str("ICP 登録番号（任意）"),
					"record_type": str("DNS のレコード種別：A/AAAA/CNAME/MX など（subdomain では任意）"),
					"record_value": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "DNS の解決値の一覧（subdomain では任意。例 [\"1.2.3.4\",\"2.3.4.5\"]）",
					},
					// ip
					"ip": str("IP アドレス。IPv4/IPv6 アドレスでなければならず、ホスト名は不可です（ホスト名は type=subdomain の domain フィールドを使用）。ip 種別では必須。service/endpoint 種別でも入力でき、IP との関連付けに使います"),
					"bound_domains": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "この IP に紐付いたドメインの一覧（ip 種別では任意）",
					},
					"open_ports": map[string]any{
						"type":        "array",
						"description": "開放ポートの一覧（ip 種別では任意）",
						"items": obj(map[string]any{
							"port":    intp("ポート番号"),
							"service": str("サービス名。例 http/ssh/mysql など（任意）"),
						}, "port"),
					},
					// app
					"app_name":    str("アプリ名（app 種別では必須）"),
					"bundle_id":   str("Bundle ID（app 種別では任意）"),
					"category":    str("アプリのカテゴリ（任意）"),
					"description": str("アプリの説明（任意）"),
					"app_icp":     str("アプリの ICP 登録（任意）"),
					"company_id":  intp("所属企業 id（app 種別では任意。app は scope からの自動帰属ができないため、明示的に指定します。id は add_company_scope が返します）"),
					// service (http)
					"url":         str("プロトコルとポートを含む完全な URL（HTTP サービスでは必須。service_type は自動で http になります）"),
					"status_code": intp("HTTP レスポンスのステータスコード。例 200/301/403/404（任意）"),
					"content_length": map[string]any{
						"type":        "integer",
						"description": "HTTP レスポンスボディのバイト数（任意）",
					},
					"page_title":   str("ページの <title> の内容（任意）"),
					"favicon_mmh3": str("favicon の MMH3 ハッシュ（任意）"),
					"technologies": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "フィンガープリント/技術スタックの一覧。例 [\"Nginx\",\"Vue\",\"Bootstrap\"]（任意）",
					},
					"auth": map[string]any{
						"type":        "array",
						"description": "見つかった認証情報の一覧。各項目に type/username/password などのフィールドを含みます（任意。追記し、上書きしません）",
						"items":       map[string]any{"type": "object"},
					},
					// service (other、HTTP 以外)
					"service_name": str("サービス名。例 ssh/mysql/redis（service が非 HTTP のとき必須）"),
					"port":         intp("ポート番号（service が非 HTTP のとき必須）"),
					// endpoint
					"method": str("HTTP メソッド：GET/POST/PUT/PATCH/DELETE など（endpoint では必須）"),
					"params": map[string]any{
						"type":        "array",
						"description": "リクエストパラメータの一覧。各項目に location(query/body/header/path)/name/value/type を含みます（任意。追記し、上書きしません）",
						"items":       map[string]any{"type": "object"},
					},
				}, "type"),
			},
		}, "assets"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.as == nil {
				return actool.Errorf("insert_assets は使えません。AssetStore が初期化されていません"), nil
			}
			var a struct {
				Assets []assetInputItem `json:"assets"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf("invalid input: " + err.Error()), nil
			}
			// task_id はプログラムが権威を持って入れる（worker は SetTaskID）。モデルからの入力は受けない。渡し忘れや渡し間違いで
			// 資産がタスクに属さない、または別タスクに属することを避ける。タスクコンテキストのない呼び出し元（auto、pentest、chat）は t.taskID=0。
			taskID := t.taskID

			type result struct {
				Index int    `json:"index"`
				ID    int64  `json:"id"`
				Type  string `json:"type"`
			}
			type errEntry struct {
				Index int    `json:"index"`
				Error string `json:"error"`
			}

			var results []result
			var errs []errEntry

			// 資産ゲートのルールは一度に読み込む。読み取りに失敗したら判定を飛ばす（挿入は止めない）。
			// インターセプトルールは全体とタスク単位の block の和。許可ルールはタスク単位の allow。
			blockRules, _ := t.as.ListAssetInterceptRules()
			var allowRules []db.AssetInterceptRule
			if t.taskID > 0 {
				if tb, ta, err := t.as.TaskInterceptRulesSplit(t.taskID); err == nil {
					blockRules = append(blockRules, tb...)
					allowRules = ta
				}
			}

			for i, item := range a.Assets {
				// 資産ゲート。先にインターセプトし、そのあと許可する。拒否された資産は挿入しない（Upsert と、その後の副作用を飛ばす）。
				domains, ips, urls := assetInterceptCandidates(item)
				if d := db.EvaluateAssetGate(blockRules, allowRules, domains, ips, urls); !d.Allowed {
					errs = append(errs, errEntry{
						Index: i,
						Error: fmt.Sprintf("資産 %s %s。挿入を禁止しました", assetInputLabel(item), d.Reason),
					})
					continue
				}

				typ := strings.TrimSpace(item.Type)
				var id int64
				var err error

				switch typ {
				case "root_domain":
					id, err = t.as.UpsertRootDomain(db.UpsertRootDomainReq{
						Domain: item.Domain,
						ICP:    item.ICP,
						TaskID: taskID,
					})

				case "ip":
					id, err = t.as.UpsertIP(db.UpsertIPReq{
						IP:           item.IP,
						BoundDomains: item.BoundDomains,
						OpenPorts:    item.OpenPorts,
						TaskID:       taskID,
					})

				case "subdomain":
					id, err = t.as.UpsertSubdomain(db.UpsertSubdomainReq{
						Domain:      item.Domain,
						RecordType:  item.RecordType,
						RecordValue: item.RecordValue,
						ICP:         item.ICP,
						TaskID:      taskID,
					})

				case "app":
					id, err = t.as.UpsertApp(db.UpsertAppReq{
						Name:        item.AppName,
						BundleID:    item.BundleID,
						Category:    item.Category,
						Description: item.Description,
						ICP:         item.AppICP,
						CompanyID:   item.CompanyID,
						TaskID:      taskID,
					})

				case "service":
					// distinguish HTTP vs other by presence of url
					if item.URL != "" {
						// agent may send "ip" or "service_ip" for the enrichment IP; accept both
						svcIP := item.ServiceIP
						if svcIP == "" {
							svcIP = item.IP
						}
						id, err = t.as.UpsertHTTPService(db.UpsertHTTPServiceReq{
							URL:           item.URL,
							Technologies:  item.Technologies,
							StatusCode:    item.StatusCode,
							ContentLength: item.ContentLength,
							PageTitle:     item.PageTitle,
							FaviconMMH3:   item.FaviconMMH3,
							Auth:          item.Auth,
							IP:            svcIP,
							TaskID:        taskID,
						})
					} else {
						id, err = t.as.UpsertOtherService(db.UpsertOtherServiceReq{
							Domain:      item.Domain,
							IP:          item.IP,
							Port:        item.Port,
							ServiceName: item.ServiceName,
							Auth:        item.Auth,
							TaskID:      taskID,
						})
					}

				case "endpoint":
					id, err = t.as.UpsertEndpoint(db.UpsertEndpointReq{
						URL:    item.URL,
						Method: item.Method,
						Params: item.Params,
						IP:     item.ServiceIP,
						TaskID: taskID,
					})

				default:
					errs = append(errs, errEntry{Index: i, Error: "unknown type: " + typ})
					continue
				}

				if err != nil {
					errs = append(errs, errEntry{Index: i, Error: err.Error()})
					continue
				}
				results = append(results, result{Index: i, ID: id, Type: typ})
				t.writes.Assets++
				t.anchorOwner(id)
				if taskID > 0 {
					var sourceNodeID *int64
					if t.ownerNode > 0 {
						nodeID := t.ownerNode
						sourceNodeID = &nodeID
					}
					summary := "Agent が insert_assets で登録"
					if t.ownerNode > 0 {
						summary = fmt.Sprintf("Worker のインテント #%d が insert_assets で登録", t.ownerNode)
					}
					_ = t.as.SetTaskAssetSource(taskID, id, "agent", summary, sourceNodeID)
				}
				// テスト範囲へ自動で入れる（source='auto'）。worker がトップレベルで明示的に挿入したこの 1 件だけを、その
				// 種別に応じて保守的な範囲として足す。副作用で派生した資産はここを通らないので、範囲はむやみに広がらない。taskID=0 のときは何もしない。
				// カバレッジのスイッチとは無関係。task_scope はタスクの範囲の境界（list や検索の絞り込みの基準）である。
				// カバレッジのスイッチは、それを分母にして指標を計算するかどうかだけを決め、範囲そのものを積むかどうかは決めない。
				{
					svcIP := item.ServiceIP
					if svcIP == "" {
						svcIP = item.IP
					}
					_ = t.as.AddAutoScope(taskID, typ, item.Domain, item.URL, svcIP)
				}
			}

			return jsonResult(map[string]any{
				"results": results,
				"errors":  errs,
			})
		},
	)
}

// addCompanyScope writes to company_scope table and triggers asset attribution.
func (t *ToolSet) addCompanyScope() actool.CoreTool {
	return writeTool(
		"add_company_scope",
		"ドメイン/IP/CIDR/ICP 登録/企業キーワードを、ある企業の【資産範囲】に追加します。ドメイン、ネットワーク帯、ICP は一致した資産を自動で取り込み、キーワードは範囲のヒントとして Agent に提供されるだけです。\n企業名は一意です：company が存在しなければ新規作成し、存在すれば再利用します(範囲を追加するだけ)。\nscope は 1 行に 1 件で、システムが自動で識別します：ルートドメイン / URL / 単一の IP / CIDR ネットワーク帯 / ICP 登録 / 企業キーワード。\n必ず reason に所属の根拠(whois/証明書/ASN など)を書いてください。\nガードレール：TLD だけの指定と広すぎるネットワーク帯(IPv4 のプレフィックスは /16-/32、IPv6 は /32-/128)は拒否し、不正な行はスキップして errors に返します。",
		obj(map[string]any{
			"company": str("企業名(存在しなければ新規作成、存在すれば再利用。名前は一意)"),
			"scope":   str("資産範囲。1 行に 1 件：ドメイン / URL / IP / CIDR / ICP 登録 / 企業キーワード"),
			"reason":  str("所属の根拠(証拠/出典)。必ず書いてください"),
			"logo":    str("企業のアイコン URL(任意。企業を新規作成するときだけ有効)"),
		}, "company", "scope"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.cs == nil {
				return actool.Errorf("add_company_scope は使えません。CompanyStore が初期化されていません"), nil
			}
			var a struct {
				Company string `json:"company"`
				Scope   string `json:"scope"`
				Reason  string `json:"reason"`
				Logo    string `json:"logo"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if strings.TrimSpace(a.Company) == "" {
				return actool.Errorf("company は空にできません"), nil
			}
			companyID, _, err := t.cs.UpsertCompany(a.Company, a.Logo)
			if err != nil {
				return actool.Errorf("企業の作成または取得に失敗しました: " + err.Error()), nil
			}
			lines := splitLines(a.Scope)
			added, skipped, invalid, errMsgs := t.cs.AddScope(companyID, lines, a.Reason)
			out := map[string]any{
				"company_id": companyID,
				"added":      added,
				"skipped":    skipped,
				"invalid":    invalid,
			}
			if len(errMsgs) > 0 {
				out["errors"] = errMsgs
			}
			return jsonResult(out)
		},
	)
}

// addTaskScope lets the plan agent add test scope to THE CURRENT TASK — the coverage
// denominator and the task's authorization edge. Worker discoveries are auto-scoped
// (precise host) by insertAssets; this tool is for DELIBERATELY WIDENING: pull a whole
// root domain or whole company into scope, or add a specific subdomain / ip.
func (t *ToolSet) addTaskScope() actool.CoreTool {
	return writeTool(
		"add_task_scope",
		"テスト範囲を【このタスク】に追加します。これはこのタスクの認可の境界であり、資産テストカバレッジの分母でもあります。\nkind は次に対応します：company(企業名義のすべての資産) / root_domain(ルートドメイン全体。すべてのサブドメインを含む) / subdomain(単一の正確なサブドメイン) / ip / cidr / icp / keyword。\n補足：worker が 1 つずつ到達したホストは、システムが【自動で】範囲に追加します(正確なサブドメイン)。このツールは【意図的に広げる】ためのもので、ルートドメイン全体や企業全体を取り込んだり、特定のサブドメイン/IP を追加で指定したりします。\nvalue：company は企業名または id を渡します(企業が存在している必要があります)。root_domain/subdomain はドメイン、ip/cidr は IP またはネットワーク帯、icp/keyword は登録番号または企業キーワードを渡します。\n必ず reason に根拠(監査できるもの)を書いてください。複数件は entries 配列で渡します。",
		obj(map[string]any{
			"entries": map[string]any{"type": "array", "description": "バッチ：[{kind, value}]。kind は company/root_domain/subdomain/ip/cidr/icp/keyword のいずれか。", "items": map[string]any{"type": "object"}},
			"kind":    str("[単一] company / root_domain / subdomain / ip / cidr / icp / keyword"),
			"value":   str("[単一] 企業名または id / ドメイン / IP / CIDR / ICP / キーワード"),
			"reason":  str("追加の根拠(監査用)。必ず書いてください"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.as == nil {
				return actool.Errorf("add_task_scope は使えません。AssetStore が初期化されていません"), nil
			}
			if t.taskID <= 0 {
				return actool.Errorf("add_task_scope にはタスクコンテキストが必要です（現在 task がありません）"), nil
			}
			type scopeEntry struct {
				Kind  string `json:"kind"`
				Value string `json:"value"`
			}
			var a struct {
				Entries    []scopeEntry `json:"entries"`
				scopeEntry              // 1 件モード
				Reason     string       `json:"reason"`
			}
			_ = json.Unmarshal(in, &a)
			items := a.Entries
			if len(items) == 0 {
				items = []scopeEntry{a.scopeEntry}
			}
			var added []map[string]any
			errs := map[string]string{}
			for i, e := range items {
				ts, err := t.as.AddAgentScope(t.taskID, strings.TrimSpace(e.Kind), e.Value, a.Reason, "agent")
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				added = append(added, map[string]any{"kind": ts.Kind, "domain": ts.Domain, "net": ts.Net, "value": ts.Value, "company_id": ts.CompanyID})
			}
			out := map[string]any{"added": added}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		},
	)
}

// listUntestedAssets lets the plan agent pull the current + directly inherited
// scope's not-yet-tested assets on demand (filter by type, paginated).
func (t *ToolSet) listUntestedAssets() actool.CoreTool {
	return readTool(
		"list_untested_assets",
		"【このタスクおよび直接関連するタスク】の範囲内で、まだ事実のアンカーでカバーされていない資産を検索します（関連する範囲は読み取り専用です。追加でテストするかどうかをあなた自身が判断するための情報で、あなたの判断の代わりにはなりません）。\n資産の種類で絞り込めます（任意）：root_domain/subdomain/service/app/endpoint/ip。\nページ分割：page は 1 から始まり、page_size のデフォルトは 10。戻り値は {assets:[{id,type,label}], total, page, page_size}。タスクのコンテキスト内でのみ使用できます。",
		obj(map[string]any{
			"type":      str("資産の種類で絞り込み（任意）：root_domain/subdomain/service/app/endpoint/ip"),
			"page":      intp("ページ番号。1 から始まります（デフォルト 1）"),
			"page_size": intp("1 ページあたりの件数（デフォルト 10）"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.as == nil {
				return actool.Errorf("list_untested_assets は使えません。AssetStore が初期化されていません"), nil
			}
			if t.taskID <= 0 || t.ts == nil {
				return actool.Errorf("list_untested_assets にはタスクコンテキストが必要です"), nil
			}
			var a struct {
				Type     string `json:"type"`
				Page     int    `json:"page"`
				PageSize int    `json:"page_size"`
			}
			_ = json.Unmarshal(in, &a)
			if a.Page <= 0 {
				a.Page = 1
			}
			if a.PageSize <= 0 {
				a.PageSize = 10
			}
			offset := (a.Page - 1) * a.PageSize
			assets, total, err := t.as.ListUntestedAssetsWithSources(t.taskID, strings.TrimSpace(a.Type), a.PageSize, offset)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return jsonResult(map[string]any{
				"assets": assets, "total": total, "page": a.Page, "page_size": a.PageSize,
			})
		},
	)
}

// listAssets lets an agent query the asset table.
func (t *ToolSet) listAssets() actool.CoreTool {
	return readTool(
		"list_assets",
		"資産ライブラリを検索します：DSL 式での検索、または id/ids での直接取得に対応し、ページ分割もできます。返すのは、【このタスクおよび直接関連するタスク】のテスト範囲内の資産だけです。\nDSL：field=value あいまい一致(ILIKE) | field==value 完全一致 | field!=value 除外 | 数値フィールドは > >= < <= に対応 | 単語だけ=全文のあいまい一致；AND/OR で組み合わせ(AND のほうが優先度が高い)、括弧でグループ化できます。資産の種類は独立した type パラメータで指定し、DSL には書きません。\nid/ids を渡さないときは dsl が空であってはいけません（条件なしの全件検索は許可されません）。\n使えるフィールド：domain(ルート/サブ/サービスのドメイン)、root_domain、ip、url、page_title、icp、service_name、app_name、method(例 GET/POST)、service_type(http|other)、record_type(例 A/CNAME)、technology(配列。=はあいまい、==は完全一致)、port/status_code/company_id(整数)。\n例：status_code>=400 AND technology=shiro ；(port==80 OR port==443) AND technology=nginx",
		obj(map[string]any{
			"dsl":    str("DSL 式（構文とフィールドはツールの説明を参照）。id/ids を渡さないときは空にできません。"),
			"type":   str("資産の種類で絞り込み：root_domain|ip|subdomain|app|service|endpoint（独立したフィールドで、dsl と組み合わせられます。type だけでは検索できず、dsl が必要です）"),
			"id":     intp("資産 id を 1 件指定して直接取得します（任意。dsl/type とは同時に指定できません）"),
			"ids":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "資産 id を複数指定して直接取得します（任意。dsl/type とは同時に指定できません）"},
			"limit":  intp("返却件数の上限。デフォルト 10（任意）"),
			"offset": intp("ページ分割のオフセット。デフォルト 0（任意）"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.as == nil {
				return actool.Errorf("list_assets は使えません。AssetStore が初期化されていません"), nil
			}
			var a struct {
				DSL    string  `json:"dsl"`
				Type   string  `json:"type"`
				ID     int64   `json:"id"`
				IDs    []int64 `json:"ids"`
				Limit  int     `json:"limit"`
				Offset int     `json:"offset"`
			}
			_ = json.Unmarshal(in, &a)
			if a.Limit <= 0 {
				a.Limit = 10
			}

			var assets []*db.Asset
			var err error
			switch {
			case a.ID > 0:
				assets, err = t.as.GetByIDsInScope(t.taskID, []int64{a.ID})
			case len(a.IDs) > 0:
				assets, err = t.as.GetByIDsInScope(t.taskID, a.IDs)
			case a.DSL != "":
				assets, err = t.as.QueryDSLInScope(a.DSL, a.Type, t.taskID, a.Limit, a.Offset)
			default:
				return actool.Errorf("id も ids も渡していないときは dsl を空にできません。条件なしで全資産を問い合わせることはできません。検索条件を渡してください"), nil
			}
			if err != nil {
				return actool.Errorf("DSL エラー: " + err.Error()), nil
			}
			return jsonResult(map[string]any{
				"count":  len(assets),
				"assets": assets,
			})
		},
	)
}

// listCompanies lets an agent enumerate companies (企業) with their scope + asset count.
func (t *ToolSet) listCompanies() actool.CoreTool {
	return readTool(
		"list_companies",
		"資産ライブラリ内の【企業/会社】と、その資産範囲(scope)、所属する資産数を一覧表示します。どの会社があるかを確認し、company_id を取得するために使います（insert_assets で app を関連付けるときや、list_assets を company_id で絞り込むときに使用）。search は任意で、会社名であいまい絞り込みします(大文字小文字を区別しない)。空欄の場合はすべてを返します。",
		obj(map[string]any{
			"search": str("企業名であいまい絞り込み(任意。大文字小文字を区別しない)。空欄の場合はすべてを返します"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.cs == nil {
				return actool.Errorf("list_companies は使えません。CompanyStore が初期化されていません"), nil
			}
			var a struct {
				Search string `json:"search"`
			}
			_ = json.Unmarshal(in, &a)
			cos, err := t.cs.ListCompanies()
			if err != nil {
				return actool.Errorf("企業の検索に失敗しました: " + err.Error()), nil
			}
			q := strings.ToLower(strings.TrimSpace(a.Search))
			type companyOut struct {
				ID         int64    `json:"id"`
				Name       string   `json:"name"`
				AssetCount int      `json:"asset_count"`
				Scope      []string `json:"scope"`
			}
			out := make([]companyOut, 0, len(cos))
			for _, c := range cos {
				if q != "" && !strings.Contains(strings.ToLower(c.Name), q) {
					continue
				}
				scope := make([]string, 0, len(c.Scope))
				for _, r := range c.Scope {
					scope = append(scope, r.Raw)
				}
				out = append(out, companyOut{ID: c.ID, Name: c.Name, AssetCount: c.AssetCount, Scope: scope})
			}
			return jsonResult(map[string]any{"count": len(out), "companies": out})
		},
	)
}

// splitLines splits a multi-line string into non-empty trimmed lines.
func splitLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// WorkerTools returns the tool set for a work agent.
func (t *ToolSet) WorkerTools() []actool.CoreTool {
	return []actool.CoreTool{
		// list_findings は残す。脆弱性を報告する前に、このタスクの確認済み脆弱性を見て、同じ脆弱性の重複報告を避ける。
		t.listFindings(),
		t.addFinding(), t.recordFact(),
		// asset management (handlers guard nil store internally)。
		// add_company_scope は worker に渡さない。企業の資産範囲を定義するのは計画、メイン、Auto の役目で、worker は探索を実行するだけである。
		t.insertAssets(), t.listAssets(),
		// work をまたいで見返す。worker も他の work の観察を再利用し、作業の重複を避ける。
		// search_all_worker_traces は、intent_id を先に知らなくても、キーワードで当たったステップを全体から拾う。
		// get_worker_trace は、ある work を特定したあと、ステップを列挙し、その場で検索し、完全な内容を取る。
		t.searchAllWorkerTraces(), t.getWorkerTrace(),
		// node_detail。worker は intent_id やノード id を受け取ったあと、そのノードの完全な詳細を調べられる（上の見返しと組み合わせる）。
		t.nodeDetail(),
		// 次のツールは worker には【渡さない】。planner と main に残す（コンテキストを読むこと、work をまたいで振り返ることは計画の役目で、
		// worker はインテント 1 件の実行と書き戻しだけを行う）。list_facts / list_companies / list_worker_traces。
	}
}

// MainAgentTools returns the human-interface tool set.
func (t *ToolSet) MainAgentTools() []actool.CoreTool {
	return []actool.CoreTool{
		t.graphOverview(), t.listFindings(), t.listFacts(), t.nodeDetail(),
		t.expandDigest(), // cold-digest §6.1
		t.getWorkerOutput(), t.getWorkerTrace(), t.searchAllWorkerTraces(), t.addHint(), t.addIntent(),
		// steer_work。人は、実行中のインテント（work）へ、軌道修正の指示をリアルタイムで入れられる（中断せず、進捗も失わない）。
		t.steerWorkTool(),
		// set_goals。人は実行時に、このタスクへ新しい最終目標を足せる（プランナーはそれに基づいて達成を判定し直す）。
		t.setGoals(),
		// set_constraints。人は実行時に、このタスクへ操作の制約（allow/deny）を足し、または変え、planner と worker の探索の境界を定める。
		t.setConstraints(),
		// asset management (handlers guard nil store internally)
		t.insertAssets(), t.addCompanyScope(), t.listAssets(),
		t.addFinding(), t.recordFact(),
		t.addTaskScope(),
		// list_untested_assets。必要に応じて、このタスクの範囲内の未テスト資産を調べる（種別とページ）。追加テストは自分で決める。
		t.listUntestedAssets(),
	}
}

// AllDomainTools returns the union of all domain tools across all agent types,
// deduped by name (mainagent order wins). Used by the server to build a registry
// for injecting domain tools into agents (Auto, custom) that don't own a per-task
// ToolSet. The caller provides real stores; tools are callable at taskID=0 scope.
func (t *ToolSet) AllDomainTools() []actool.CoreTool {
	seen := map[string]bool{}
	var out []actool.CoreTool
	all := append(append(t.MainAgentTools(), t.PlannerTools()...), t.WorkerTools()...)
	for _, tool := range all {
		if !seen[tool.Name()] {
			seen[tool.Name()] = true
			out = append(out, tool)
		}
	}
	return out
}
