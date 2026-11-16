package agent

import (
	"encoding/json"
	"fmt"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

const findingIDGuidance = "\n\n**脆弱性番号の取り決め**：finding_id は独立した脆弱性記録の ID です。finding_node_id は探索ノードの ID です。list_findings / list_task_findings / node_detail / get_task_node_detail の id は探索ノード ID のままなので、同じ戻り値の finding_id から独立した番号を読んでください。get_finding_traffic / bind_finding_traffic は独立した finding_id を使います。古い update_finding_report の finding_id パラメータには、引き続き finding_node_id を渡します。report_finding の先頭行の数字を証拠ツールに使わないでください。番号のエラーが出ても、ほかの数字を推測しないでください。"

// The server supplies the persisted setting. A missing setting/host is off.
// Consulted at assembly and again on writes so an already-running session
// cannot keep binding after the user switches the feature off.
var FindingTrafficBindingEnabled func() bool

func findingTrafficBindingEnabled() bool {
	return FindingTrafficBindingEnabled != nil && FindingTrafficBindingEnabled()
}

// Applied after ToolResolve: user descriptions and prompts remain intact, while
// all actual reporters (including Planner and custom chat agents) see the same
// API contract. Disabled/unbound tools are never reintroduced here.
func findingWorkflowTools(agentKey string, tools []actool.CoreTool) ([]actool.CoreTool, string) {
	if !findingTrafficBindingEnabled() {
		out := make([]actool.CoreTool, 0, len(tools))
		for _, tool := range tools {
			if tool.Name() == "bind_finding_traffic" {
				continue
			}
			if agentKey == "reporter" && (tool.Name() == "traffic_search" || tool.Name() == "traffic_get" || tool.Name() == "traffic_blob") {
				continue
			}
			switch tool.Name() {
			case "report_finding", "add_hint", "add_task_hint":
				// Work on a copy: toggling back on must restore the original schema.
				raw, _ := json.Marshal(tool.InputSchema())
				var schema map[string]any
				if json.Unmarshal(raw, &schema) == nil {
					stripTrafficParameters(schema)
					tool = DecorateTool(tool, tool.Description(), schema)
				}
			}
			out = append(out, tool)
		}
		return out, ""
	}
	out := append([]actool.CoreTool(nil), tools...)
	has := map[string]bool{}
	for i, tool := range out {
		has[tool.Name()] = true
		note := ""
		switch tool.Name() {
		case "report_finding":
			note = "\n既定では、レポート Agent がレポートを書く前にトラフィックを照合して関連付けます。報告者は evidence に、検証コマンド、重要な出力、すでにある実際のトラフィック ID とその用途を残し、レポート Agent が実行記録と照合できるようにしてください。関連付けのためだけに追加でパケットを調べる必要はありません。明示的な即時関連付けも互換です。traffic_refs または evidence_hint_id で、確認済みの参照を渡せます。後者はこのタスクの指定 hint の構造化参照を読みます。いずれかが無効なら、今回の報告はすべて失敗します。TCP やパケットがない場合、これらの任意パラメータは不要です。戻り値の finding_id と finding_node_id は、それぞれ独立した記録と探索ノードを表します。"
		case "add_hint", "add_task_hint":
			note = "\n確認済みの脆弱性を引き渡すときは、対応するヒントの traffic_refs に、確認済みトラフィックの ID、用途、説明、順序を残してください（1 件ならトップレベル、まとめて渡すなら対応する hints 要素）。text には、それが証明する具体的な脆弱性を書いてください。呼び出し側は、文章だけを引き渡して、すでにあるトラフィック参照を捨ててはいけません。未確認の候補は証拠として渡せません。"
		case "get_finding_traffic", "bind_finding_traffic", "list_findings", "list_task_findings", "node_detail", "get_task_node_detail", "update_finding_report":
			note = findingIDGuidance
		}
		if note != "" {
			out[i] = DecorateTool(tool, tool.Description()+note, tool.InputSchema())
		}
	}
	guidance := ""
	if has["report_finding"] || has["add_task_hint"] || has["add_hint"] {
		guidance = "\n\n**トラフィック証拠の引き渡し（任意）**：自動の関連付けは、既定ではレポート Agent が、脆弱性を保存したあと、レポートを書く前に行います。報告者は evidence に、検証コマンド、重要な出力、すでにある実際のトラフィック ID とその用途を残し、タスクでは intent_id を付けて、レポート Agent がたどれるようにしてください。関連付けのためだけに追加でパケットを調べる必要はありません。Auto / Planner が代わりに報告するときは、実行者がすでに持っている参照を捨てないでください。add_hint / add_task_hint は traffic_refs で引き渡せます。明示的な即時関連付けも、report_finding の traffic_refs / evidence_hint_id と互換です。TCP、またはパケットがないときは、通常どおり登録してください。ID を推測せず、パケットを補うためだけに探索を繰り返さないでください。"
		if has["add_task_hint"] && !has["add_hint"] {
			guidance += "\nプラットフォームの対話にタスクのコンテキストがないときは、report_finding を直接呼ばないでください。add_task_hint で、対応する既存タスクへ引き渡し、タスク Agent に登録させ、list_task_findings で結果を照合してください。"
		}
		if has["prove_goal"] || has["goal_met"] {
			guidance += "\n目標の完了を判定する前に、今回すでにある証拠の報告と引き渡しを終えてください。証拠の引き渡しが終わっていないのに、文章上の脆弱性が登録されたことだけを理由に、タスクを終えたり Worker を取り消したりしないでください。パケットがない場合は、待ったり、無理にパケットを取ったりする必要はありません。"
		}
	}
	if has["update_finding_report"] && has["bind_finding_traffic"] && has["get_finding_traffic"] {
		guidance += "\n\n**レポート前のトラフィック自動関連付け（有効）**：今回起きた脆弱性について、トラフィックを照合して関連付けてからレポートを書いてください。まず report_finding の戻り JSON、または get_task_node_detail / list_task_findings から、明確な finding_id と finding_node_id を取ってください。脆弱性の詳細、対応するインテントの実行記録、すでにある証拠の一覧を読み、報告者が引き渡した実際の ID を優先してください。今回の検証が HTTP で、トラフィックツールが使えるなら、traffic_search で候補を絞り、traffic_get で 1 件ずつ、リクエスト/レスポンスがその脆弱性を本当に支えるか確認してください。ドメインと時刻は絞り込みにだけ使い、帰属の証明にはなりません。確認した証拠を再現順に bind_finding_traffic(finding_id, traffic_refs) で関連付け、baseline / proof / verification / supporting を選び、用途を書いてください。操作できるのは今回の脆弱性だけです。脆弱性を作り直したり、目標を再探索したりしないでください。関連付けに成功したら、get_finding_traffic を再度呼んで最新の version を取り、必要な本文を読み、実際に読んだ version を evidence_version として update_finding_report に渡してください（その finding_id パラメータは、引き続き finding_node_id です）。すでにある関連付けを繰り返して追加する必要はありません。TCP、未採取、ツールが使えない、正確に一致するものがない、というときは自動関連付けを飛ばし、文章やコマンドの証拠に基づいて通常どおりレポートを書き、理由を書いてください。トラフィックを揃えるために推測しないでください。関連付けに失敗したら成功したと述べないでください。すでにある証拠は残し、レポートに未関連の理由を書いてください。"
	}
	if guidance != "" || has["get_finding_traffic"] || has["update_finding_report"] {
		guidance += findingIDGuidance
	}
	return out, guidance
}

func stripTrafficParameters(schema map[string]any) {
	props, _ := schema["properties"].(map[string]any)
	delete(props, "traffic_refs")
	delete(props, "evidence_hint_id")
	if required, ok := schema["required"].([]any); ok {
		kept := required[:0]
		for _, key := range required {
			if key != "traffic_refs" && key != "evidence_hint_id" {
				kept = append(kept, key)
			}
		}
		schema["required"] = kept
	}
	if hints, ok := props["hints"].(map[string]any); ok {
		if items, ok := hints["items"].(map[string]any); ok {
			stripTrafficParameters(items)
		}
	}
}

// HintTrafficSchema is shared by the task-local and cross-task hint tools.
func HintTrafficSchema() map[string]any {
	return map[string]any{"type": "array", "description": "任意：確認済みで、このヒント内の具体的な脆弱性に対応するトラフィックの参照（順序を保持）。引き継いだ後、report_finding に evidence_hint_id を渡すと、これらの参照を付けられます。", "items": obj(map[string]any{"traffic_id": str("実際のトラフィック ID"), "role": str("baseline / proof / verification / supporting"), "note": str("このトラフィックが裏付ける結論")}, "traffic_id")}
}

func (t *ToolSet) findingRefsFromHint(hintID int64, explicit []db.TrafficRef) ([]db.TrafficRef, error) {
	if hintID <= 0 {
		return db.NormalizeTrafficRefs(explicit)
	}
	n, err := t.ts.GetNode(hintID) // local store only: inherited hints cannot supply evidence
	if err != nil {
		return nil, err
	}
	if n == nil || n.Kind != db.KindHint {
		return nil, fmt.Errorf("evidence_hint_id=%d はこのタスクのヒントノードである必要があります（継承したヒントは関連付けに直接使えません）", hintID)
	}
	var payload struct {
		Refs []db.TrafficRef `json:"traffic_refs"`
	}
	if err := json.Unmarshal(n.Payload, &payload); err != nil {
		return nil, err
	}
	return db.NormalizeTrafficRefs(append(append([]db.TrafficRef{}, explicit...), payload.Refs...))
}
