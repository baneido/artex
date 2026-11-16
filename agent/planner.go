package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// Planner is the event-driven LLM planner (docs §4.3): each time the asset or
// exploration graph changes (debounced), it reads the exploration route, queries
// assets, judges whether the task goal is met, and emits 0..N exploration intents
// into the frontier. It is the sole intent generator.
type Planner struct {
	findingRecorder   FindingRecorder
	prov              llm.Provider
	model             string
	tx                *transcript.Store                      // raw LLM conversation persistence (nil = off)
	window            int                                    // context window in tokens (for compaction)
	windowFn          func() int                             // optional dynamic task-chain minimum
	maxTurns          int                                    // max agent turns per run (0 = unlimited)
	killWork          func(intentID int64) error             // engine callback to terminate a running work (nil = off)
	steerWork         func(intentID int64, msg string) error // engine callback to steer a running work mid-run (nil = off)
	proxyAddr         string                                 // recording proxy for WebFetch (empty = direct)
	proxyCACert       string                                 // recording proxy's CA cert path (HTTPS verify)
	webSearch         WebSearchOpts                          // web_search tool backend selection (off by default)
	workDir           string                                 // shared work dir (surfaced in prompt as artifact-output target)
	injectConstraints func() bool                            // resolver: inject task operation constraints into system prompt? (nil = yes)
	nonStreamingFn    func() bool                            // resolver: use non-streaming (Complete) path? (nil = streaming)
	noaEnabledFn      func() bool                            // resolver: use experimental noa compaction? (nil = off)
	maxTokensFn       func() int                             // resolver: per-reply output cap (nil/0 = send no cap)
	compactor         *Compactor                             // cold-node compaction (§7); nil = disabled

	// todos keeps ONE plan-scratchpad per task (keyed by exploration id) so the
	// planner's multi-step plan survives across wake-ups — each Plan() is a fresh
	// session, but the shared store lets it record a serial exploit chain once and
	// dispatch it step-by-step over rounds instead of front-loading it in parallel.
	todoMu sync.Mutex
	todos  map[int64]*actool.TodoStore
}

func NewPlanner(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int) *Planner {
	return &Planner{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns, todos: map[int64]*actool.TodoStore{}}
}

func (p *Planner) SetCompactionWindowResolver(fn func() int) { p.windowFn = fn }

// SetCompactor wires the cold-node compactor (cold-digest §7). Called each
// planner wake-up to advance the round counter, maintain cold stamps, and
// (off the hot path) fold cold nodes into digests. nil = feature disabled.
func (p *Planner) SetCompactor(c *Compactor) { p.compactor = c }

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default).
func (p *Planner) SetNonStreaming(fn func() bool) { p.nonStreamingFn = fn }

func (p *Planner) nonStreaming() bool { return p.nonStreamingFn != nil && p.nonStreamingFn() }

// SetNoaEnabled wires a resolver deciding whether runs use the experimental noa
// context-compression mechanism. nil/unset = off (built-in compaction). Read per
// run so the settings toggle takes effect without rebuilding the agent.
func (p *Planner) SetNoaEnabled(fn func() bool) { p.noaEnabledFn = fn }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (p *Planner) SetMaxTokens(fn func() int) { p.maxTokensFn = fn }

func (p *Planner) maxTokens() int {
	if p.maxTokensFn == nil {
		return 0
	}
	return p.maxTokensFn()
}

func (p *Planner) compactionWindow() int {
	if p.windowFn != nil {
		return p.windowFn()
	}
	return p.window
}

// SetProxy points the planner's WebFetch at the recording proxy plus the CA cert
// it trusts to verify HTTPS through it (empty addr = direct).
func (p *Planner) SetProxy(addr, caCert string) { p.proxyAddr, p.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for the planner (off by default).
func (p *Planner) SetWebSearch(o WebSearchOpts) { p.webSearch = o }

// SetConstraintInject wires a resolver deciding whether this task's operation
// constraints get injected into the planner system prompt. Read per round so the
// settings toggle takes effect without rebuilding the agent. nil = inject (default).
func (p *Planner) SetConstraintInject(fn func() bool) { p.injectConstraints = fn }

// wantConstraints reports whether constraint injection is enabled (default yes).
func (p *Planner) wantConstraints() bool { return p.injectConstraints == nil || p.injectConstraints() }

// todoFor returns the task's persistent planning todo store, creating it on first
// use. Shared across all of this task's planner wake-ups.
func (p *Planner) todoFor(expID int64) *actool.TodoStore {
	p.todoMu.Lock()
	defer p.todoMu.Unlock()
	s := p.todos[expID]
	if s == nil {
		s = actool.NewTodoStore()
		p.todos[expID] = s
	}
	return s
}

// SetKillWork wires the engine's per-work terminate callback so the planner's
// kill_work tool can stop a single running worker.
func (p *Planner) SetKillWork(fn func(intentID int64) error) { p.killWork = fn }

// SetSteerWork wires the engine's per-work steering callback so the planner's
// steer_work tool can inject a mid-run course-correction into a running worker.
func (p *Planner) SetSteerWork(fn func(intentID int64, msg string) error) { p.steerWork = fn }

// renderPlannerTodos formats the persistent planning todo for injection into the
// wake-up prompt (empty when there are no todos yet — first wake-up).
func renderPlannerTodos(items []actool.Todo) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n【あなたの計画 TODO（起床をまたいで残る。前ラウンドであなたが書いたもの）】：\n")
	for _, it := range items {
		mark := map[actool.TodoStatus]string{actool.TodoPending: "☐", actool.TodoInProgress: "▶", actool.TodoCompleted: "✔"}[it.Status]
		if mark == "" {
			mark = "☐"
		}
		b.WriteString(fmt.Sprintf("  %s %s\n", mark, it.Content))
	}
	b.WriteString("これに沿って進めてください。【前提ステップが完了している、または依存する fact が既にある】次の一歩だけにインテントを出してください。TodoWrite で一覧を更新し、fact で満たされたステップは completed にしてください。一覧で pending または in_progress のステップを重複して出さないでください。")
	return b.String()
}

// TriggerEvent describes what concretely caused this planning round to fire, so
// the planner looks first at the actual change instead of re-scanning the whole
// overview. Kind:
//
//	"done"    — a worker finished intent IntentID (its output conclusion is fetched).
//	"finding" — a worker reported a finding on intent IntentID (Detail = 要約).
//	"goal"    — the human (via メイン agent の set_goals) added one OR MORE goals in a
//	            single call (Goals = 今回追加した目標テキスト。1 件以上。set_goals はバッチに対応).
//	"goal_deleted" — the human deleted a goal from 概要の目標管理 (Detail = 削除された目標テキスト).
//	"goal_edited"  — the human edited a goal from 概要の目標管理 (OldGoal→NewGoal のテキスト).
//	"cancelled" — the human deleted intent IntentID (Detail = 削除理由). The intent is
//	            stopped (not deleted) and the reason is attached to it as a fact.
type TriggerEvent struct {
	Kind     string
	IntentID int64
	Detail   string
	Summary  string   // Kind=="cancelled" 専用。削除前に捕まえたインテント要約（真の削除後はノードがなく、再照会できない）
	Goals    []string // Kind=="goal" 専用。今回の set_goals で追加した目標テキスト（1 件または複数）
	OldGoal  string   // Kind=="goal_edited" 専用。変更前の目標テキスト
	NewGoal  string   // Kind=="goal_edited" 専用。変更後の目標テキスト
	Hints    []string // Kind=="hint" 専用。今回の add_hint で追加したヒントテキスト（1 件または複数）
}

// renderTriggers spells out the change(s) that fired this round: for a finished
// worker — which intent + its output conclusion; for a finding — which intent +
// what was found. Empty for time/heartbeat wakes. Reads the store (best-effort;
// a blank field never blocks the round).
func renderTriggers(ts *db.ExplorationStore, evs []TriggerEvent) string {
	if len(evs) == 0 || ts == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n【今回このラウンドを起こした実際の変化（先にここを見て、方向を補うか決めてください）】：")
	for _, ev := range evs {
		switch ev.Kind {
		case "goal":
			if len(ev.Goals) == 1 {
				b.WriteString(fmt.Sprintf("\n- 人（メイン agent）が目標を 1 件追加しました：%s。新しい未達成の目標です。対応するインテントがまだなければ、これに沿って探索方向を補ってください。", ev.Goals[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- 人（メイン agent）が目標を %d 件追加しました：%s。いずれも新しい未達成の目標です。対応するインテントがまだない目標へ、1 件ずつ探索方向を補ってください。", len(ev.Goals), strings.Join(ev.Goals, "；")))
			}
		case "hint":
			if len(ev.Hints) == 1 {
				b.WriteString(fmt.Sprintf("\n- 人（メイン agent）が戦略ヒントを 1 件追加しました：%s。探索グラフに付いています。対応するインテントがまだなければ、これに沿って探索方向を調整または補足してください。", ev.Hints[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- 人（メイン agent）が戦略ヒントを %d 件追加しました：%s。いずれも探索グラフに付いています。1 件ずつ、これに沿って探索方向を調整または補足してください。", len(ev.Hints), strings.Join(ev.Hints, "；")))
			}
		case "goal_deleted":
			b.WriteString(fmt.Sprintf("\n- 人がこの目標を削除しました：%s。この目標は除かれています。残りの目標と方向を再判定してください（この目標へインテントを出す必要はありません）。", ev.Detail))
		case "goal_edited":
			b.WriteString(fmt.Sprintf("\n- 人が目標を変更しました。「%s」から「%s」です。新しい目標に沿って探索方向を調整してください（元の方向がもう当てはまらないなら、出すのを止めてください）。", ev.OldGoal, ev.NewGoal))
		case "finding":
			b.WriteString(fmt.Sprintf("\n- インテント #%d（%s）の worker が finding を 1 件報告しました：%s", ev.IntentID, intentSummary(ts, ev.IntentID), ev.Detail))
		case "cancelled":
			// インテント内容は、削除時に捕まえた Summary を優先する（真の削除後はノードがなく、intentSummary では引けない）。
			sm := ev.Summary
			if sm == "" {
				sm = intentSummary(ts, ev.IntentID)
			}
			b.WriteString(fmt.Sprintf("\n- インテント #%d はユーザーが削除しました。内容は %s、削除理由は %s です。このインテントは削除済みで、もう実行しません。これに沿って計画し直してください。", ev.IntentID, sm, ev.Detail))
		default: // "done"
			b.WriteString(fmt.Sprintf("\n- インテント #%d（%s）の worker が終わりました。出力した結論：%s", ev.IntentID, intentSummary(ts, ev.IntentID), workerOutput(ts, ev.IntentID)))
			if fids := factIDsYielded(ts, ev.IntentID); fids != "" {
				b.WriteString(fmt.Sprintf("。このインテントが新しく生んだ事実 id：%s ", fids))
			}
		}
	}
	b.WriteString("\n（完全な詳細は node_detail / get_worker_output / list_findings で再確認できます。）")
	return b.String()
}

// factIDsYielded lists the fact ids an intent produced this run as "#12、#15", so the
// planner can jump straight to the round's incremental facts. Empty (best-effort) when
// the intent yielded no facts or the lookup fails.
func factIDsYielded(ts *db.ExplorationStore, id int64) string {
	ids, err := ts.FactsYielded(id)
	if err != nil || len(ids) == 0 {
		return ""
	}
	parts := make([]string, len(ids))
	for i, fid := range ids {
		parts[i] = fmt.Sprintf("#%d", fid)
	}
	return strings.Join(parts, "、")
}

// intentSummary reads an intent node's one-line summary (best-effort, "?" on miss).
func intentSummary(ts *db.ExplorationStore, id int64) string {
	n, err := ts.GetNode(id)
	if err != nil || n == nil {
		return "?"
	}
	var p map[string]any
	if json.Unmarshal(n.Payload, &p) == nil {
		if s, ok := p["summary"].(string); ok && s != "" {
			return s
		}
	}
	return "?"
}

// workerOutput returns the finished worker's conclusion for an intent — the last
// 'result' (else 'text') activity's full detail, truncated. Same source get_worker_output uses.
func workerOutput(ts *db.ExplorationStore, id int64) string {
	acts, _, err := ts.ActivityList(&id, 0, 1000)
	if err != nil {
		return "(出力の取得に失敗しました)"
	}
	var pick *db.Activity
	for i := range acts {
		if acts[i].Kind == "result" {
			pick = &acts[i]
		} else if acts[i].Kind == "text" && pick == nil {
			pick = &acts[i]
		}
	}
	if pick == nil {
		return "(この work にはまだ出力記録がありません)"
	}
	out, _ := ts.ActivityDetail(pick.ID)
	if out == "" {
		out = pick.Summary
	}
	return truncOutput(out, 800)
}

// truncOutput caps a worker-output blob so the trigger context doesn't bloat the
// system prompt every round; full text is one get_worker_output call away.
func truncOutput(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + " …（切り詰めています。全文は get_worker_output を見てください）"
}

// renderGraphOverview folds the pre-computed graph_overview snapshot into the
// wake-up prompt so the planner starts each round with the full situation in
// hand — saving the round-trip it would otherwise spend calling the tool. It is
// the exact same JSON graph_overview would return; deeper detail is still one
// tool call away (node_detail / list_facts / …).
func renderGraphOverview(data map[string]any) string {
	b, err := json.Marshal(data)
	if err != nil {
		return "" // fall back to the model calling graph_overview itself
	}
	return "\n\n【このラウンドの状況（graph_overview を先に取得したもので、そのツールを呼んだ戻りと同じです。詳細が必要なら node_detail/list_facts などを必要に応じて呼んでください）】：\n" + string(b)
}

// plannerDefaultTmpl is the built-in EDITABLE body (区分 [A]) of the planner prompt,
// seeded into agent_prompts. Goal is a {{.Goal}} template var; the 中間生成物の出力規約
// tail is code-owned (artifactSpec) and appended by plannerSystem after rendering.
const plannerDefaultTmpl = `あなたは、サイバーセキュリティプラットフォーム上の認可された侵入テストシステムの「プランナー」です。頻繁に起こされます（グラフが変わるたびに起こされます）。責務は、状況を読み、目標を判定し、**まだ覆われていない新しい方向が確かに存在するときだけ**探索インテントを補うことです。あなたはプランナーです。実行者ではありません。このラウンドの成果物は【インテントを生成する、または言い切る】か【目標を判定する】かだけです。plan の中で作業そのものを片付けないでください。

タスク目標：{{.Goal}}

**このラウンドでインテントを何件出すか（先にこれを明確にしてください）**：
- **硬い下限（最優先）**：【目標が未達成】かつ【現在 open も running のインテントが 1 件もない】（frontier_open=0 かつ running_intents が空）なら、このラウンドは目標へ進むインテントを少なくとも 1 件【出さなければなりません】。実行中の work もなく、待ち行列の方向もないときにインテント 0 件は、タスクの停止です。既知の方向が recent_done にしかなくても、下の done/exhausted/blocked の判断に沿って、別の 1 件を開くか、続きを 1 件出してください。
- 硬い下限の外では、**インテント 0 件は正常な結果です。ただし正当な理由が必要です**（「少なく出すほうが安全」を既定にしないでください）。①**覆い済み**：思いついた方向は、まだ open/running のインテントが処理している（言い換えで、既存のインテントを重複生成するのは重大な誤りです）。②**依存を待つ**：次の一歩が、現在実行中の work の成果に依存し、それがまだ出ていない（このとき無理に出すと、下流は前提を得られず空回りします。次に起こされてグラフが更新されてから出してください）。
- 逆に、【未カバーで、実行中の work に依存しない】新しい方向が確かにある、または目標が未達成で範囲内に未テストの面が残っているなら、出してください。インテント 0 件を、手を抜く既定にしないでください。

**起床のたびに行う判断の流れ**：

1. **完全な状況はこのプロンプトの下に付いています**（graph_overview の戻りそのものなので、再呼び出しは不要です）。task（元のタイトルと目標、ルートノード）、資産数、goals と状態、open/running/recent_done のインテント、sites_without_endpoints（エンドポイントのないサイト。まだ探る余地のある方向の手がかり）、facts（探索事実の数。脆弱性とは別の 2 種類）、recent_facts（{id,summary,confidence?}）です。
   - **範囲**：探索ノード（goals/インテント/facts/findings）はこのタスクだけを含みます。**資産グラフはグローバルに共有**されます（複数タスクで同一。資産数は範囲内のグローバルな数で、このタスク固有ではありません）。このタスクに関係しない資産が出たら無視してください。
   - **系譜**：各インテントは parents（上流。どの事実やインテントから派生したか）と yields（下流。どの事実や検出事項を生んだか）を持ち、recent_facts の各件は from_intent を持ちます。これで「どの事実がどの方向から来たか、新しい方向へ総合できるか」を理解してください。
   - **否定や疑義の観察**（recent_facts の「ポートが閉じている」「インジェクションできない」など）は worker の観察であり、定説ではありません。採用する前に node_detail(id) で evidence を見てください。evidence がしっかりしていて、confidence=observed で、手段を尽くしている場合だけ、その方向を一時的に塞がれたと見なします。evidence がない、「そう見える」「1 回探っただけ」、または confidence=inferred のものは【まだ探り切れていない】として扱い、範囲内で他のインテントが覆っていなければ、既定で確認用インテントを 1 件出して、裏付けるか覆してください（**同じ否定方向の再確認は多くても 1 回**です。再確認後も否定で、証拠が妥当なら、その結論を尊重し、もう出さないでください）。
   - **より深い詳細が必要なときだけ、必要に応じて呼びます**。list_facts（ページ分割、新しい順、既定 20、q で絞り込み、before でページ送り、total/has_more 付き）、list_findings（脆弱性のすべて）、node_detail(id)（完全な証拠と詳細。一覧と recent_facts は要約だけ）、list_assets（pull。q 検索、type/company_id/task_id の絞り込み、ページ分割、または id/ids で直接取得）、asset_neighbors。資産はグローバル共有なので、既定で全量を引かないでください。

2. **目標を判定する（中心の責務）**：goals フィールドには目標と状態が既に入っています。いずれかの検出事項や事実が達成を証明している未達成の目標は、prove_goal(goal_id, evidence_id, reason) で met にしてください。**印を付けたものがちょうど最後の未完了目標であるとき、システムはタスク全体の完了を自動判定します**。締めは 1 件ずつの prove_goal だけで進み、ほかに「一括完了」の手段はありません。
   - ⚠️ **数量での受け入れ確認（前倒しで印を押すことは禁止です）**：目標に数量化できる条件（カバレッジ X%、flag を N 個、ある権限の取得）があるとき、prove_goal の前に【必ず】上の graph_overview の実測値（coverage.pct、findings_total の件数など）を照合してください。基準に達していなければ prove_goal は【禁止】です。差分を埋めるインテントを出してください。「おおむね達成」「中核は取れた」を理由に、前倒しで met にしないでください。例：カバレッジ 100% が要求され、実測 coverage.pct=40% なら未達成です。補測のインテントを出し続けてください。

3. **（任意。開始時だけで、ごく軽量）探って理解する**：グラフに fact がほとんどまだなく（recent_facts がほぼ空で、タスクが始まったばかり）、状況だけでは最初のインテントを具体的に言えないときだけ、Bash などで目標へごく少量の読み取り専用の探査をしてください（例：curl を 1 から 2 回、トップページやフィンガープリントを見る）。**合法な成果物は、より正確なインテント記述の 1 文だけです**。脆弱性の発見、検証、悪用ではなく、エンドポイント、ディレクトリ、パラメータの列挙結果でもありません（それらは worker の仕事なので、インテントに書いて出してください）。硬い境界は 3 つです。
   - グラフに worker が生んだ fact が既にある（facts>0、または recent_facts が空でない）なら、自分で再び探査することは【禁止】です。判断は既存の fact に基づき、このラウンドの成果物は「新しいインテントを出す」か「終わる」かだけです。ある手がかりを深く掘りたいなら、インテントを出して worker に調べさせ、自分で curl しないでください。
   - 開始時でも探査は多くても 3 回で手を止め、最初のインテントを言い切るためだけにしてください。自分が「方向を素早く決める」のではなく「深く調べている」（エンドポイントやディレクトリを 1 つずつ列挙する、id を 1 つずつ試す、デコードの連鎖、同じ API を繰り返し探る、インジェクション、権限越え、脆弱性のテスト検証。これらはすべて worker の重い仕事です）と気づいたら、すぐ手を止めてインテントに書いてください。
   - 既存の事実や状況から判断できるなら、探査は不要です。

4. **どの新しい方向を補うか決める**：**ここでの「自制」は【既存のインテントを繰り返さない】ことだけを指します。「少なく出せるなら少なく出す」ではありません**。目標が未達成のとき、既定で問うのは「目標へ近づくために、まだ覆われていない、より深く、より強い打ち方は何か」であり、「締めてよいか」ではありません。インテントは【開いた探索方向】です（固定の種類やメニューではありません）。既知の事実、資産、目標と合わせて方向を自分で判断し、open、running、recent_done と 1 件ずつ比べてください。
   - open/running が既に覆っているなら、もう生成しないでください（処理中です）。
   - recent_done に出たことがあるなら、**まずそのインテントの state（各件に付いています）で、どう止まったかを見分けてから決めてください**。
     · **done（正常に走り切った）**：覆い済みです。同じものを再送しないでください。行き止まりかどうかは state ではなく、yields した fact の結論で見てください。【実質的に新しい仕組み】（新しい事実、資産、パラメータ、明らかに違う打ち方）が出たときだけ再送し、summary に前回との違いを書いてください。言い換えや「もう一度やれば通るかもしれない」は数えません。再試行は禁止です。
     · **exhausted（予算を使い切り、途中で切られ、一部だけ書き戻した）/ blocked（モデルまたはネットワークの失敗で、ほぼ探れていない）**：どちらも途中で終わっておらず、情報が足りません。先に get_worker_trace / get_worker_output で、実際にどこまでやり、どこで止まったかを見てから、次から選んでください。突破に近くて予算で切られたなら、「前回の進捗の続き」を出します。純粋な外部障害で走れていない（blocked に多い）なら、同じ方向をそのまま再送します。毎回同じ箇所で止まるなら、打ち方や方向を変えます。根拠は常に trace の実際の進捗であり、state そのものではありません。
   - どのインテントにも覆われていない完全に新しい方向なら、生成してください。
   - 既知の方向がすべて、まだ open/running のインテントに覆われているなら、生成せずそのまま終了してください（実行中または待ち行列の work があるので、それらの進行を待ちます）。ただし覆っているのが recent_done だけで、open/running がなく、目標が未達成なら、先頭の硬い下限に従い、別のものを開くか続きを出さなければなりません。
   - **深さはカバレッジより優先します**：coverage は下限であり受け入れ項目であって、探索目標そのものではありません。価値の高い入口（RCE、権限昇格、データ流出へつながり得るもの）を見つけたら、カバレッジを平らにするために広く浅く資産を測るのではなく、その道を【深く突き破る】インテントを優先して出してください。
   - **経路の多様性を保ち、早く収束させないでください**：目標が未達成のとき、既存インテントが同じ経路や入口に集まっていて、【本質的に異なる】未カバーの方向（別の入口面、別種の資産、別の利用チェーン）があるなら、同じ線に同義のインテントを足すのではなく、その分岐を優先して補ってください（文言ではなく実質の差を見てください）。その分岐が既存インテントに覆われているなら、やはり生成しないでください。理想は、仕組みの異なる経路が 2 から 3 本並ぶことです（例：「アップロードの連鎖から攻める」と「認証回避から攻める」）。ある経路が【目標への接近】の証拠を出してから、資源をそこへ集中します。**ただし多様性は、常に先頭の【操作の制約】に従います**。制約が除外した入口面、ポート、ホスト、操作には、本質が違ってもインテントを生成しないでください。

   **直列の利用チェーンは、段階に分けて出し、並列に割らないでください。** 強い依存のある直列チェーン（①の次が②、その次が③。後の一歩は前の一歩の実際の成果に依存する）は、一度に並列で出さないでください（下流は、まだ存在しない前提を得られず、繰り返しや空回りになります）。TodoWrite でチェーン全体を TODO にし（1 ステップ 1 件）、このラウンドは「前提が満たされた」ステップだけを出します（通常は最初の一歩です）。それが fact を出したあと、次に起こされたとき（プロンプトに TODO 一覧が付きます）に次の一歩を出し、満たされたものは completed にしてください。「同じこと」を 2 件に割らないでください（「発動点を確認する」と「発動点を発動する」は同じ一歩です）。【並行で、互いに依存しない】次元（関係のないエンドポイントを複数列挙するなど）だけ、複数インテントを並列に使ってください。

5. **提出**：絞り込んだ新しい方向を、add_intent の【1 回】でまとめて提出してください（intents 配列。価値の高いもの最大 4 件。1 件ずつ何度も呼ばないでください）。
   - **summary**：その方向を自然言語の 1 文で書く（テスト目標の完全なアドレス、何をするか、なぜか）。固定分類にはめないでください。重複排除は、主にこれと既存インテントの比較で行います。
   - **asset_ids**：この方向でテストまたは攻撃する対象資産 id（できるだけ渡す。0 件、1 件、複数。list_assets から）。方向が具体的な資産（サイト、API、パラメータ、ホスト）を中心にするなら必ず渡してください。カバレッジの重複排除と、資産チェーンへの接続に使います。複数資産にまたがるならすべて渡してください。純粋なグローバル偵察で具体的な資産がないときだけ空にします。
   - **parent_ids**：この方向がどの上流ノードを総合して導かれたか（任意。0 件、1 件、複数）。複数の事実が合わさって 1 つのインテントになるならすべて渡してください。ある上流のインテントや検出事項から派生したなら、その id も渡してください。最上位のまったく新しい方向は空にしてください。

繰り返しや無理な埋め合わせはしないでください。ただし目標が未達成で、未カバーかつより深い打ち方があるなら、出すべきときは出してください。簡潔に、焦点を絞り、効率よく進めてください。`

func plannerSystem(goal, dataDir, workDir string) string {
	body := renderSystem("planner", plannerDefaultTmpl, PlannerVars{Goal: goal, DataDir: dataDir, Now: nowStr()})
	return body + artifactSpec(workDir)
}

// Plan runs one planning round. emit, if non-nil, receives the planner's execution
// steps (so users can see how it reads the situation and judges goals — the
// planner is the intent generator and was previously a black box). Returns whether
// the planner judged the goal met.
// triggers carries the concrete change(s) that fired this round — worker(s) done
// and/or finding(s) reported (may be several — the engine debounces a burst; empty
// for time/heartbeat wakes). They are spelled out at the top of the prompt so the
// planner looks first at the actual change (which intent, its output/finding).
func (p *Planner) Plan(ctx context.Context, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, goal string, triggers []TriggerEvent, emit func(db.Activity)) (met bool, reason string, err error) {
	// cold-digest §2.3/§7: advance this task's planner-round counter, maintain the
	// cold_since_round stamps, and (if a threshold is hit) kick off background
	// compaction. Synchronous part is cheap (a few queries); the LLM compaction
	// runs in a detached goroutine so it never adds latency to this round.
	p.compactor.OnPlannerRound(ctx, ts)
	tsx := NewToolSet(ts, "planner")
	tsx.SetFindingRecorder(p.findingRecorder)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetTaskID(taskID)
	tsx.SetCoverageEnabled(as == nil || as.CoverageEnabled(taskID))
	tsx.killWork = p.killWork   // enable kill_work tool (nil = unavailable)
	tsx.steerWork = p.steerWork // enable steer_work tool (nil = unavailable)
	if origin, _ := ts.OriginFactID(); origin > 0 {
		tsx.SetOwnerNode(origin) // planner-side anchors default to the task root (origin fact)
	}
	// ドメインツールと、基本の既定ツールセット（Read/Write/Edit/MultiEdit/LS/Glob/Grep/Bash）
	// 資産カバレッジ機能がオフのときは add_task_scope/list_untested_assets を除く（プロンプトに入れない）。
	base := append(tsx.DropCoverageTools(tsx.PlannerTools()), actool.DefaultTools()...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts)})
	tools, def, cleanup := AugmentTools(ctx, "planner", base)
	defer cleanup()
	// 重要な状況（完了したばかりのインテントと、先に取った完全なグラフ）は【このラウンドの user 入力】へ移す（下の input）。system
	// には静的な計画本文だけを残す。移すことで system はラウンドごとに安定し、キャッシュに有利になる。代償として、1 ラウンドが長くなると状況が
	// compaction で圧縮され得る（planner の 1 ラウンドは通常短いので、リスクは低い）。situational は下の input に連結する。
	situational := renderTriggers(ts, triggers) + renderGraphOverview(tsx.graphOverviewData())
	// タスク単位の deadline と終局モード（ctx 経由で注入。taskclock.go を参照）。終局のラウンドは、タスクタイムアウト用の
	// planner 締め文を【このラウンドの操作指示】として、このラウンドの user 入力へ連結する（situational に続ける）。最後の
	// 目標判定だけをさせ、新しいインテントは出させない。
	tc := taskClockFrom(ctx)
	if tc.Final {
		situational += "\n\n【タスク終局の締め（このラウンドの特別な指示。上の通常の計画手順を上書きします）】：" + resolveTaskTimeoutWrapup("planner")
	}
	// このタスクの作業ディレクトリ <workDir>/tasks/<taskID> を先に作る。
	taskDir := ensureRunDir(p.workDir, taskID, 0)
	ctx = intercept.WithReviewContext(ctx, taskDir, intercept.ReviewBackground{})
	sysBody := plannerSystem(goal, p.workDir, taskDir)
	if p.wantConstraints() {
		sysBody += constraintBlock(ts) // 操作の制約があればシステムプロンプトへ注入し、探索の境界を定める
	}
	system, boundary := deferredSystem(sysBody, def)
	// planner 自身には壁時計予算がない。deadline があるときは MaxDuration を残り時間まで挟み、実行中の計画ラウンドが
	// タスクの時刻に達したときに締めへ入るようにする（タイムアウトならタスクタイムアウト文、ステップ数なら per-run の文）。
	maxDur, clamped := clampMaxDuration(tc.DeadlineUnix, 0)
	settle := wrapupSettlement("planner", nil)
	if tc.DeadlineUnix > 0 {
		settle = wrapupSettlementForTask("planner", nil, clamped)
	}
	opts := agentcore.Options{
		Provider:        p.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		EnableWebFetch:  true, // 記録プロキシ経由で痕跡を残す。プロキシ CA を読み、MITM が再署名した HTTPS 証明書を検証する
		WebFetchProxy:   p.proxyAddr,
		WebFetchCACert:  p.proxyCACert,
		// ネット検索（任意）。ddgs は key 不要。brave-free は BraveKey、tavily は TavilyKey が必要。
		// WebSearchProxy は独立した出口プロキシ（http/https/socks5）で、トラフィックを記録する MITM プロキシとは別。空なら直接接続。
		EnableWebSearch:       p.webSearch.Enabled,
		WebSearchBackend:      p.webSearch.Backend,
		BraveSearchAPIKey:     p.webSearch.BraveKey,
		TavilySearchAPIKey:    p.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: p.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  p.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   p.webSearch.DeepSeekModel,
		WebSearchProxy:        p.webSearch.Proxy,
		BashEnv:               proxyEnv(p.proxyAddr, p.proxyCACert), // Bash の子コマンドは既定でプロキシ経由とし、CA を信頼する
		WorkingDir:            taskDir,                              // このタスクの作業ディレクトリ <workDir>/tasks/<taskID>
		ToolOutputDir:         cmdOutDir(taskDir),
		MaxTurns:              p.maxTurns, // 0 = unlimited (configurable in agent management)
		MaxDuration:           maxDur,     // 0=無制限。deadline があるときは deadline までの残り
		Compaction:            compactionConfig(p.compactionWindow()),
		// 起床をまたいで共有する計画 TODO。直列チェーンを複数ラウンドのあいだ残す（session は新しく、store はそうではない）。
		Todos: p.todoFor(ts.ID()),
		// 【このラウンド】のステップ数予算に達すると SDK が締めを実行する。このラウンドで固めた結論を保存する（出すべき add_intent、
		// 証明できる prove_goal、直列チェーンの TodoWrite）。計画を止めるのではない。planner はその後も繰り返し起こされる。
		// clamped（タスク deadline に挟まれた）のときは PromptByReason を使う（wrapupSettlementForTask を参照）。
		Settlement:   settle,
		NonStreaming: p.nonStreaming(), // この profile が非ストリーミングを選んだときは Provider.Complete を使う
		MaxTokens:    p.maxTokens(),    // 0 = 上限を送らない。サーバーの既定値に任せる
	}
	if p.tx != nil { // persist raw LLM conversation; one accumulating file per task's planner
		opts.Transcript = p.tx
		opts.SessionID = fmt.Sprintf("exp%d-planner", ts.ID())
	}
	// 実験機能。有効にすると noa がコンテキスト圧縮を担う（アーカイブは <workDir>/noa/<SessionID> に集まり、永続）。
	noaSession := fmt.Sprintf("exp%d-planner", ts.ID())
	enableNoa(&opts, p.noaEnabledFn, p.workDir, noaSession, noaWarn(noaSession))
	// 状況（完了したばかりのインテントと完全なグラフ）は、今はこのラウンドの user 入力へ連結する（下の input）。user にはほかに
	// 指示と、起床をまたぐ TODO がある（todo はモデル自身の計画メモで、再生成できるので user に置いてよい）。
	// 書き出しは「このラウンドに具体的な変化があるか」で 2 通りに分ける。変化があるときは下の【実際の変化】ブロックを指す。変化がないとき
	// （心拍の定期巡回、hint、再開など）は「グラフが変化した」と偽らず、実行中インテントの再確認を添える。
	lead := "具体的な変化が起きました（下の【今回このラウンドを起こした実際の変化】を見てください）。これに沿って次の一歩を計画してください："
	if len(triggers) == 0 {
		lead = "このラウンドは**定期巡回（心拍の時刻）で、具体的な変化の信号がない**起床です。グラフに新しい変化があるとは限りません。ついでに実行中のインテントを再確認してください。長く進捗がない、または逸れているものは steer_work で軌道修正し、方向が全体として間違っているものは kill_work で損を止めてください。そのあと目標を判定し、方向を補うか決めてください："
		// 心拍、または変化のない起床で、グラフ全体に open も running のインテントもなければ、探索は止まっている（worker が走っておらず、
		// 待ち行列の方向もない）。planner に明示し、このラウンドで新しい方向を補わせる。実行中インテントを見ただけで空回りさせない。
		if active, err := ts.HasActiveIntent(); err == nil && !active {
			lead = "このラウンドは**定期巡回（心拍の時刻）**の起床で、現在 **open も running のインテントが 1 件もありません**。走っている worker もなく、待ち行列の方向もなく、探索は止まっています。このラウンドで、目標へ進み、グラフ上の既存インテントと**重複しない**新しいインテントを 1 件以上**出さなければなりません**（0 件は不可です）。まず下の状況で目標が達成済みか判定し、未達成ならすぐに方向を補ってください："
		}
	}
	input := lead + situational + "\n\n上の状況に基づいて目標を判定してください。目標が【本当に達成された】（目標の成果を得た、または目標の脆弱性を確認した）ときは、prove_goal で 1 件ずつ印を付けてください。**硬い下限：目標がまだ未達成で、現在 open も running のインテントが 1 件もない（frontier_open=0 かつ running_intents が空）なら、このラウンドは目標へ進むインテントを少なくとも 1 件出さなければなりません。このとき待つ実行中の work もなく、待ち行列の方向もなく、インテント 0 件はタスクの停止です。open/running のインテントが進行しているとき、または目標が達成済みのときだけ、このラウンドは新しいインテントを出さなくてよいです。**" +
		renderPlannerTodos(opts.Todos.List())
	// MaxDuration は今、壁時計の時刻になると実行中のツールを中断し、生きている ctx の上でその場で締めへ入る。1 ラウンドの固着が
	// 締めを迂回することはなく、外側の硬い ctx の予備は要らない。ctx が載せるのは pause、kill、shutdown だけである。
	_, _, err = captureRun(ctx, opts, input,
		func(r db.Activity) {
			if emit != nil {
				r.Worker = "planner" // planner activity has no intent_id (it generates them)
				emit(r)
			}
		})
	return tsx.GoalMet, tsx.Reason, err
}
