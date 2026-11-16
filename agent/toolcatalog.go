package agent

import (
	"context"
	"encoding/json"

	actool "github.com/Autumn-27/norma/tool"
)

// このファイルは「組み込みツール」を、コードだけの存在から、列挙でき、DB で上書きできる目録にする。
//   - BuiltinToolSeeds()：3 つの実行 agent の組み込みツール集を seed 記録へ展開する（key +
//     説明 + パラメータ schema + 既定で関連付ける agent）。サーバ起動時に冪等で tools テーブルへ播く。
//   - ToolResolve フック：実行時、DB の tools 行に従い、組み立て済みツールへ「agent ごとの絞り込み +
//     説明と schema の上書き + パラメータ既定値の注入」を行う。key と handler はコード側のまま。DB が変えるのは「説明文と既定値」だけ。
// handler（Call の挙動）は常にコード由来である。DB はそれを変えられず、モデルが見る説明と、省略時の引数だけを変えられる。

// ToolSeed は組み込みツールの、播けるスナップショット。key は CoreTool.Name()（handler と固定で結び、
// UI は読み取り専用）。Desc と Schema はコード内のツール定義から取る。Agents は、コードが既定でどの agent に渡すか。
type ToolSeed struct {
	Key    string         // = CoreTool.Name()。主キー。変更不可
	Desc   string         // トップレベルの説明（UI で上書きできる）
	Schema map[string]any // パラメータの JSON Schema（構造は読み取り専用。description と default は UI で変えられる）
	Agents []string       // 既定で関連付ける agent key（worker/planner/mainagent）
}

// builtinToolsByAgent は「読み取り専用の空殻」ToolSet（store は nil）で、各実行 agent の
// ドメインツール集を組む。ツールのコンストラクタはクロージャを Spec に入れるだけで、構築時に store を参照しない。だから nil でも安全である。
// ここで使うのは Name()、Description()、InputSchema() を読むためだけで、Call はしない。
//
// SDK の汎用ツール actool.DefaultTools()（Read/Write/Edit/MultiEdit/LS/Glob/
// Grep/Bash）は【意図的に含めない】。各 agent が固定で持ち、「誰に関連付けるか」の選択がなく、説明の多くは Prompt()
// にある（この表は Description() だけを覆うので、半分だけの上書きになり誤解を招く）。seed しないと DB 行がなく、ToolResolve は
// そのまま通し、上書きしない。挙動は以前と同じである。表に入れて管理するのは artex 自身のドメインツールだけである。
func builtinToolsByAgent() map[string][]actool.CoreTool {
	ts := NewToolSet(nil, "")
	return map[string][]actool.CoreTool{
		"mainagent": ts.MainAgentTools(),
		"planner":   ts.PlannerTools(),
		"worker":    ts.WorkerTools(),
		// goals（目標分解）は既定で set_goals と set_constraints を関連付ける。分解した目標と、
		// 抽出した操作の制約を、これらで庫へ書く。mainagent と同じ管理対象ツールを共有し、web で説明と schema を変え、agent ごとに選べる。
		"goals": {ts.setGoals(), ts.setConstraints()},
		// auto は既定で脆弱性の報告と資産管理のツールを関連付ける。他のドメインツールは UI で必要に応じて選べる。
		// 新しい庫はここから seed する。古い庫は seedAutoDefaultBindings が移行する。
		"auto": {ts.addFinding(), ts.insertAssets(), ts.addCompanyScope(), ts.listAssets(), ts.listCompanies()},
		// pentest（独立した侵入 agent）は既定で、資産の参照、資産の登録、脆弱性の報告、脆弱性の参照、企業の参照を関連付ける。
		// 新しい庫はここから seed する。古い庫は seedPentestDefaultBindings が移行する。
		"pentest": {ts.listAssets(), ts.insertAssets(), ts.addFinding(), ts.listFindings(), ts.listCompanies()},
	}
}

// defaultUnbound：これらの system ツールは通常どおり目録に入る（web で見え、agent ごとに手動で選べる）。ただし
// 既定では【どの agent にも関連付けない】。ToolResolve は、関連付けが空のツールをすべての agent から捨てる。明示的な opt-in が要る。
// それでもある agent の base ツール集に残す（goal_met が PlannerTools にある、など）のは、seed が
// それを組んで desc と schema を取れるようにするためと、ユーザーが手動で戻したあと実行時の base にあり、ToolResolve が残せるようにするためである。
//
// goal_met は、prove_goal を 1 件ずつ飛ばし、全体から【タスク全体の完了】を宣言する。重みが大きく、誤判定のリスクがあり、
// 「prove_goal が最後の目標を met にする → 自動で締めくくる」と重複する。だから既定ではどの agent にも渡さず、必要なとき手動で関連付ける。
var defaultUnbound = map[string]bool{"goal_met": true}

// BuiltinToolSeeds は各 agent の組み込みツール集を重複排除して seed 一覧へまとめる。同名ツール（list_assets のように
// 複数 agent が持つもの）は 1 件に合成し、Agents は和集合を取る。defaultUnbound のツールは関連付けを強制的に空にする。
func BuiltinToolSeeds() []ToolSeed {
	byAgent := builtinToolsByAgent()
	order := []string{"mainagent", "goals", "planner", "worker", "auto", "pentest"}

	type acc struct {
		tool   actool.CoreTool
		agents []string
	}
	m := map[string]*acc{}
	var keys []string
	for _, ak := range order {
		for _, t := range byAgent[ak] {
			a, ok := m[t.Name()]
			if !ok {
				a = &acc{tool: t}
				m[t.Name()] = a
				keys = append(keys, t.Name())
			}
			a.agents = append(a.agents, ak)
		}
	}

	out := make([]ToolSeed, 0, len(keys))
	for _, k := range keys {
		a := m[k]
		agents := a.agents
		if defaultUnbound[k] {
			agents = []string{} // 目録には入り、手動で関連付けできるが、既定ではどの agent にも渡さない（null ではなく [] を保存し、他のツールと揃える）
		}
		out = append(out, ToolSeed{
			Key:    k,
			Desc:   a.tool.Description(),
			Schema: a.tool.InputSchema(),
			Agents: agents,
		})
	}
	return out
}

// ToolResolve, if set, post-processes an agent's fully-assembled tool list against
// the DB tools table: it drops tools not bound to this agent (or globally disabled)
// and wraps the rest so the model sees the DB-overridden description/schema and
// 省略時の引数 get injected. Tools with no matching DB row (MCP/skill/host tools like
// traffic) pass through untouched. nil = tools unchanged. Wired in server/assembly.go.
var ToolResolve func(ctx context.Context, agentKey string, tools []actool.CoreTool) []actool.CoreTool

// DecorateTool wraps t so Description()/InputSchema() report the DB overrides and
// Call() injects scalar parameter defaults (from schema's "default" props) whenever
// the model omitted them. Name/Prompt/permission/scheduler flags delegate to t, so
// the tool's identity and handler are unchanged. Empty desc/schema fall back to t's.
func DecorateTool(t actool.CoreTool, desc string, schema map[string]any) actool.CoreTool {
	if desc == "" {
		desc = t.Description()
	}
	if len(schema) == 0 {
		schema = t.InputSchema()
	}
	return &overriddenTool{CoreTool: t, desc: desc, schema: schema}
}

// overriddenTool is a CoreTool decorator: it embeds the original (so all behavioral
// methods — Prompt/IsReadOnly/IsConcurrencySafe/CheckPermissions/Name — delegate)
// and overrides only the model-facing description/schema plus default injection.
type overriddenTool struct {
	actool.CoreTool
	desc   string
	schema map[string]any
}

func (o *overriddenTool) Description() string         { return o.desc }
func (o *overriddenTool) InputSchema() map[string]any { return o.schema }

func (o *overriddenTool) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	return o.CoreTool.Call(ctx, injectDefaults(in, o.schema), tc)
}

// injectDefaults fills scalar parameter defaults declared in the (possibly edited)
// schema into the input JSON whenever the model omitted the field or left it empty/
// null. Structure (names/types/required) is untouched — only 省略時の値 are merged in.
func injectDefaults(in json.RawMessage, schema map[string]any) json.RawMessage {
	defs := scalarDefaults(schema)
	if len(defs) == 0 {
		return in
	}
	m := map[string]json.RawMessage{}
	if len(in) > 0 {
		if err := json.Unmarshal(in, &m); err != nil {
			return in // non-object input: don't touch it
		}
	}
	changed := false
	for k, dv := range defs {
		if cur, ok := m[k]; !ok || isEmptyJSON(cur) {
			m[k] = dv
			changed = true
		}
	}
	if !changed {
		return in
	}
	b, err := json.Marshal(m)
	if err != nil {
		return in
	}
	return b
}

// scalarDefaults extracts properties[k]["default"] for scalar params (string/
// integer/number/boolean). Array/object defaults are skipped: merging them is
// ambiguous and not worth the surprise.
func scalarDefaults(schema map[string]any) map[string]json.RawMessage {
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return nil
	}
	out := map[string]json.RawMessage{}
	for name, raw := range props {
		p, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		dv, ok := p["default"]
		if !ok || dv == nil {
			continue
		}
		switch p["type"] {
		case "string", "integer", "number", "boolean":
			if b, err := json.Marshal(dv); err == nil {
				out[name] = b
			}
		}
	}
	return out
}

func isEmptyJSON(raw json.RawMessage) bool {
	s := string(raw)
	return s == "null" || s == `""`
}
