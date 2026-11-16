package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/traffic"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// jsonResult marshals v to a JSON tool result.
func jsonResult(v any) (actool.Result, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	return actool.Text(string(b)), nil
}

// 本ファイルは P2「タスク横断の編成ツール群」を実装する（docs/実行編成 §2 P2）。これらは host ツールである。
// Manager（任意のタスクの Store）、Engine（一時停止）、タスク作成の流れに触れるので、server 層に置く。
// 読み取り系は「既存の per-task ツール」を対象タスクの store に向けて動かす（一時的な ToolSet を作り
// 対応するツールを Call する）。同じロジックをそのまま使う。制御系（spawn/pause）は Manager/Engine を直接呼ぶ。
// トラフィックツールと同じく tools テーブルへ seed し、agent ごとに関連付ける（編成 agent に関連付けたときだけ見える）。

// hostTools is the runtime host-tool provider fed to ToolAugment: traffic tools
// (gated by capture) + cross-task orchestration tools + user-defined custom tools.
// The second return is the names of custom tools flagged `deferred` (schema
// withheld, routed via SearchExtraTools/ExecuteExtraTool). Per-agent binding still
// decides who actually sees any of them.
//
//nolint:unused // used as the hostTools provider in wireAgentAugment
func (s *Server) hostTools() ([]actool.CoreTool, map[string][]string) {
	tools := append(s.m.HostTools(), s.orchestrationTools()...)
	tools = append(tools, s.findingRetestTools()...)
	tools = append(tools, s.platformTools()...) // プラットフォーム操作のツール（skill/ツール/MCP の作成と変更。Auto 用）
	custom, err := s.customTools()
	if err != nil {
		log.Printf("[custom-tool] 読み込みに失敗しました: %v", err)
		return tools, nil
	}
	tools = append(tools, custom...)
	// deferred custom tools → name -> its bound agent keys. ToolAugment turns a
	// name into a deferred entry only for agents it's actually bound to (so we don't
	// advertise a tool the per-agent binding will drop from the callable set).
	deferred := map[string][]string{}
	rows, _ := s.m.pg.ListCustomTools()
	for _, t := range rows {
		if t.Deferred && t.Enabled {
			deferred[t.Key] = t.Agents
		}
	}
	return tools, deferred
}

// orchestrationTools returns the cross-task tool set. Bound per-agent via the
// tools table (default: no binding — opt-in for orchestration agents).
func (s *Server) orchestrationTools() []actool.CoreTool {
	return []actool.CoreTool{
		s.toolListTasks(),
		s.toolListLLMProfiles(),
		s.toolSpawnTask(),
		s.toolPauseTask(),
		s.toolGetTaskGraph(),
		s.toolListTaskFindings(),
		s.toolAddHint(),
		s.toolGetWorkerTrace(),
		s.toolListWorkerTraces(),
		s.toolSearchWorkerTraces(),
		s.toolGetTaskNodeDetail(),
		s.toolUpdateFindingReport(),
		s.toolGetFindingTraffic(),
		s.toolBindFindingTraffic(),
	}
}

// --- schema helpers ---

func strParam(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

// parseProfileID reads an LLM profile id from a tool arg that may arrive as a JSON
// number (5) or a numeric string ("5"); returns 0 when absent/unparseable.
func parseProfileID(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		v, _ := strconv.ParseInt(strings.TrimSpace(str), 10, 64)
		return v
	}
	return 0
}

func objSchema(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		req := make([]any, len(required))
		for i, r := range required {
			req[i] = r
		}
		m["required"] = req
	}
	return m
}

func roTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		ReadOnly:   func(json.RawMessage) bool { return true },
		Concurrent: func(json.RawMessage) bool { return true },
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

func wrTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

// delegateToTask resolves the `task_id` in the input, builds a ToolSet bound to
// that task's store, strips task_id, and calls the chosen per-task tool — so the
// cross-task read reuses the exact in-task logic against another task.
func (s *Server) delegateToTask(ctx context.Context, in json.RawMessage, pick func(*agent.ToolSet) actool.CoreTool) (actool.Result, error) {
	var head struct {
		TaskID string `json:"task_id"`
	}
	_ = json.Unmarshal(in, &head)
	if strings.TrimSpace(head.TaskID) == "" {
		return actool.Errorf("task_id は必須です"), nil
	}
	t, ok := s.m.Task(head.TaskID)
	if !ok {
		return actool.Errorf("task が存在しません: " + head.TaskID), nil
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(in, &m)
	delete(m, "task_id")
	inner, _ := json.Marshal(m)
	tsx := agent.NewToolSet(t.Store, "orchestrator")
	if s.m.Assets() != nil {
		tsx.SetAssetStore(s.m.Assets(), s.m.Assets().Companies())
	}
	tsx.SetNotify(t.Notify)         // 汎用の起動（専用コールバックのない書き込みはここを通る。読み取りツールは no-op）
	tsx.SetNotifyHint(t.NotifyHint) // add_hint → 「人（メイン agent）が戦略ヒントを N 件追加しました：…」を 1 件記録して planner を起こす
	return pick(tsx).Call(ctx, inner, nil)
}

// --- tools ---

func (s *Server) toolListTasks() actool.CoreTool {
	return roTool("list_tasks",
		"すべてのタスク(id/説明/目標/状態/実行時間/親タスク/LLM 設定)を一覧表示します。オーケストレーション agent は、これで全体を把握し、どのタスクが長く止まっているか、それぞれがどの LLM を使っているかを確認します。実行時間：実行中=作成→現在、終了状態=作成→最後のアクティビティ(秒)。llm_profile：タスクの planner/worker が使う設定名で、(有効な設定)=グローバルで有効な設定に従います。",
		objSchema(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			lastAct, _ := s.m.PG().LastActivityAll()
			// id -> name to resolve each task's pinned LLM profile.
			profName := map[int64]string{}
			if profs, err := s.m.pg.ListProfiles(); err == nil {
				for _, p := range profs {
					profName[p.ID] = p.Name
				}
			}
			out := make([]map[string]any, 0)
			for _, t := range s.m.List() {
				status := s.deriveTaskStatus(t)
				end := lastAct[t.ExpID]
				if live := s.engine.LastActivity(t.ID); live > end {
					end = live
				}
				dur := int64(0)
				if status == "running" {
					dur = time.Now().Unix() - t.CreatedAt
				} else if end > t.CreatedAt {
					dur = end - t.CreatedAt
				}
				row := map[string]any{"id": t.ID, "description": t.Description, "goal": t.Goal, "status": status, "run_seconds": dur}
				if t.ParentRef != "" {
					row["parent_ref"] = t.ParentRef
				}
				llmState := t.llmStateSnapshot()
				if llmState.ProfileID == nil {
					row["llm_profile"] = "(有効な設定)"
				} else if n, ok := profName[*llmState.ProfileID]; ok {
					row["llm_profile"] = n
				} else {
					row["llm_profile"] = fmt.Sprintf("#%d(削除済み)", *llmState.ProfileID)
				}
				out = append(out, row)
			}
			return jsonResult(out)
		})
}

// toolListLLMProfiles lists the available LLM profiles (name/model/active) so an
// orchestration agent can pick one for spawn_task's llm_profile. Never leaks keys.
func (s *Server) toolListLLMProfiles() actool.CoreTool {
	return roTool("list_llm_profiles",
		"利用できる LLM 設定(profile)を一覧表示します：id、名前、モデル、形式、現在有効な設定かどうか。spawn_task の llm_profile_id パラメータに id を渡すと、子タスク専用の LLM を指定できます（例：偵察は安価なモデル、悪用は強力なモデル）。API Key は含みません。",
		objSchema(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			profs, err := s.m.pg.ListProfiles()
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			out := make([]map[string]any, 0, len(profs))
			for _, p := range profs {
				out = append(out, map[string]any{
					"id": p.ID, "name": p.Name, "model": p.Model, "format": p.Format, "is_active": p.IsDefault,
				})
			}
			return jsonResult(map[string]any{"profiles": out})
		})
}

func (s *Server) toolSpawnTask() actool.CoreTool {
	return wrTool("spawn_task",
		"子タスクを新規作成して探索エンジンを起動し、task_id を返します。1 つの事柄(例：1 問/1 つの目標)を独立したタスクとして割り当てるために使います。parent_ref は任意：現在のオーケストレーションに関連する親タスクの id を渡すと、親子の関連付けになります。",
		objSchema(map[string]any{
			"description":            strParam("タスクの説明(短いタイトル)"),
			"goal":                   strParam("タスクの目標(何を達成するか)"),
			"parent_ref":             strParam("任意：親タスクの id(親子の関連付け)"),
			"source_task_ids":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": fmt.Sprintf("任意：読み取り専用で継承する元タスクの id の一覧(最大 %d 個)。子タスクは、これらのタスクで判明した資産/結論を、読み取り専用で出発点として参照できます。parent_ref の単純な親子ポインタとは異なり、内容を継承します。", db.MaxTaskSourceCount)},
			"llm_profile_id":         map[string]any{"type": "integer", "description": "任意：この子タスクの planner/worker が使う LLM 設定の id(list_llm_profiles を参照)。空欄の場合は親タスクを継承し、さらにグローバルで有効な設定にフォールバックします"},
			"timeout_seconds":        map[string]any{"type": "integer", "description": "任意：タスク単位のタイムアウト(秒)。時間になると正常終了の処理を行い、timeout の終了状態に入ります。空欄または 0 = 無制限"},
			"plan_heartbeat_seconds": map[string]any{"type": "integer", "description": "任意：planner のハートビートの間隔(秒)。前回の計画終了またはタスク開始からこの値が経過し、その間にトリガーがない場合 → 計画を 1 回実行します(デッドロックの回避 + 実行中の worker の監督のため)。空欄または 0 = デフォルトの 600(10min)；"},
			"seed_first_intent":      map[string]any{"type": "boolean", "description": "任意：単純なタスクで有効にできます。作成時に、説明+目標を内容とする最初のインテントを直接発行し、worker が最初の planner を待たずにすぐテストを始められるようにします。デフォルトは false(標準の「先に計画してから実行」)。"},
		}, "description", "goal"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Description          string          `json:"description"`
				Goal                 string          `json:"goal"`
				ParentRef            string          `json:"parent_ref"`
				SourceTaskIDs        []string        `json:"source_task_ids"`
				LLMProfileID         json.RawMessage `json:"llm_profile_id"`
				TimeoutSeconds       int             `json:"timeout_seconds"`
				PlanHeartbeatSeconds int             `json:"plan_heartbeat_seconds"`
				SeedFirstIntent      bool            `json:"seed_first_intent"`
			}
			_ = json.Unmarshal(in, &a)
			if strings.TrimSpace(a.Description) == "" {
				a.Description = "名称未設定のタスク"
			}
			if strings.TrimSpace(a.Goal) == "" {
				return actool.Errorf("goal は必須です"), nil
			}
			if a.TimeoutSeconds < 0 {
				a.TimeoutSeconds = 0
			}
			// 読み取り専用で継承する元タスク。件数の上限と、各 id の有効性、重複、存在。検証は HTTP のタスク作成と同じ。
			if len(a.SourceTaskIDs) > db.MaxTaskSourceCount {
				return actool.Errorf(fmt.Sprintf("関連付けできるタスクは最大 %d 個です", db.MaxTaskSourceCount)), nil
			}
			sourceIDs := make([]int64, 0, len(a.SourceTaskIDs))
			seenSources := map[int64]bool{}
			for _, raw := range a.SourceTaskIDs {
				id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
				if err != nil || id <= 0 || seenSources[id] {
					return actool.Errorf("関連付けるタスクの id が無効か、重複しています"), nil
				}
				if _, ok := s.m.Task(strconv.FormatInt(id, 10)); !ok {
					return actool.Errorf(fmt.Sprintf("関連付けるタスク #%d が存在しません", id)), nil
				}
				seenSources[id] = true
				sourceIDs = append(sourceIDs, id)
			}
			// LLM profile resolution: explicit id > inherit parent's pin > active(nil).
			var pin *int64
			if id := parseProfileID(a.LLMProfileID); id > 0 {
				if _, ok := s.loadProfileConfig(id); !ok {
					return actool.Errorf(fmt.Sprintf("LLM 設定 #%d が存在しないか、API Key が設定されていません", id)), nil
				}
				pin = &id
			} else if a.ParentRef != "" {
				if pt, ok := s.m.Task(a.ParentRef); ok {
					pin = pt.LLMProfileID
				}
			}
			var llmIDs []int64
			if pin != nil {
				llmIDs = []int64{*pin}
			}
			t, err := s.m.CreateTaskWithOptions(a.Description, a.Goal, db.TaskCreateOptions{
				SourceTaskIDs:        sourceIDs,
				LLMProfileIDs:        llmIDs,
				TimeoutSeconds:       a.TimeoutSeconds,
				PlanHeartbeatSeconds: a.PlanHeartbeatSeconds,
			})
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if a.ParentRef != "" {
				t.ParentRef = a.ParentRef
				if id, e := strconv.ParseInt(t.ID, 10, 64); e == nil {
					_ = s.m.PG().SetParentRef(id, a.ParentRef)
				}
			}
			// 作成後の共通処理。HTTP のタスク作成（server.go createTask）と同じ launchTask を使う。
			// seed と、バックグラウンドで見える目標分解（第 0 ラウンド、LLM の手順、goal を 1 件ずつ）と engine.Run。
			// seed_first_intent のデフォルトは false（標準は先に計画してから実行）。単純なタスクは 1 work のテストを直接出せる。
			s.launchTask(t, a.Description+" "+a.Goal, a.SeedFirstIntent)
			return actool.Text(fmt.Sprintf("task created: %s", t.ID)), nil
		})
}

func (s *Server) toolPauseTask() actool.CoreTool {
	return wrTool("pause_task", "指定したタスクを一時停止します(その planner/worker のループを停止)。",
		objSchema(map[string]any{"task_id": strParam("一時停止するタスクの id")}, "task_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				TaskID string `json:"task_id"`
			}
			_ = json.Unmarshal(in, &a)
			t, ok := s.m.Task(a.TaskID)
			if !ok {
				return actool.Errorf("task が存在しません: " + a.TaskID), nil
			}
			if _, err := s.applyTaskControlWithCause(t, "pause", agent.AbortPausedByOrchestrator); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text("task paused: " + a.TaskID), nil
		})
}

func (s *Server) toolGetTaskGraph() actool.CoreTool {
	return roTool("get_task_graph", "指定したタスクの探索グラフの概要を読み取ります(graph_overview と同じ：資産数/frontier/検出事項/カバレッジなど)。task_id でタスクを指定します。",
		objSchema(map[string]any{"task_id": strParam("タスク id")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).GraphOverviewTool)
		})
}

func (s *Server) toolListTaskFindings() actool.CoreTool {
	return roTool("list_task_findings", "指定したタスクの確認済みの脆弱性を読み取ります(flag/PoC を含み、各項目に id/task_id/intent_id/vulnclass/severity/要約/状態が付きます)。task_id でタスクを指定します。",
		objSchema(map[string]any{"task_id": strParam("タスク id")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).ListFindingsTool)
		})
}

func (s *Server) toolAddHint() actool.CoreTool {
	return wrTool("add_task_hint", "指定したタスクに戦略的なヒントを注入します(そのタスクの planner が次にインテントを生成するときに読みます)。\n★バッチ優先：複数のヒントは hints 配列にまとめて一度に送ってください（戻り値の ids 配列は hints と同じ長さ・同じ順序で、失敗した項目は id=0）。1 件だけなら hints を省略し、トップレベルの text を直接渡します。",
		objSchema(map[string]any{
			"task_id":      strParam("タスク id"),
			"hints":        map[string]any{"type": "array", "description": "【こちらを優先して使う】ヒントの配列。各要素のフィールドはトップレベルと同じ（text/asset_ids/traffic_refs）。", "items": objSchema(map[string]any{"text": strParam("ヒントの内容"), "asset_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}}, "traffic_refs": agent.HintTrafficSchema()})},
			"text":         strParam("[単一] ヒントの内容"),
			"traffic_refs": agent.HintTrafficSchema(),
			"asset_ids":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "紐付ける資産 id（任意。0/1/複数。そのタスク内の資産 id）"},
		}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).AddHintTool)
		})
}

func (s *Server) toolGetWorkerTrace() actool.CoreTool {
	return roTool("get_task_worker_trace",
		"指定したタスクの work(インテント)の実行過程を確認します：get_task_worker_trace(task_id, intent_id) でステップの要約を、続けて step_ids=[...] を付けるとそのステップの完全な内容を取得します(一度に最大 5 件で、超えた分は先頭 5 件だけを返します)。",
		objSchema(map[string]any{
			"task_id":   strParam("タスク id"),
			"intent_id": map[string]any{"type": "integer", "description": "インテント id(そのタスク内の work)"},
			"step_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "任意：完全な内容を取得するステップ id(一度に最大 5 件で、超えた分は先頭 5 件だけを返し、残りは omitted_step_ids に列挙されます)"},
		}, "task_id", "intent_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).GetWorkerTraceTool)
		})
}

func (s *Server) toolListWorkerTraces() actool.CoreTool {
	return roTool("list_task_worker_traces", "指定したタスクで実行された work(インテント)と、それぞれのステップ数を一覧表示します。確認する価値のある work を見つけるために使います(そのあと get_task_worker_trace を使用)。",
		objSchema(map[string]any{"task_id": strParam("タスク id")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).ListWorkerTracesTool)
		})
}

func (s *Server) toolSearchWorkerTraces() actool.CoreTool {
	return roTool("search_task_worker_traces", "指定したタスクで、すべての work の実行過程をキーワードで検索します(一致したステップの要約 + intent_id を返します)。",
		objSchema(map[string]any{"task_id": strParam("タスク id"), "q": strParam("検索キーワード")}, "task_id", "q"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).SearchWorkerTracesTool)
		})
}

func (s *Server) toolGetTaskNodeDetail() actool.CoreTool {
	return roTool("get_task_node_detail",
		"指定したタスクの探索グラフのノードの完全な内容を読み取ります(検出事項/事実/インテント/目標：要約 + 詳細/証拠/PoC)。id は探索ノードの id です(report_finding が返すもの、または list_task_findings の id)。脆弱性レポートを書く前に、これでその脆弱性の完全な証拠を取得します。",
		objSchema(map[string]any{
			"task_id": strParam("タスク id"),
			"id":      map[string]any{"type": "integer", "description": "探索グラフのノード id(資産 id ではない)"},
		}, "task_id", "id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).NodeDetailTool)
		})
}

// toolUpdateFindingReport writes/overwrites a finding's detailed Markdown report.
// finding_id is the id report_finding returned ("finding recorded: <id>", the
// finding node id). The write (SetFindingReportByNodeID) is keyed by node_id and
// task-agnostic, so this host tool needs no task_id / exploration store.
func (s *Server) toolUpdateFindingReport() actool.CoreTool {
	return wrTool("update_finding_report",
		"登録済みの脆弱性の【詳細レポート】を書き込み/更新します(Markdown 全文。以前の内容は全体を上書き)。finding_id には report_finding が返した id(\"finding recorded: <id>\" の数字)を渡します。レポートには、脆弱性の概要、影響と危険性、再現手順、証拠/PoC、修正の提案を含めることを推奨します。",
		objSchema(map[string]any{
			"finding_id":       map[string]any{"type": "integer", "description": "対象の脆弱性 id(report_finding が返した id)"},
			"report":           strParam("詳細レポートの全文。Markdown 形式"),
			"evidence_version": map[string]any{"type": "integer", "description": "get_finding_traffic が返した証拠の version。新しい証拠の変更をレポートが上書きするのを防ぐために使います"},
		}, "finding_id", "report"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				EvidenceVersion *int64          `json:"evidence_version"`
				FindingID       json.RawMessage `json:"finding_id"`
				Report          string          `json:"report"`
			}
			_ = json.Unmarshal(in, &a)
			nodeID := parseProfileID(a.FindingID) // 「数値、または数値の文字列」の解釈を流用する
			if nodeID <= 0 {
				return actool.Errorf("finding_id が無効です"), nil
			}
			n, err := s.m.pg.SetFindingReportVersionByNodeID(ctx, nodeID, a.Report, a.EvidenceVersion)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if n == 0 {
				return actool.Errorf(fmt.Sprintf("finding_id=%d に対応する脆弱性記録が見つかりません（先に report_finding で登録してください）", nodeID)), nil
			}
			return actool.Text(fmt.Sprintf("finding %d report updated (%d chars)", nodeID, len(a.Report))), nil
		})
}

// deriveTaskStatus mirrors listTasks' status derivation for the list_tasks tool.
func (s *Server) deriveTaskStatus(t *Task) string {
	lifecycle := t.lifecycleSnapshot()
	switch {
	case isTerminalStatus(lifecycle.Status):
		return lifecycle.Status
	case lifecycle.Paused || s.engine.IsPaused(t.ID):
		return "paused"
	case s.engine.ReadyFor(t) && s.engine.Started(t.ID):
		return "running"
	}
	return "created"
}

// orchestrationToolSeeds seeds the cross-task tools into the tools table so they
// are bindable per-agent (default: bound to nobody — opt-in for orchestration
// agents). First-insert only, like the traffic seeds.
func (s *Server) seedOrchestrationTools() {
	// task-op + platform tools default-bind to the built-in Auto agent (プラットフォームを操作するための
	// ものである)。SeedTool は初回挿入だけ効く。既存 DB で seed 済みの行は seedAutoDefaultBindings が関連付けを補う。
	autoAgents, _ := json.Marshal([]string{"auto"})
	for _, t := range s.orchestrationTools() {
		schema, _ := json.Marshal(t.InputSchema())
		bindings := autoAgents
		if t.Name() == "bind_finding_traffic" {
			bindings = json.RawMessage(`["reporter"]`)
		}
		_ = s.m.PG().SeedTool(t.Name(), t.Description(), schema, bindings)
	}
	for _, t := range s.platformTools() {
		schema, _ := json.Marshal(t.InputSchema())
		_ = s.m.PG().SeedTool(t.Name(), t.Description(), schema, autoAgents)
	}
	s.refreshBuiltinToolSchemas()
	s.seedAutoDefaultBindings()
	s.seedPlannerDefaultBindings()
	s.seedPlannerListAssetsBinding()
	s.seedCompanyScopeRebind()
	s.seedWorkerReadToolsUnbind() // list_facts/list_companies/list_worker_traces を worker のデフォルトから外す（1 回限り）
	s.seedWorkerReadbackRebind()  // 古い移行の誤削除を直す。search_all_worker_traces/get_worker_trace/node_detail を worker へ関連付け直す（1 回限り）
	s.seedAutoReportFindingBinding()
	s.unbindGoalMetDefault()
	s.reseedGoalsPrompt()             // goals のプロンプトに「操作制約を抜き出す」ステップを足す → 既存 DB へ新しいデフォルトを 1 版追加（1 回限り）
	s.reseedMainAgentPrompt()         // mainagent のプロンプトに「目標達成後、add_intent で目標を作るか聞き返す」を足す（1 回限り）
	s.reseedPlannerPrompt()           // planner のプロンプト。「0 インテント」の正当な理由を書き直し、定量の受け入れ確認を足す（1 回限り）
	s.reseedWorkerPrompt()            // worker のプロンプト。否定の結論に証拠のしきい値を足す（1 回限り）
	s.seedReporterAgent()             // 用意する「レポート作成」agent とツール関連付けと finding トリガー（1 回限り）
	s.upgradeReporterTriggerMessage() // 既存 DB の追加移行。reporter に evidence_version を返させる（1 回限り）
	s.seedFindingTrafficTools()       // 任意の証拠パラメータと読み取り専用の証拠ツールを足す。ユーザー設定は残す
	s.seedFindingWorkflowTools()
	s.refreshToolDescriptionsJa() // 組み込みツールの説明とパラメータ説明を日本語のデフォルトにする（1 回限り）
	// 注。pentest のデフォルトのツール関連付けに移行は不要。BuiltinToolSeeds は新規初期化のときに
	// list_assets/insert_assets/report_finding/list_findings/list_companies を
	// pentest と一緒に seed 済みである（このプロジェクトに古い DB はなく、移行はしない）。
}

// refreshBuiltinToolSchemas propagates code schema/description changes on the
// orchestration + platform tools into already-seeded rows ONCE per version flag —
// SeedTool is first-insert-only, so a new param (e.g. spawn_task の llm_profile) never
// reaches an old DB otherwise. Preserves each tool's agent binding + enabled flag.
// Bump the flag whenever these tools' schemas/descriptions change in code.
func (s *Server) refreshBuiltinToolSchemas() {
	const flag = "tool_schema_refresh_v7_list_facts_paging"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	tools := append(s.orchestrationTools(), s.platformTools()...)
	for _, t := range tools {
		schema, _ := json.Marshal(t.InputSchema())
		if err := s.m.pg.RefreshToolDefaults(t.Name(), t.Description(), schema); err != nil {
			log.Printf("[tools] refresh %s schema failed: %v", t.Name(), err)
		}
	}
	// 同時に、一部の組み込み agent ツールをコードのデフォルトへ戻す。
	//   - goal_met。既存 DB に seed された説明には「この計画ラウンドを終える」という誤解があり、planner がこれを
	//     「空のラウンドを終える」手段にして、走り始めにタスク全体の完了を誤判定する。
	//   - insert_assets。related 引数を追加した（資産が現在のタスクに関係するか、カバー率に入れるかを示す）。
	//     SeedTool は初回挿入だけなので、既存 DB の seed 済み schema はこの新パラメータを受け取れない。
	//   - list_facts。ページングにし、limit/before/q を追加した。既存 DB の seed 済みの空 schema のままだと
	//     ツール管理ページは「パラメータなし」と出し、モデルもこのパラメータ説明を受け取れない。
	refreshBuiltin := map[string]bool{"goal_met": true, "insert_assets": true, "list_facts": true}
	for _, sd := range agent.BuiltinToolSeeds() {
		if !refreshBuiltin[sd.Key] {
			continue
		}
		schema, _ := json.Marshal(sd.Schema)
		if err := s.m.pg.RefreshToolDefaults(sd.Key, sd.Desc, schema); err != nil {
			log.Printf("[tools] refresh %s desc failed: %v", sd.Key, err)
		}
	}
	_ = s.m.pg.SetSetting(flag, "true")
	log.Printf("[tools] orchestration/platform ツールの schema をコードのデフォルトへ更新しました（1 回限り）")
}

// refreshToolDescriptionsJa rewrites every built-in tool's model-facing description and
// parameter descriptions to the current (Japanese) code defaults ONCE. SeedTool is
// first-insert-only, so rows seeded with the former Chinese text never change otherwise.
// Like refreshBuiltinToolSchemas this keeps each tool's agent binding and enabled flag,
// but it does replace descriptions a user edited by hand; bump the flag only deliberately.
func (s *Server) refreshToolDescriptionsJa() {
	const flag = "tool_descriptions_ja_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	refresh := func(name, desc string, schema any) {
		raw, err := json.Marshal(schema)
		if err != nil {
			log.Printf("[tools] refresh %s: %v", name, err)
			return
		}
		if err := s.m.pg.RefreshToolDefaults(name, desc, raw); err != nil {
			log.Printf("[tools] refresh %s: %v", name, err)
		}
	}
	for _, sd := range agent.BuiltinToolSeeds() {
		refresh(sd.Key, sd.Desc, sd.Schema)
	}
	var tools []actool.CoreTool
	tools = append(tools, s.orchestrationTools()...)
	tools = append(tools, s.platformTools()...)
	tools = append(tools, s.findingRetestTools()...)
	tools = append(tools, s.toolUpdateFindingReport(), s.toolGetFindingTraffic(), s.toolBindFindingTraffic())
	tools = append(tools, traffic.SeedToolMetas()...)
	for _, t := range tools {
		refresh(t.Name(), t.Description(), t.InputSchema())
	}
	// The two seeded custom agents are renamed only while they still carry the original
	// Chinese name and description, so a user's own edits are left alone.
	for _, r := range [][4]string{
		{"reporter", "报告撰写", "漏洞详细报告撰写：发现漏洞时自动触发，查取证据与执行过程后写 Markdown 报告并回写。", "レポート作成|脆弱性の詳細レポート作成：脆弱性の検出時に自動で起動し、証拠と実行過程を調べて Markdown のレポートを書き、書き戻します。"},
		{db.FindingRetestAgentKey, "漏洞复测", "从漏洞详情手动启动，读取原证据并保存独立复测结论。", "脆弱性の再テスト|脆弱性の詳細から手動で開始し、元の証拠を読み取って、独立した再テストの結論を保存します。"},
	} {
		to := strings.SplitN(r[3], "|", 2)
		if _, err := s.m.pg.Exec(`UPDATE agents SET name=$4, description=$5 WHERE key=$1 AND name=$2 AND description=$3`, r[0], r[1], r[2], to[0], to[1]); err != nil {
			log.Printf("[agents] rename %s: %v", r[0], err)
		}
	}
	_ = s.m.pg.SetSetting(flag, "true")
	log.Printf("[tools] 組み込みツールの説明を日本語のデフォルトへ更新しました（1 回限り）")
}

// unbindGoalMetDefault removes goal_met's default "planner" binding ONCE (guarded by
// a settings flag), so existing DBs match the new default of NO agent. goal_met bypasses
// per-goal prove_goal to declare the whole task done — powerful/risky and redundant with
// the prove_goal→auto-complete path — so it ships unbound; users can re-bind it per agent
// in the UI. A user's own binding to another agent is untouched (we only strip planner).
func (s *Server) unbindGoalMetDefault() {
	const flag = "goal_met_unbind_default_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.RemoveAgentFromTool("planner", "goal_met"); err != nil {
		log.Printf("[tools] goal_met から planner の関連付け解除に失敗しました: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// reseedGoalsPrompt は goals の目標分解プロンプトを【現在のコードのデフォルト】へ戻す。デフォルト本文に
// 「先に操作制約を抜き出して（set_constraints）から目標を分解する」ステップが増えた。SeedPromptIfEmpty は初回挿入だけなので、既存 DB の
// 既存の version 1 はこのステップを受け取れない。ここではバージョン管理で【新しいバージョンを 1 つ追加】して切り替える（ResetPromptToDefault）。
// 古いバージョンは履歴に残る。ユーザーがカスタムしていればバージョン記録から戻せる。settings の flag で守る → 1 回だけ。
// 以後デフォルトが変わったらこの flag を bump する。新規 DB は処理しない（SeedPromptIfEmpty が最新のデフォルトを seed 済み）。
func (s *Server) reseedGoalsPrompt() {
	const flag = "goals_prompt_constraint_step_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 成功しても失敗しても 1 回だけ試す
	a, err := s.m.pg.GetAgentByKey("goals")
	if err != nil || a == nil {
		return // 新規の DB で agent 行がまだないときは、seedPrompts が最新のデフォルトを直接 seed するので、この移行は不要
	}
	tmpl := agent.BuiltinPromptSeeds()["goals"]
	if tmpl == "" {
		return
	}
	// 新規の DB では seedPrompts が最新のデフォルトを seed 済み → 現在のバージョンはコードのデフォルトと等しいので、重複バージョンを足さない。
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] goals プロンプトを新しいデフォルトへ更新するのに失敗しました: %v", err)
		return
	}
	log.Printf("[prompts] goals プロンプトに新しいデフォルト版を追加しました（操作制約を抽出する手順を追加。1 回限り）")
}

// reseedMainAgentPrompt は mainagent のプロンプトを【現在のコードのデフォルト】へ戻す。デフォルト本文に「目標がすべて
// 達成されたあと add_intent でインテントを直接投入するとき、正式な目標として登録するか人に聞き返す」案内が増えた。SeedPromptIfEmpty は初回挿入
// だけなので、既存 DB のバージョンはこれを受け取れない。バージョン管理で【新しいバージョンを 1 つ追加】して切り替える（ResetPromptToDefault）。古いバージョンは
// 履歴に残り、ユーザーがカスタムしていればバージョン記録から戻せる。settings の flag で守る → 1 回だけ。新規 DB は処理しない
// （SeedPromptIfEmpty が最新のデフォルトを seed 済み）。reseedGoalsPrompt と同じ形である。
func (s *Server) reseedMainAgentPrompt() {
	const flag = "mainagent_prompt_goalless_intent_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 成功しても失敗しても 1 回だけ試す
	a, err := s.m.pg.GetAgentByKey("mainagent")
	if err != nil || a == nil {
		return // 新規の DB で agent 行がまだないときは、seedPrompts が最新のデフォルトを直接 seed するので、この移行は不要
	}
	tmpl := agent.BuiltinPromptSeeds()["mainagent"]
	if tmpl == "" {
		return
	}
	// 新規の DB では seedPrompts が最新のデフォルトを seed 済み → 現在のバージョンはコードのデフォルトと等しいので、重複バージョンを足さない。
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] mainagent プロンプトを新しいデフォルトへ更新するのに失敗しました: %v", err)
		return
	}
	log.Printf("[prompts] mainagent プロンプトに新しいデフォルト版を追加しました（目標達成後に目標作成を尋ねる。1 回限り）")
}

// reseedPlannerPrompt は planner のプロンプトを【現在のコードのデフォルト】へ戻す。デフォルト本文を短く組み直し、「抑制」は
// 重複排除だけに下げ、「深さはカバー率より優先」「硬い下限。目標が未達で実行中のインテントがなければ必ず出す」を足し、否定の結論の再確認に上限を付けた。
// デフォルトに実質の変更があるたびに下の flag を bump する（現在 v2）。既存 DB をもう一度戻す。SeedPromptIfEmpty は初回挿入だけなので、既存 DB のバージョンは受け取れない。バージョン管理で
// 【新しいバージョンを 1 つ追加】して切り替える（ResetPromptToDefault）。古いバージョンは履歴に残る。ユーザーがカスタムしていればバージョン記録から
// 戻せる。settings の flag で守る → 1 回だけ。新規 DB は処理しない（SeedPromptIfEmpty が最新のデフォルトを seed 済み）。
// reseedGoalsPrompt と同じ形である。
func (s *Server) reseedPlannerPrompt() {
	const flag = "planner_prompt_compact_realistic_v2"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 成功しても失敗しても 1 回だけ試す
	a, err := s.m.pg.GetAgentByKey("planner")
	if err != nil || a == nil {
		return // 新規の DB で agent 行がまだないときは、seedPrompts が最新のデフォルトを直接 seed するので、この移行は不要
	}
	tmpl := agent.BuiltinPromptSeeds()["planner"]
	if tmpl == "" {
		return
	}
	// 新規の DB では seedPrompts が最新のデフォルトを seed 済み → 現在のバージョンはコードのデフォルトと等しいので、重複バージョンを足さない。
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] planner プロンプトを新しいデフォルトへ更新するのに失敗しました: %v", err)
		return
	}
	log.Printf("[prompts] planner プロンプトに新しいデフォルト版を追加しました（簡略化、抑制的な降格と重複除去、深さ優先、否定の再確認の上限。1 回限り）")
}

// reseedWorkerPrompt は worker のプロンプトを【現在のコードのデフォルト】へ戻す。デフォルト本文の record_fact から「否定の結論は
// 観察と試しの読み方を書く」文を消し、confidence（observed/inferred）と「このインテントの手段を尽くしたか」を切り離した（プランナーを誤らせやすい）。
// 同時に facts 配列の分割を「互いに完全に独立し、まとめられない」ごく少数の例外へ絞った。flag を v3 に bump し、既存 DB をもう一度戻す。
// SeedPromptIfEmpty は初回挿入だけなので、既存 DB のバージョンは受け取れない。バージョン管理で【新しいバージョンを 1 つ追加】して切り替え、古いバージョンは履歴に残して戻せる。
// settings の flag で守る → 1 回だけ。新規 DB は処理しない。reseedGoalsPrompt と同じ形である。
func (s *Server) reseedWorkerPrompt() {
	const flag = "worker_prompt_compact_v4"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 成功しても失敗しても 1 回だけ試す
	a, err := s.m.pg.GetAgentByKey("worker")
	if err != nil || a == nil {
		return // 新規の DB で agent 行がまだないときは、seedPrompts が最新のデフォルトを直接 seed するので、この移行は不要
	}
	tmpl := agent.BuiltinPromptSeeds()["worker"]
	if tmpl == "" {
		return
	}
	// 新規の DB では seedPrompts が最新のデフォルトを seed 済み → 現在のバージョンはコードのデフォルトと等しいので、重複バージョンを足さない。
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] worker プロンプトを新しいデフォルトへ更新するのに失敗しました: %v", err)
		return
	}
	log.Printf("[prompts] worker プロンプトに新しいデフォルト版を追加しました（文脈の確認は list_assets/list_findings に絞り、list_facts/node_detail/asset_neighbors を外した。1 回限り）")
}

// reporterToolCallMessage は、レポートを書く前に get_finding_traffic を 1 回読むことを無条件に求める。
// このツールは読み取り専用で、「取得スイッチには依存しない」。自動関連付けがオフでも、人手で関連付けた証拠は読める。ここで
// 「自動関連付けが有効なときだけ読む」と書くと、デフォルトがオフの設定では reporter が evidence_version を渡さない。
// SetFindingReportVersionByNodeID は legacy の意味で -1 を書き、脆弱性の詳細と Markdown のエクスポートは
// ずっと「証拠が変わり、レポートの更新待ち」のままになる。UI にはそれを消す入口がない。
const reporterToolCallMessage = "直前に report_finding で脆弱性が 1 件登録されました。戻り JSON の finding_id（独立した脆弱性記録の ID）と finding_node_id（探索ノードの ID）を読んでください。" +
	"まず get_finding_traffic(finding_id) で、現在の証拠一覧と version を読み取ります（一覧が空なのは正常です。その場合も通常どおりレポートを書いてください）。" +
	"実行ガイドで自動関連付けが有効なら、読み取る前に、今回の脆弱性のトラフィックを確認して関連付けてください。ノードの詳細には finding_node_id を使います。" +
	"最後に update_finding_report(finding_id=finding_node_id, report, evidence_version=実際に読み取った version) で保存してください。" +
	"evidence_version は必ず渡してください。渡さないと、レポートは更新待ちのままになります。2 種類の番号を混用しないでください。"

// 旧版のトリガーメッセージ（0.3.8 以前）。これと 1 文字も違わない記録だけを移行で上書きする。ユーザーが変えたものはそのまま残す。
const reporterToolCallMessageV1 = "上面刚有一个漏洞被 report_finding 登记。请从触发上下文里取出 finding_id" +
	"（工具返回 \"finding recorded: <id>\" 里的数字）与任务 id，按你的职责撰写该漏洞的详细报告，" +
	"最后调用 update_finding_report(finding_id, report) 保存。"

// upgradeReporterTriggerMessage は、既存 DB でまだデフォルト文面の reporter トリガーメッセージを新しい版へ戻す。
// seedReporterAgent は reporter_agent_seed_v1 で守られ、agent を新規作成するときだけトリガーを書く。そのため
// アップデートした DB は新しい文面を受け取れない。ツール schema は seedFindingTrafficTools が
// evidence_version を補ったが、reporter にそれを使えと伝えるものがない。1 回限りで、変更されていない文面だけを上書きする。
func (s *Server) upgradeReporterTriggerMessage() {
	const flag = "reporter_trigger_evidence_version_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 1 回だけ試す
	triggers, err := s.m.pg.ListTriggersFor("reporter")
	if err != nil {
		log.Printf("[reporter] トリガーの読み取りに失敗しました: %v", err)
		return
	}
	for _, t := range triggers {
		if !t.OnToolCall || t.ToolCallMessage != reporterToolCallMessageV1 {
			continue // ユーザーが変えた、または finding トリガーではない。触らない。
		}
		t.ToolCallMessage = reporterToolCallMessage
		if err := s.m.pg.UpdateTrigger(t); err != nil {
			log.Printf("[reporter] トリガーメッセージの更新に失敗しました: %v", err)
			return
		}
		log.Printf("[reporter] トリガーメッセージを、evidence_version を読んで返す内容に更新しました")
	}
}

// seedReporterAgent は「レポート作成」のカスタム agent を用意する（builtin=false。UI で編集と削除ができる）。
// update_finding_report とタスク照会ツールを関連付け、「report_finding が呼ばれたら起動する」
// トリガーを付ける。脆弱性が 1 件登録されるたびに、詳細レポートを書かせる。1 回限り（settings の flag で守る）。ユーザーが消したら再作成しない。
// 依存。orchestration ツールは本関数の上で SeedTool 済みなので、関連付けできる。
func (s *Server) seedReporterAgent() {
	const flag = "reporter_agent_seed_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // 成功しても失敗しても 1 回だけ試す

	if exist, _ := s.m.pg.GetAgentByKey("reporter"); exist != nil {
		return // key は使用中（ユーザーが手で作った）。上書きしない
	}
	a, err := s.m.pg.CreateAgent("reporter", "レポート作成",
		"脆弱性の詳細レポート作成：脆弱性の検出時に自動で起動し、証拠と実行過程を調べて Markdown のレポートを書き、書き戻します。")
	if err != nil {
		log.Printf("[reporter] agent の作成に失敗しました: %v", err)
		return
	}
	if err := s.m.pg.SeedPromptIfEmpty(a.ID, agent.ReporterDefaultPrompt); err != nil {
		log.Printf("[reporter] seed prompt に失敗しました: %v", err)
	}
	// トリガー実行の方針。parallel + none。脆弱性 1 件につきレポート 1 件。複数の finding は並行でそれぞれ書く。
	// merge は none にする。そうしないと（デフォルトの all）一群の finding が 1 回の実行にまとまり、並行の意味がなくなる。
	// maxParallel=5。同時のレポートセッションは最大 5。瞬間的に LLM 呼び出しが多くなりすぎないようにする。
	if err := s.m.pg.SetAgentTriggerBehavior("reporter", "parallel", "none", 5); err != nil {
		log.Printf("[reporter] トリガー実行方針の設定に失敗しました: %v", err)
	}
	// 必要なツールを関連付ける。レポートを書く、証拠と実行過程と状況を読む。
	if err := s.m.pg.AddAgentToToolBinding("reporter", []string{
		"update_finding_report", "get_task_node_detail", "list_task_findings",
		"get_task_worker_trace", "list_task_worker_traces", "search_task_worker_traces",
		"get_task_graph",
	}); err != nil {
		log.Printf("[reporter] ツールの関連付けに失敗しました: %v", err)
	}
	// トリガー。report_finding が呼ばれたら起動する（ツールは "finding recorded: <id>" を返し、finding_id が付く。
	// タスク id もトリガーメッセージにある）。
	if _, err := s.m.pg.CreateTrigger(&db.AgentTrigger{
		AgentKey:        "reporter",
		Enabled:         true,
		OnToolCall:      true,
		ToolNames:       []string{"report_finding"},
		ToolCallMessage: reporterToolCallMessage,
	}); err != nil {
		log.Printf("[reporter] トリガーの作成に失敗しました: %v", err)
	}
	log.Printf("[reporter] 「レポート作成」agent と finding トリガーを用意しました")
}

// seedAutoReportFindingBinding adds "auto" to report_finding's binding ONCE so
// conversation-context agents can call it without requiring an intent_id.
func (s *Server) seedAutoReportFindingBinding() {
	const flag = "auto_report_finding_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("auto", []string{"report_finding"}); err != nil {
		log.Printf("[auto] report_finding のデフォルト関連付けに失敗しました: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedPlannerDefaultBindings adds "planner" to report_finding's binding ONCE
// (guarded by a settings flag), so existing DBs — whose report_finding row was
// seeded as worker-only — also let the planner record findings. Fresh DBs already
// get it via PlannerTools(); this only backfills without overriding a user unbind.
func (s *Server) seedPlannerDefaultBindings() {
	const flag = "planner_report_finding_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"report_finding"}); err != nil {
		log.Printf("[planner] report_finding のデフォルト関連付けに失敗しました: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedPlannerListAssetsBinding adds "planner" to list_assets's binding ONCE
// (guarded by a settings flag), so existing DBs — whose list_assets row was seeded
// as auto/pentest-only — also let the planner query the asset store by DSL. Fresh
// DBs already get it via PlannerTools(); this only backfills without overriding a
// user unbind.
func (s *Server) seedPlannerListAssetsBinding() {
	const flag = "planner_list_assets_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"list_assets"}); err != nil {
		log.Printf("[planner] list_assets のデフォルト関連付けに失敗しました: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedCompanyScopeRebind changes add_company_scope's default binding ONCE on
// existing DBs (guarded by a settings flag): the tool moves off worker and onto
// planner — defining a company's asset scope is a planning/main/auto concern, not
// something a worker does mid-exploration. Fresh DBs already get planner via
// PlannerTools() and lack worker via WorkerTools(); this only backfills old rows.
// One-shot + flag-guarded so a user who later re-binds worker isn't overridden.
func (s *Server) seedCompanyScopeRebind() {
	const flag = "company_scope_rebind_v1" // worker→planner のデフォルト関連付けの切り替え
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"add_company_scope"}); err != nil {
		log.Printf("[planner] add_company_scope のデフォルト関連付けに失敗しました: %v", err)
		return
	}
	if err := s.m.pg.RemoveAgentFromTool("worker", "add_company_scope"); err != nil {
		log.Printf("[worker] add_company_scope の関連付け解除に失敗しました: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedWorkerReadToolsUnbind strips the read-context tools off worker's default
// binding ONCE on existing DBs (guarded by a settings flag): a worker executes one
// intent and writes back — reading facts/companies and listing all workers' traces is
// a planning/main concern, not the executor's. Fresh DBs already lack these via
// WorkerTools(); this only backfills old rows without overriding a user who
// deliberately re-binds worker. Each RemoveAgentFromTool is per-tool +
// membership-guarded, so planner/mainagent bindings of the same tool are untouched.
//
// NOTE: search_all_worker_traces / get_worker_trace / node_detail are intentionally NOT
// unbound — worker owns them for cross-work look-back + node drill-down (see WorkerTools).
// They used to be in this list back when worker lacked them; seedWorkerReadbackRebind
// repairs DBs whose old run stripped them.
func (s *Server) seedWorkerReadToolsUnbind() {
	const flag = "worker_readtools_unbind_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	for _, k := range []string{
		"list_facts", "list_companies", "list_worker_traces",
	} {
		if err := s.m.pg.RemoveAgentFromTool("worker", k); err != nil {
			log.Printf("[worker] %s を worker から外せませんでした: %v", k, err)
			return // 失敗したら flag を書かない。次回起動で再試行する
		}
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedWorkerReadbackRebind re-binds the cross-work look-back / drill-down tools onto
// worker ONCE (guarded by a settings flag): an earlier seedWorkerReadToolsUnbind wrongly
// stripped search_all_worker_traces / get_worker_trace / node_detail from worker after
// they had been added to WorkerTools(), so any DB that ran that migration lost them.
// Fresh DBs already have them via WorkerTools() and this is a harmless no-op there.
// One-shot + flag-guarded so a user who later deliberately unbinds them isn't overridden.
func (s *Server) seedWorkerReadbackRebind() {
	const flag = "worker_readback_rebind_v2" // v2: 追加 node_detail
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("worker", []string{
		"search_all_worker_traces", "get_worker_trace", "node_detail",
	}); err != nil {
		log.Printf("[worker] 振り返り/詳細ツールの再関連付けに失敗しました: %v", err)
		return // 失敗したら flag を書かない。次回起動で再試行する
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedAutoDefaultBindings adds "auto" to the task-op + platform tools' bindings
// ONCE (guarded by a settings flag), so existing DBs whose tool rows were seeded
// before Auto existed still give Auto its default toolset — without re-adding it
// after a user deliberately unbinds.
func (s *Server) seedAutoDefaultBindings() {
	const flag = "auto_default_bindings_v3" // v3: 古い資産ツール名を置き換え、insert_assets/add_company_scope を加える
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	keys := make([]string, 0, len(platformToolKeys)+12)
	for _, t := range s.orchestrationTools() {
		keys = append(keys, t.Name())
	}
	keys = append(keys, platformToolKeys...)
	// 資産ツール。Auto がプラットフォームを操作するとき、資産の参照と登録、企業範囲の管理がよく要る。
	keys = append(keys, "insert_assets", "add_company_scope", "list_assets")
	if err := s.m.pg.AddAgentToToolBinding("auto", keys); err != nil {
		log.Printf("[auto] デフォルト関連付けに失敗しました: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}
