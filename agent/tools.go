package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
	acperm "github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// compactIntents distills intents to {id, summary, state, asset_ids, parents,
// yields} so the planner sees both the direction and its LINEAGE — parents (the
// upstream nodes it derived from: facts/intents/findings) and yields (the facts/
// findings it produced) — without pulling full payloads. parentsOf/yieldsOf are
// built from the exploration edges in graph_overview.
func compactIntents(ns []*db.Node, parentsOf, yieldsOf map[int64][]int64) []map[string]any {
	out := make([]map[string]any, 0, len(ns))
	for _, n := range ns {
		var p map[string]any
		_ = json.Unmarshal(n.Payload, &p)
		m := map[string]any{"id": n.ID, "summary": p["summary"], "state": n.State}
		if n.Inherited {
			m["source_task_id"] = n.SourceTaskID
			m["inherited"] = true
		}
		// asset_ids is the structured "which assets this direction covers" signal for
		// dedup; fall back to legacy payload keys (target_ids plural, then target_id
		// single) so intents stored before the rename still surface their anchors.
		if tg, ok := p["asset_ids"]; ok && tg != nil {
			m["asset_ids"] = tg
		} else if tg, ok := p["target_ids"]; ok && tg != nil {
			m["asset_ids"] = tg
		} else if tg, ok := p["target_id"]; ok && tg != nil && tg != "" {
			m["asset_ids"] = []any{tg}
		}
		if ps := parentsOf[n.ID]; len(ps) > 0 {
			m["parents"] = ps // 上游：本意图派生自哪些节点（多个事实可共同产生一个意图）
		}
		if ys := yieldsOf[n.ID]; len(ys) > 0 {
			m["yields"] = ys // 下游：本意图产生了哪些事实/发现
		}
		out = append(out, m)
	}
	return out
}

// ToolSet exposes the PG-backed dual graph (asset + exploration) to an LLM agent.
// One ToolSet is created per planner/worker run; per-run signals live here.
type ToolSet struct {
	findingRecorder FindingRecorder
	as              *db.AssetStore   // asset store (optional; nil = asset tools not available)
	cs              *db.CompanyStore // company store (optional)
	ts              *db.ExplorationStore
	worker          string
	taskID          int64 // PG tasks.id; 0 when unknown (tests / orchestrator cross-task reads)
	// coverageDisabled mirrors tasks.coverage_enabled=false. Stored inverted so the
	// zero value (all existing ToolSet constructions) means ENABLED — matching the
	// DB default (true). When true: graphOverviewData drops the coverage block, the
	// auto-scope hook (insertAssets) is skipped, and add_task_scope/list_untested_assets
	// are filtered out of the agent's tool list. The scope field stays regardless.
	coverageDisabled bool
	// ownerNode is the exploration node that writes attach to: assets this run
	// touches get anchored to it as lineage/provenance (NOT visibility — the asset
	// graph is global and shared). Worker = its claimed intent; planner = begin root.
	ownerNode int64
	GoalMet   bool
	Reason    string
	writes    WriteCounts
	// killWork, if set, terminates a running work by intent id (engine callback,
	// wired by the planner). nil = the kill_work tool reports unavailable.
	killWork func(intentID int64) error
	// steerWork, if set, queues a mid-run course-correction for the work running an
	// intent id (engine callback, wired by the planner): the worker injects it before
	// its next tool call and re-plans, without being killed. nil = tool unavailable.
	steerWork func(intentID int64, msg string) error
	// enrich, if set, receives async auto-completion triggers (DNS resolve for a
	// domain, HTTP probe for a site). nil = no engine enrichment.
	enrich EnrichTrigger
	// notify, if set, wakes the task's planner after a graph change that should be
	// re-planned promptly (currently: a new hint). nil = no wake (the hint is still
	// stored and read on the next round triggered by other events). debounced.
	notify func()
	// notifyFinding, if set, wakes the task's planner when this run reports a finding,
	// carrying (intentID, summary) so the round can spell out which intent found what.
	// Wired for workers; nil elsewhere → falls back to notify (bare wake).
	notifyFinding func(intentID int64, summary string)
	// resumeTask, if set, revives the task after a graph change that should make a
	// stopped task run again (currently: set_goals adds a goal). It flips a terminal/
	// paused task back to running and (re)starts the engine loops — a plain notify()
	// can't, because the planner's terminal gate swallows wakes. Wired ONLY for the
	// main agent (human steering); nil for the goals decomposer and workers.
	resumeTask func()
	// notifyGoal, if set, wakes the planner AND records ONE "人新增了 N 个目标：…" trigger
	// for a whole set_goals call (batch-aware — one call, one trigger, not one per goal)
	// so the next round spells out the added goals (instead of the planner having to
	// spot new open goals in the overview). Wired ONLY for the main agent; nil for the
	// goals decomposer (round-0 has no running planner to inform) and workers → those
	// fall back to the bare notify.
	notifyGoal func(texts []string)
	// notifyHint, if set, wakes the planner AND records ONE "人新增了 N 条战略提示：…"
	// trigger for a whole add_hint call (batch-aware — one call, one trigger) so the next
	// round is told the round was fired by a new hint and spells the hint out, instead of
	// the planner having to spot it folded into the graph overview. Wired for the main
	// agent + cross-task orchestration; nil elsewhere → falls back to the bare notify.
	notifyHint func(texts []string)
}

// SetNotifyGoal wires the goal-add trigger callback (see ToolSet.notifyGoal). Set only
// by the main-agent chat, so runtime-added goals are announced to the planner by name.
func (t *ToolSet) SetNotifyGoal(fn func([]string)) { t.notifyGoal = fn }

// SetNotifyHint wires the hint-add trigger callback (see ToolSet.notifyHint). Set by
// the main-agent chat and cross-task orchestration, so a runtime-added hint fires a
// planner round announced by name instead of a bare wake.
func (t *ToolSet) SetNotifyHint(fn func([]string)) { t.notifyHint = fn }

// SetResumeTask wires the task-revive callback (see ToolSet.resumeTask). Set only by
// the main-agent chat, so runtime-added goals can pull a finished task back to running.
func (t *ToolSet) SetResumeTask(fn func()) { t.resumeTask = fn }

// SetNotify wires the planner-wake callback (see ToolSet.notify). Set by callers
// that hold the task handle (main-agent chat, cross-task orchestration).
func (t *ToolSet) SetNotify(fn func()) { t.notify = fn }

// SetNotifyFinding wires the finding-wake callback (see ToolSet.notifyFinding).
func (t *ToolSet) SetNotifyFinding(fn func(int64, string)) { t.notifyFinding = fn }

// EnrichTrigger is the enrichment engine seen from the tool layer (see package
// enrich). Kept as an interface here to avoid coupling agent → enrich.
type EnrichTrigger interface {
	ResolveDomain(id int64, host string)
	ProbeSite(id int64, url string)
}

// WriteCounts breaks down what a worker persisted this run, by node kind, so the
// engine can log an accurate "wrote back" summary instead of lumping assets and
// findings under "facts" (record_fact → Facts, insert_assets → Assets,
// report_finding → Findings; each element of a batch counts once).
type WriteCounts struct {
	Facts    int
	Assets   int
	Findings int
}

// Total is every node persisted this run, regardless of kind — the
// "explored but persisted nothing" signal (Total == 0).
func (w WriteCounts) Total() int { return w.Facts + w.Assets + w.Findings }

// String renders the per-kind breakdown for logs, e.g. "事实1 资产25 漏洞0".
func (w WriteCounts) String() string {
	return fmt.Sprintf("事实%d 资产%d 漏洞%d", w.Facts, w.Assets, w.Findings)
}

// Writes reports what this run wrote back, split by node kind (so the engine can
// tell "explored but persisted nothing" apart from a completed intent, and log an
// honest breakdown instead of calling assets/findings "facts").
func (t *ToolSet) Writes() WriteCounts { return t.writes }

func NewToolSet(ts *db.ExplorationStore, worker string) *ToolSet {
	return &ToolSet{ts: ts, worker: worker}
}

// SetTaskID sets the PG task id on this ToolSet so that report_finding can
// dual-write to the standalone findings table (which survives task deletion).
func (t *ToolSet) SetTaskID(id int64) { t.taskID = id }

// SetCoverageEnabled records whether this task has the asset-coverage feature on
// (default enabled). Passing false makes graphOverviewData omit the coverage block
// and DropCoverageTools filter the two coverage-only tools out of the agent's tool
// list. It does NOT stop scope accumulation: insertAssets' auto-scope hook runs
// either way, because task_scope is the task's range boundary (the filter basis for
// asset queries), not merely a coverage denominator.
func (t *ToolSet) SetCoverageEnabled(enabled bool) { t.coverageDisabled = !enabled }

// CoverageDisabled reports whether the coverage feature is off for this task.
func (t *ToolSet) CoverageDisabled() bool { return t.coverageDisabled }

// coverageOnlyTools are the LLM tools that only make sense when asset coverage is
// on. When the feature is off they are filtered out of the agent's tool list so
// they neither pollute the prompt nor let the model build a disabled denominator.
// add_task_scope is deliberately NOT here: task_scope is the task's range boundary
// (the filter basis for asset queries), not merely a coverage denominator, so the
// agents that own范围定义 keep it either way — in lockstep with insertAssets'
// auto-scope hook, which also runs regardless of the switch.
var coverageOnlyTools = map[string]bool{"list_untested_assets": true}

// DropCoverageTools returns tools with the coverage-only ones removed when this
// task has the feature disabled; otherwise it returns tools unchanged.
func (t *ToolSet) DropCoverageTools(tools []actool.CoreTool) []actool.CoreTool {
	if !t.coverageDisabled {
		return tools
	}
	out := tools[:0:0]
	for _, tool := range tools {
		if coverageOnlyTools[tool.Name()] {
			continue
		}
		out = append(out, tool)
	}
	return out
}

// Cross-task reuse: exported accessors returning the per-task tool logic bound to
// THIS ToolSet's store. Host-side orchestration tools build a ToolSet for an
// arbitrary task, then Call these — so cross-task reads/hint reuse the exact
// same logic as the in-task tools. (readTool ignores ToolContext, so Call(…,nil)
// is safe; add_hint is a writeTool but also doesn't deref the context here.)
func (t *ToolSet) GraphOverviewTool() actool.CoreTool      { return t.graphOverview() }
func (t *ToolSet) ListFindingsTool() actool.CoreTool       { return t.listFindings() }
func (t *ToolSet) GetWorkerTraceTool() actool.CoreTool     { return t.getWorkerTrace() }
func (t *ToolSet) ListWorkerTracesTool() actool.CoreTool   { return t.listWorkerTraces() }
func (t *ToolSet) SearchWorkerTracesTool() actool.CoreTool { return t.searchAllWorkerTraces() }
func (t *ToolSet) NodeDetailTool() actool.CoreTool         { return t.nodeDetail() }
func (t *ToolSet) AddHintTool() actool.CoreTool            { return t.addHint() }

// SetEnrich wires the async enrichment engine (DNS/HTTP auto-completion).
func (t *ToolSet) SetEnrich(e EnrichTrigger) { t.enrich = e }

// SetOwnerNode sets the exploration node that writes anchor to (worker: its
// intent node; planner/main: the begin root). Assets created/referenced while
// ownerNode is set are anchored to it as lineage (not visibility).
func (t *ToolSet) SetOwnerNode(id int64) { t.ownerNode = id }

// anchorOwner records a lineage edge from this run's owner node to an asset
// (no-op if unset). Provenance only — the asset graph is global and shared, so
// this no longer affects which assets a task can read.
func (t *ToolSet) anchorOwner(assetID int64) {
	if t.ts != nil && t.ownerNode > 0 && assetID > 0 {
		_ = t.ts.Anchor(t.ownerNode, assetID)
	}
}

// pid parses an id that may arrive as a JSON number or string ("" / 0 → 0).
func pid(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		v, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		return v
	}
	return 0
}

// pidList parses a list of ids (number|string), dropping zeros/invalids.
func pidList(raw []json.RawMessage) []int64 {
	var out []int64
	for _, r := range raw {
		if v := pid(r); v > 0 {
			out = append(out, v)
		}
	}
	return out
}

func obj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}
func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func intp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}
func idp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

func readTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		ReadOnly:    func(json.RawMessage) bool { return true },
		Concurrent:  func(json.RawMessage) bool { return true },
		Permissions: func(context.Context, json.RawMessage, acperm.Context) acperm.Decision { return acperm.Allowed() },
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

func writeTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		Permissions: func(context.Context, json.RawMessage, acperm.Context) acperm.Decision { return acperm.Allowed() },
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

// readExpTool / writeExpTool build a domain tool whose handler dereferences the
// task-bound ExplorationStore. Two ToolSets carry a nil store: the catalog's
// seed-only shell (never called) and the server-level one behind buildDomainReg,
// which the tools table can bind to ANY agent — including ones that never run
// inside a task (auto/pentest/reporter/自定义 agent/旁路提问). Refusing there
// keeps a mis-bound tool a bad tool call; without the guard it was a nil deref,
// and tool handlers run on the harness's own goroutine, so the panic is out of
// reach of every recover() in the server and kills the whole process.
func (t *ToolSet) readExpTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return readTool(name, desc, schema, t.needExploration(name, run))
}

func (t *ToolSet) writeExpTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return writeTool(name, desc, schema, t.needExploration(name, run))
}

// needExploration wraps a handler so it only runs with an exploration store.
// Tools that degrade more usefully than "unavailable" (report_finding points at
// add_task_hint, set_goals/set_constraints at the task itself) keep their own
// bespoke guard instead.
func (t *ToolSet) needExploration(name string, run func(context.Context, json.RawMessage) (actool.Result, error)) func(context.Context, json.RawMessage) (actool.Result, error) {
	return func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
		if t.ts == nil {
			return actool.Errorf(name + " 需要任务上下文（探索图）：当前 agent 不在某个任务内运行，取不到任务的探索图，该工具不可用。请在任务内使用它，或改用带 task_id 的跨任务读取工具（get_task_node_detail / list_task_findings / get_task_graph 等）。"), nil
		}
		return run(ctx, in)
	}
}

func jsonResult(v any) (actool.Result, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	return actool.Text(string(b)), nil
}

// --- read tools (planner + worker) ---

func (t *ToolSet) graphOverview() actool.CoreTool {
	return t.readExpTool("graph_overview",
		"(探索チェーン図)探索状況を蒸留した要約：資産数、API のないサイト、frontier、検出事項、hints(人間/メイン agent の戦略的なヒント。インテントを生成するときは必ず取り込む)。計画時は最初にこれを呼びます。",
		obj(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			return jsonResult(t.graphOverviewData())
		})
}

// graphOverviewData computes the distilled situational snapshot shared by the
// graph_overview tool and the planner's wake-up prompt (which pre-injects it so
// the model needn't spend a turn calling the tool — every plan round starts with
// an empty context and always needs this first).
func (t *ToolSet) graphOverviewData() map[string]any {
	out := map[string]any{}
	// goals summary folded in so the planner needn't call list_goals each round.
	goals, _ := t.ts.ListByKind(db.KindGoal, 100)
	gsum := make([]map[string]any, 0, len(goals))
	for _, g := range goals {
		var p map[string]any
		_ = json.Unmarshal(g.Payload, &p)
		gsum = append(gsum, map[string]any{"id": g.ID, "state": g.State, "text": p["text"]})
	}
	out["goals"] = gsum
	// hints: 人类/主 agent 通过 add_hint 挂上图的战略提示；folded in so the
	// planner reads them every round when generating intents (否则只写不读).
	hints, _ := t.ts.ListByKind(db.KindHint, 50)
	hsum := make([]map[string]any, 0, len(hints))
	for _, h := range hints {
		var p map[string]any
		_ = json.Unmarshal(h.Payload, &p)
		hint := map[string]any{"id": h.ID, "state": h.State, "text": p["text"]}
		if findingTrafficBindingEnabled() && p["traffic_refs"] != nil {
			hint["traffic_refs"] = p["traffic_refs"]
		}
		hsum = append(hsum, hint)
	}
	out["hints"] = hsum
	// lineage from the exploration edges: an intent's parents (what it
	// derived_from — possibly several facts combined) and its yields (the
	// facts/findings it produced). factFrom maps a fact → the intent that
	// produced it. This is the relationship layer the flat lists lacked.
	edges, _ := t.ts.Edges(5000)
	parentsOf := map[int64][]int64{}
	yieldsOf := map[int64][]int64{}
	factFrom := map[int64]int64{}
	for _, e := range edges {
		switch e.Rel {
		case db.RelDerivedFrom, db.RelSpawns: // upstream: derived_from (fact/finding/intent→intent) or spawns (origin fact→goal, legacy begin→intent)
			parentsOf[e.To] = append(parentsOf[e.To], e.From)
		case db.RelYields: // intent --yields--> fact/finding
			yieldsOf[e.From] = append(yieldsOf[e.From], e.To)
			factFrom[e.To] = e.From
		}
	}
	// cold-digest §6: members folded into an active digest are shown via cold_digests
	// (below), not the flat recent_* lists. `covered` maps member id → its digest id.
	// §6 render-time revival check: a covered member that has become hot again (a new
	// intent derived from it) must reappear this round — so `hidden` folds a member out
	// only when it is covered AND still cold.
	covered, _ := t.ts.CoveredMembers()
	// Render-time hot set (ancestor of a live intent / fact under a live intent).
	// §6 revival check: a covered member that revived (now hot) must NOT stay folded
	// — hidden() only folds a member out when it is covered AND still cold. Computed
	// every round (cheap for real graph sizes); nil map degrades safely.
	var hotAtRender map[int64]bool
	if cg, _, err := loadColdGraph(t.ts); err == nil {
		hotAtRender = cg.hotSet()
	}
	hidden := func(id int64) bool { _, c := covered[id]; return c && !hotAtRender[id] }
	const openIntentsCap = 30
	fr, _ := t.ts.Frontier(openIntentsCap) // priority DESC, id ASC —— 优先级最高的前 N 条；真实总数见 frontier_open
	out["open_intents"] = compactIntents(fr, parentsOf, yieldsOf)
	all, _ := t.ts.ListByKind(db.KindIntent, 300)
	var running, recentDone []*db.Node
	for _, n := range all {
		switch n.State {
		case "running":
			running = append(running, n)
		case "done", "blocked", "exhausted":
			if hidden(n.ID) {
				continue // in a cold_digest and still cold — shown via cold_digests (§6.2)
			}
			recentDone = append(recentDone, n) // 最新在前（all 按 id 降序）；折叠的已剔除，输出时截最新 N
		}
	}
	out["running_intents"] = compactIntents(running, parentsOf, yieldsOf)
	// done_intents_total：已结束意图（done/blocked/exhausted）总数，与 recent_done_intents
	// 平行命名——后者只是它的最新窗口截断视图。两键并排即自描述："看到的是 N/总数"，
	// 让 planner 去重时别把"没显示"当成"没派过"，无需在提示词里另行解释。
	if dt, err := t.ts.CountFinishedIntents(); err == nil {
		out["done_intents_total"] = dt
	}
	// frontier_open：开放意图真实总数（open_intents 只是其中优先级最高的前 N 条截断视图）。
	if fo, err := t.ts.CountOpenIntents(); err == nil {
		out["frontier_open"] = fo
	} else {
		out["frontier_open"] = len(fr)
	}
	// findings (confirmed vulns) and facts (worker exploration results) are
	// now distinct node kinds. recent_facts surfaces fact summaries (esp.
	// negative results) so the planner sees them in one call; full content
	// via node_detail(id).
	vulnNodes, _ := t.ts.ListByKind(db.KindFinding, 1000)
	factNodes, _ := t.ts.ListByKind(db.KindFact, 1000) // newest first
	out["findings_total"] = len(vulnNodes)             // 确认漏洞总数（目标判定看它）；明细见 finding_list（最新一窗）
	out["facts"] = len(factNodes)                      // 探索事实/结论数（含否定结论）
	// findings 是任务里最高价值的产物 → 概览带最新一窗（≤10 条，vulnNodes 已按 id 降序即最新在前），
	// 让 planner 每轮判目标时一眼看到最近确认的漏洞；全量/更早的用 list_findings 取。
	// 每条只留 {id, summary, from_intent?}：from_intent 是产生本漏洞的意图。
	// evidence/assets/vulnclass/severity/state 等仍可用 list_findings / node_detail(id) 取。
	const findingListCap = 10
	findingList := make([]map[string]any, 0, findingListCap)
	for _, n := range vulnNodes {
		if len(findingList) >= findingListCap {
			break
		}
		var fp map[string]any
		_ = json.Unmarshal(n.Payload, &fp)
		m := map[string]any{"id": n.ID, "summary": fp["summary"]}
		if from := factFrom[n.ID]; from > 0 {
			m["from_intent"] = from // 本漏洞由哪个意图产生
		}
		findingList = append(findingList, m)
	}
	out["finding_list"] = findingList
	// recent_facts：非折叠事实里最新的一窗（≤N，factNodes 按 id 降序即最新在前）。已折进
	// digest 且仍冷的（hidden）走 cold_digests，不在此重复。每条 {id, summary, from_intent?,
	// confidence?}；evidence 等详情用 node_detail(id)。更早的用 list_facts 翻。
	const recentFactsCap = 20
	recentFacts := make([]map[string]any, 0, recentFactsCap)
	for _, n := range factNodes {
		if len(recentFacts) >= recentFactsCap {
			break
		}
		if hidden(n.ID) {
			continue // 已折进 digest 且仍冷 —— 见 cold_digests
		}
		m := compactNode(n)
		if from := factFrom[n.ID]; from > 0 {
			m["from_intent"] = from // 本事实由哪个意图产生
		}
		// confidence 带进概览：让规划者一眼看出哪条结论只是 inferred（尤其否定结论
		// 别当铁案）；evidence 较长，留给 node_detail(id)。
		var fp map[string]any
		if json.Unmarshal(n.Payload, &fp) == nil {
			if c, ok := fp["confidence"].(string); ok && c != "" {
				m["confidence"] = c
			}
		}
		recentFacts = append(recentFacts, m)
	}
	out["recent_facts"] = recentFacts
	// recent_done_intents：非折叠的已结束意图里最新的一窗（≤N，recentDone 已按 id 降序）。
	// 更早的看 done_intents_total 计数 + node_detail(id)。
	const recentDoneCap = 12
	if len(recentDone) > recentDoneCap {
		recentDone = recentDone[:recentDoneCap]
	}
	out["recent_done_intents"] = compactIntents(recentDone, parentsOf, yieldsOf)
	// cold-digest §6.1: 折叠冷区的 digest body，按最新成员时间降序取前 N；被截的更旧 digest
	// 只给裸 id（仍可 expand_digest 展开），避免冷区唯一出口被无限拉长。
	const coldDigestsCap = 15
	if cds, more := coldDigestsRecent(t.ts, coldDigestsCap); len(cds) > 0 {
		out["cold_digests"] = cds // [{id, body, member_count}] —— 直接读 body (§6.1)
		if len(more) > 0 {
			out["cold_digests_more"] = more // 被截断的更旧 digest 的 id；用 expand_digest(id) 展开
		}
	}
	// the original task (root) so the planner always has it, not just the
	// decomposed goals.
	if description, goal, err := t.ts.Root(); err == nil {
		out["task"] = map[string]any{"description": description, "goal": goal}
	}
	// Direct source tasks are a live, read-only blackboard view. Keep their
	// summaries in a separate field so their intents never enter this task's
	// frontier or get mistaken for locally claimable work.
	out["related_tasks"] = t.relatedTaskOverviews()
	// coverage：粗略的资产测试覆盖度参考——范围(task_scope)内的资产里，被 fact 碰过的
	// 占比 + by_type(按类型的 总数/已测)。要看未测的具体资产由 agent 按需调 list_untested_assets 自行判断。仅任务上下文有。
	// 资产覆盖度功能关闭时(coverageDisabled)：只保留 host_count(目标主机数的感知信息)，
	// 丢弃 denominator/tested/pct/by_type/note 等覆盖度度量，避免污染上下文、也不诱导
	// 已隐藏的 add_task_scope/list_untested_assets。
	if t.as != nil && t.ts != nil && t.taskID > 0 {
		{
			m := map[string]any{}
			if !t.coverageDisabled {
				if cov, err := t.as.TaskCoverageWithSources(t.taskID); err == nil {
					m["denominator"] = cov.Denominator
					m["tested"] = cov.Tested
					m["by_type"] = cov.ByType
					m["note"] = "coverage资产测试覆盖度（包括接口等各种相关资产），粗略估计、仅供参考：包含当前任务与直接关联任务的 scope、事实锚点；关联 scope 只读。容器型资产/大量枚举会让它偏低，勿据此认为已测完；可用 add_task_scope 增补本任务范围、list_untested_assets 看未测资产【通常不调用list_untested_assets，按照任务推进即可】；"
					if cov.Denominator == 0 {
						m["pct"] = nil
						m["status"] = "范围未锚定"
					} else {
						m["pct"] = cov.Pct
					}
				}
			}
			if hosts, err := t.as.HostsByTaskWithSources(t.taskID); err == nil {
				// 只给主机总数，不再把 host 列表平铺进 graph_overview（大范围任务里那是每轮
				// 都重复携带的大量字符串，对规划决策价值有限）；具体主机按需 list_assets 查。
				m["host_count"] = len(hosts)
			}
			if len(m) > 0 {
				out["coverage"] = m
			}
		}
	}
	return out
}

func inheritedMap(m map[string]any, sourceTaskID int64) map[string]any {
	m["source_task_id"] = sourceTaskID
	m["inherited"] = true
	return m
}

const (
	relatedOverviewTotalTextRunes      = 48_000
	relatedOverviewMaxTextPerSource    = 8_000
	relatedOverviewMaxGoalsPerSource   = 8
	relatedOverviewMaxHintsPerSource   = 6
	relatedOverviewMaxFactsPerSource   = 12
	relatedOverviewMaxFindingsPerTask  = 6
	relatedOverviewMaxIntentsPerTask   = 8
	relatedOverviewMaxScopePerSource   = 12
	relatedOverviewMaxDigestsPerSource = 6
)

// overviewTextBudget bounds inherited prompt text while preserving a fair slice
// for every direct source. Full evidence remains available through the on-demand
// read tools, so truncation here does not discard persisted blackboard data.
type overviewTextBudget struct {
	remaining int
	truncated bool
}

func relatedOverviewBudgetForSources(sourceCount int) int {
	if sourceCount <= 0 {
		return 0
	}
	if sourceCount > db.MaxTaskSourceCount {
		sourceCount = db.MaxTaskSourceCount
	}
	perSource := relatedOverviewTotalTextRunes / sourceCount
	if perSource > relatedOverviewMaxTextPerSource {
		perSource = relatedOverviewMaxTextPerSource
	}
	return perSource
}

func (b *overviewTextBudget) take(value any, fieldLimit int) string {
	var text string
	switch value := value.(type) {
	case string:
		text = strings.TrimSpace(value)
	case nil:
		return ""
	default:
		text = strings.TrimSpace(fmt.Sprint(value))
	}
	if text == "" {
		return ""
	}
	if b.remaining <= 0 || fieldLimit <= 0 {
		b.truncated = true
		return ""
	}
	runes := []rune(text)
	limit := fieldLimit
	if limit > b.remaining {
		limit = b.remaining
	}
	if len(runes) > limit {
		b.truncated = true
		if limit == 1 {
			text = "…"
		} else {
			text = string(runes[:limit-1]) + "…"
		}
		runes = []rune(text)
	}
	b.remaining -= len(runes)
	return text
}

func recentTerminalIntents(store *db.ExplorationStore, limit int) []*db.Node {
	if limit <= 0 {
		return []*db.Node{}
	}
	const batch = 300
	cursor := int64(0)
	out := make([]*db.Node, 0, limit)
	for len(out) < limit {
		page, more, err := store.ListByKindPage(db.KindIntent, cursor, batch)
		if err != nil || len(page) == 0 {
			break
		}
		for _, intent := range page {
			switch intent.State {
			case "done", "blocked", "exhausted", "stopped":
				out = append(out, intent)
			}
			if len(out) >= limit {
				break
			}
		}
		if !more {
			break
		}
		cursor = page[len(page)-1].ID
	}
	return out
}

// relatedTaskOverviews distills persistent blackboard state from direct source
// tasks. It intentionally reads each source's local store methods, never its own
// related sources, so inheritance is one level only.
func (t *ToolSet) relatedTaskOverviews() []map[string]any {
	sources, err := t.ts.DirectSourceStores()
	if err != nil {
		return []map[string]any{}
	}
	if len(sources) > db.MaxTaskSourceCount {
		sources = sources[:db.MaxTaskSourceCount]
	}
	perSourceTextBudget := relatedOverviewBudgetForSources(len(sources))
	out := make([]map[string]any, 0, len(sources))
	for _, source := range sources {
		ts := source.Store
		// §2 cross-task: render the source task's OWN folded view — fold out the
		// members it has already folded, and surface its cold_digests read-only.
		hidden := hiddenMembersFor(ts)
		budget := overviewTextBudget{remaining: perSourceTextBudget}
		item := map[string]any{
			"source_task_id": source.Task.TaskID,
			"inherited":      true,
			"task": map[string]any{
				"description": budget.take(source.Task.Description, 800),
				"goal":        budget.take(source.Task.Goal, 800),
				"status":      source.Task.Status,
			},
		}
		stats, statsErr := ts.Stats()

		edges, _ := ts.Edges(5000)
		parentsOf := map[int64][]int64{}
		yieldsOf := map[int64][]int64{}
		factFrom := map[int64]int64{}
		for _, edge := range edges {
			switch edge.Rel {
			case db.RelDerivedFrom, db.RelSpawns:
				parentsOf[edge.To] = append(parentsOf[edge.To], edge.From)
			case db.RelYields:
				yieldsOf[edge.From] = append(yieldsOf[edge.From], edge.To)
				factFrom[edge.To] = edge.From
			}
		}

		goals, _ := ts.ListByKind(db.KindGoal, relatedOverviewMaxGoalsPerSource)
		goalSummary := make([]map[string]any, 0, len(goals))
		for _, goal := range goals {
			var payload map[string]any
			_ = json.Unmarshal(goal.Payload, &payload)
			goalSummary = append(goalSummary, inheritedMap(map[string]any{
				"id": goal.ID, "state": goal.State, "text": budget.take(payload["text"], 400),
			}, source.Task.TaskID))
		}
		item["goals"] = goalSummary

		hints, _ := ts.ListByKind(db.KindHint, relatedOverviewMaxHintsPerSource)
		hintSummary := make([]map[string]any, 0, len(hints))
		for _, hint := range hints {
			var payload map[string]any
			_ = json.Unmarshal(hint.Payload, &payload)
			hintSummary = append(hintSummary, inheritedMap(map[string]any{
				"id": hint.ID, "state": hint.State, "text": budget.take(payload["text"], 400),
			}, source.Task.TaskID))
		}
		item["hints"] = hintSummary

		facts, _ := ts.ListByKind(db.KindFact, relatedOverviewMaxFactsPerSource)
		findings, _ := ts.ListByKind(db.KindFinding, relatedOverviewMaxFindingsPerTask)
		intentNodes, _ := ts.ListByKind(db.KindIntent, 300)
		terminalIntent := make(map[int64]bool, len(intentNodes))
		for _, intent := range intentNodes {
			terminalIntent[intent.ID] = inheritedIntentSummaryState(intent.State)
		}
		item["facts"] = len(facts)
		item["findings"] = len(findings)
		if statsErr == nil {
			item["facts"] = stats[db.KindFact]
			item["findings"] = stats[db.KindFinding]
			if stats[db.KindGoal] > len(goals) || stats[db.KindHint] > len(hints) ||
				stats[db.KindFact] > len(facts) || stats[db.KindFinding] > len(findings) {
				budget.truncated = true
			}
		}
		recentFindings := make([]map[string]any, 0, len(findings))
		for _, finding := range findings {
			entry := inheritedMap(compactFinding(finding), source.Task.TaskID)
			entry["summary"] = budget.take(entry["summary"], 400)
			recentFindings = append(recentFindings, entry)
		}
		item["recent_findings"] = recentFindings
		recentFacts := make([]map[string]any, 0, len(facts))
		for _, fact := range facts {
			if hidden(fact.ID) {
				continue // folded into this source's cold_digests — shown there (§2/§6.2)
			}
			m := inheritedMap(compactNode(fact), source.Task.TaskID)
			m["summary"] = budget.take(m["summary"], 400)
			if from := factFrom[fact.ID]; from > 0 && terminalIntent[from] {
				m["from_intent"] = from
			}
			var payload map[string]any
			if json.Unmarshal(fact.Payload, &payload) == nil {
				if confidence, ok := payload["confidence"].(string); ok && confidence != "" {
					m["confidence"] = confidence
				}
			}
			recentFacts = append(recentFacts, m)
		}
		item["recent_facts"] = recentFacts

		recentDoneRaw := recentTerminalIntents(ts, relatedOverviewMaxIntentsPerTask)
		recentDone := recentDoneRaw[:0] // in-place filter: drop this source's folded intents (§2)
		for _, intent := range recentDoneRaw {
			if hidden(intent.ID) {
				continue
			}
			recentDone = append(recentDone, intent)
		}
		for _, intent := range recentDone {
			intent.Inherited = true
			intent.SourceTaskID = source.Task.TaskID
		}
		intentResults := compactIntents(recentDone, parentsOf, yieldsOf)
		for i, intent := range recentDone {
			intentResults[i]["summary"] = budget.take(intentResults[i]["summary"], 400)
			acts, _, err := ts.ActivityPageForTerminalIntent(intent.ID, 0, 20)
			if err != nil {
				continue
			}
			var resultSummary, textFallback string
			for _, activity := range acts {
				switch activity.Kind {
				case "result":
					resultSummary = activity.Summary
				case "text":
					textFallback = activity.Summary
				}
			}
			if resultSummary == "" {
				resultSummary = textFallback
			}
			if resultSummary != "" {
				intentResults[i]["result_summary"] = budget.take(resultSummary, 800)
			}
		}
		item["recent_intent_results"] = intentResults
		// §2 cross-task: the source task's folded cold region, read-only, newest-member
		// first & capped like the current task's. Members (and overflow digests) are
		// resolvable via expand_digest(id)/node_detail(id), which search source tasks.
		if cds, more := coldDigestsRecent(ts, relatedOverviewMaxDigestsPerSource); len(cds) > 0 {
			for _, cd := range cds {
				cd["inherited"] = true
				cd["source_task_id"] = source.Task.TaskID
			}
			item["cold_digests"] = cds
			if len(more) > 0 {
				item["cold_digests_more"] = more // 被截断的更旧 digest 的 id；expand_digest(id) 展开
			}
		}
		if statsErr == nil {
			item["node_stats"] = stats
		}

		if t.as != nil {
			if scopeRows, err := t.as.ListTaskScope(source.Task.TaskID); err == nil && len(scopeRows) > 0 {
				scopeCount := len(scopeRows)
				if len(scopeRows) > relatedOverviewMaxScopePerSource {
					scopeRows = scopeRows[:relatedOverviewMaxScopePerSource]
					budget.truncated = true
				}
				scope := make([]map[string]any, 0, len(scopeRows))
				for _, row := range scopeRows {
					entry := map[string]any{"kind": row.Kind, "source": budget.take(row.Source, 300)}
					switch {
					case row.Domain != "":
						entry["value"] = budget.take(row.Domain, 400)
					case row.Net != "":
						entry["value"] = budget.take(row.Net, 400)
					case row.Value != "":
						entry["value"] = budget.take(row.Value, 400)
					case row.CompanyID != nil:
						entry["company_id"] = *row.CompanyID
					}
					scope = append(scope, entry)
				}
				item["asset_scope"] = scope
				item["asset_scope_count"] = scopeCount
			}
			if coverage, err := t.as.TaskCoverage(source.Task.TaskID, source.Task.ExplorationID); err == nil {
				item["asset_coverage"] = map[string]any{
					"denominator": coverage.Denominator,
					"tested":      coverage.Tested,
					"pct":         coverage.Pct,
					"by_type":     coverage.ByType,
				}
			}
		}
		if budget.truncated {
			item["summary_truncated"] = true
		}
		out = append(out, item)
	}
	return out
}

func inheritedIntentSummaryState(state string) bool {
	switch state {
	case "done", "blocked", "exhausted", "stopped":
		return true
	default:
		return false
	}
}

// compactNode distills any exploration node to id + summary + state, dropping the
// big detail/evidence (fetch that on demand via node_detail).
func compactNode(n *db.Node) map[string]any {
	var p map[string]any
	_ = json.Unmarshal(n.Payload, &p)
	m := map[string]any{"id": n.ID, "state": n.State, "summary": p["summary"]}
	if n.Inherited {
		inheritedMap(m, n.SourceTaskID)
	}
	return m
}

// compactFinding is compactNode plus the vuln-specific vulnclass/severity.
func compactFinding(n *db.Node) map[string]any {
	var p map[string]any
	_ = json.Unmarshal(n.Payload, &p)
	m := map[string]any{"id": n.ID, "state": n.State, "summary": p["summary"]}
	if n.Inherited {
		inheritedMap(m, n.SourceTaskID)
	}
	if vc, ok := p["vulnclass"]; ok && vc != nil && vc != "" {
		m["vulnclass"] = vc
	}
	if sv, ok := p["severity"]; ok && sv != nil && sv != "" {
		m["severity"] = sv
	}
	return m
}

func (t *ToolSet) listFindings() actool.CoreTool {
	return t.readExpTool("list_findings", "このタスクと直接関連するタスクの【確認済みの脆弱性】を一覧表示します(コンパクト：id+task_id+intent_id+vulnclass+severity+要約+状態)。関連タスクの項目には source_task_id/inherited=true が付き、読み取り専用です。ここには脆弱性だけが含まれます。通常の探索事実は list_facts、詳細は node_detail(id) を使います。",
		obj(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			f, _ := t.ts.ListByKindWithSources(db.KindFinding, 500)
			if err := t.ts.PopulateFindingTrafficIDs(f); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			intentOf, _ := t.ts.FindingIntentsWithSources() // finding id -> 产生它的 intent id
			taskID := t.taskID
			if taskID <= 0 {
				taskID, _ = t.ts.TaskID()
			}
			out := make([]map[string]any, 0, len(f))
			for _, n := range f {
				m := compactFinding(n)
				if n.FindingID > 0 {
					m["finding_id"], m["finding_node_id"], m["traffic_count"] = n.FindingID, n.ID, n.TrafficCount
				}
				if n.Inherited {
					m["task_id"] = n.SourceTaskID
				} else {
					m["task_id"] = taskID
				}
				if iid, ok := intentOf[n.ID]; ok {
					m["intent_id"] = iid
				}
				out = append(out, m)
			}
			return jsonResult(out)
		})
}

// factsPageSize is the default page size for list_facts. Facts pile up on long
// tasks; returning all of them at once (the old behaviour) could blow up the
// context, so default to the newest page and let the agent page/filter for more.
const factsPageSize = 20

func (t *ToolSet) listFacts() actool.CoreTool {
	return t.readExpTool("list_facts", "このタスクと直接関連するタスクの【探索事実/結論】を、新しい順にページ分割して一覧表示します(コンパクト：id+要約+状態。要約が長いと切り詰められ、全文は node_detail(id) で取得)。パラメータはすべて任意：limit(デフォルト 20、上限 100)、before(カーソル。前のページが返した next_before を渡すと、より古い 1 ページを取得。省略/0=最新の 1 ページ)、q(要約のキーワードで絞り込み)。戻り値は {facts, total, has_more, next_before}：total は絞り込み後の総数で、has_more=true のときは next_before で次のページへ進みます。関連タスクの項目には source_task_id/inherited=true が付き、読み取り専用です。脆弱性は list_findings を見てください。",
		obj(map[string]any{
			"limit":  intp("返す件数。デフォルト 20、上限 100"),
			"before": intp("ページ分割のカーソル：id がこの値より小さい古い事実だけを返します。省略または 0 = 最新の 1 ページ"),
			"q":      str("事実の要約のキーワードで絞り込みます（大文字小文字を区別しない）。省略 = 絞り込みなし"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Limit  int    `json:"limit"`
				Before int64  `json:"before"`
				Q      string `json:"q"`
			}
			_ = json.Unmarshal(in, &a)
			limit := a.Limit
			if limit <= 0 {
				limit = factsPageSize
			}
			if limit > 100 {
				limit = 100
			}
			f, hasMore, total, err := t.ts.ListByKindPageWithSources(db.KindFact, a.Before, limit, strings.TrimSpace(a.Q))
			if err != nil {
				return actool.Result{}, err
			}
			out := make([]map[string]any, 0, len(f))
			for _, n := range f {
				out = append(out, compactFact(n))
			}
			res := map[string]any{"facts": out, "total": total, "has_more": hasMore}
			if hasMore && len(f) > 0 {
				res["next_before"] = f[len(f)-1].ID // 传回它取下一页(更旧的)
			}
			return jsonResult(res)
		})
}

// factSummaryMax caps a fact summary in list_facts output. Facts carry one-line
// conclusions, but nothing enforces brevity; a runaway summary must not bloat a
// whole page. Full text stays available via node_detail(id).
const factSummaryMax = 160

// compactFact is compactNode with the summary rune-capped for list_facts, so a
// page of facts stays bounded regardless of how long any single summary grew.
func compactFact(n *db.Node) map[string]any {
	m := compactNode(n)
	if s, ok := m["summary"].(string); ok && len([]rune(s)) > factSummaryMax {
		m["summary"] = string([]rune(s)[:factSummaryMax]) + "…"
		m["summary_truncated"] = true
	}
	return m
}

func (t *ToolSet) nodeDetail() actool.CoreTool {
	return t.readExpTool("node_detail", "id で、このタスクまたは直接関連するタスクの【探索グラフのノード】の完全な内容を取得します。継承したノードには source_task_id/inherited=true が付き、読み取り専用です。対象は list_facts/list_findings/graph_overview が返した探索ノードの id のみです。資産には list_assets/asset_neighbors を使ってください。",
		obj(map[string]any{"id": idp("探索グラフのノード id(資産 id ではない)")}, "id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				ID json.RawMessage `json:"id"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.ID)
			if id <= 0 {
				return actool.Errorf("id 必填"), nil
			}
			n, err := t.ts.GetNodeWithSources(id)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if n == nil {
				return actool.Errorf(fmt.Sprintf("未找到探索节点 %d。若你想查的是资产，请用 list_assets / asset_neighbors（资产与探索节点是不同的 id 空间，资产 id 不能传给 node_detail）。", id)), nil
			}
			if err := t.ts.PopulateFindingTrafficIDs([]*db.Node{n}); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return jsonResult(n) // full payload incl. detail / evidence, plus explicit finding IDs
		})
}

// --- planner write tools ---

// intentItem 是 add_intent 批量/单条的一条探索方向。
type intentItem struct {
	Summary   string            `json:"summary"`
	AssetIDs  []json.RawMessage `json:"asset_ids"`
	ParentIDs []json.RawMessage `json:"parent_ids"`
	Priority  int               `json:"priority"`
}

// addOneIntent 创建一条意图节点并连上游血缘，返回 id。
// 约束：意图只能锚在已确认知识上——每个 parent_id 必须是已存在的 fact/finding
// 节点（不能挂在别的意图/目标/提示上）。顶层全新方向留空 parent_ids，兜底连 origin fact。
// 这样"每个意图都连到 fact 节点、且是发现驱动而非凭空规划"从创建路径上被强制。
func (t *ToolSet) addOneIntent(it intentItem) (int64, error) {
	if strings.TrimSpace(it.Summary) == "" {
		return 0, fmt.Errorf("summary 不能为空")
	}
	// 先校验锚点（建节点前，避免坏锚点留下孤儿意图）。
	parents := pidList(it.ParentIDs)
	for _, pidv := range parents {
		n, err := t.ts.GetNodeWithSources(pidv)
		if err != nil || n == nil {
			return 0, fmt.Errorf("parent_id %d 不存在于本任务或直接关联任务：parent_ids 必须是已存在的【事实(fact)/发现(finding)】节点 id；顶层全新方向请留空 parent_ids", pidv)
		}
		if n.Kind != db.KindFact && n.Kind != db.KindFinding {
			return 0, fmt.Errorf("parent_id %d 是 %q 节点，不能作为意图锚点：意图只能锚在已确认的【事实(fact)/发现(finding)】上，不能挂在意图/目标/提示上；顶层全新方向请留空 parent_ids", pidv, n.Kind)
		}
	}
	priority := it.Priority
	if priority == 0 {
		priority = 5
	}
	anchors := pidList(it.AssetIDs)
	// 资产拦截：意图绑定的资产若命中系统资产拦截规则，则禁止下发该意图。
	if t.as != nil && len(anchors) > 0 {
		hits, err := t.as.CheckAssetsIntercept(t.taskID, anchors)
		if err != nil {
			return 0, fmt.Errorf("资产拦截校验失败：%w", err)
		}
		if len(hits) > 0 {
			var b strings.Builder
			fmt.Fprintf(&b, "意图「%s」绑定的资产未通过测试范围校验，请停止对相关资产进行测试：", it.Summary)
			for _, h := range hits {
				fmt.Fprintf(&b, "\n - %s", h.Describe())
			}
			return 0, fmt.Errorf("%s", b.String())
		}
	}
	payload := map[string]any{"summary": it.Summary}
	if len(anchors) > 0 {
		payload["asset_ids"] = anchors
	}
	id, err := t.ts.AddIntent(payload, priority, anchors, "planner")
	if err != nil {
		return 0, err
	}
	// upstream lineage: link each (validated) fact/finding parent → this intent, so
	// "multiple facts combine into one new intent" is expressible.
	for _, parent := range parents {
		_ = t.ts.Link(parent, db.RelDerivedFrom, id)
	}
	// a top-level intent (no explicit parent) connects to the origin fact, so every
	// intent still traces back to a fact node — at task start the only fact is the
	// origin, and the first intents derive from it.
	if len(parents) == 0 {
		if origin, _ := t.ts.OriginFactID(); origin > 0 {
			_ = t.ts.Link(origin, db.RelDerivedFrom, id)
		}
	}
	return id, nil
}

func (t *ToolSet) addIntent() actool.CoreTool {
	return t.writeExpTool("add_intent", "【探索方向】を生成して frontier に書き込み、探索チェーンにつなぎます。インテントは自由な探索方向で、固定の種類はありません。summary に、何を探索/検証/悪用するかを一文で自由に書きます。\n★バッチ優先：1 回の計画で絞り込んだ複数の新しい方向は、intents 配列にまとめて一度に送ってください（1 件ずつ呼ぶより往復が減ります）。戻り値の ids 配列は intents と同じ長さ・同じ順序です（失敗した項目は id=0、詳細は errors）。1 件だけなら intents を省略し、トップレベルの summary を直接渡します。",
		obj(map[string]any{
			"intents":    map[string]any{"type": "array", "description": "【こちらを優先して使う】追加する探索方向の配列。順番に処理されます。各要素のフィールドは、下のトップレベルのフィールド(summary/asset_ids/parent_ids/priority)と同じです。戻り値の ids はこの配列と同じ長さ・同じ順序です。", "items": map[string]any{"type": "object"}},
			"summary":    str("[単一] この探索方向を一文で説明します：何をするか + なぜか。方向が明確に書かれていれば十分で、資産 id には依存しません。"),
			"asset_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "この方向でテスト/攻撃する【対象資産 id】（**できるだけ渡す**。0/1/複数。list_assets が返す資産 id であり、探索ノードの id ではない）：この探索方向がどの資産（サイト/API/パラメータ/ホストなど）を対象にしているかを示します。方向が特定の資産を中心にしているなら、必ず渡してください。これは「この探索がどの対象を狙うか」の構造化された目印で、カバレッジの重複排除と、インテントを資産チェーンにつなぐために使われます。純粋なグローバル偵察で、具体的な対象資産が本当にない場合にだけ空にします。"},
			"parent_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "上流のアンカー id（任意。0/1/複数）：この方向が、どの【確認済みの事実(fact)/検出事項(finding)】を総合して導かれたか。**既存の fact/finding ノードの id だけを指定でき、インテント/目標/ヒントは指定できません**。インテントは確認済みの知識に根ざす必要があり、無根拠な計画ではなく発見に駆動されます。複数の事実から 1 つの新しいインテントが生まれる場合は複数を渡します。トップレベルの完全に新しい偵察方向は空にしてください（タスクの起点 origin fact に自動で接続されます）。"},
			"priority":   intp("優先度 0-10、デフォルト 5"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Intents    []intentItem `json:"intents"`
				intentItem              // 单条模式：顶层 summary/asset_ids/parent_ids/priority
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Intents) > 0
			items := a.Intents
			if !batch {
				items = []intentItem{a.intentItem}
			}

			ids := make([]int64, len(items))
			errs := map[string]string{}
			createdAny := false
			for i, it := range items {
				id, err := t.addOneIntent(it)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
				createdAny = true
			}

			// 人经主 agent 直投意图 → 若任务已 done（无 open 目标的 goalless 分支），把它
			// 拉回 running，worker 才能领这条意图执行。resumeTask 仅由主 agent 的 Chat 接入
			// (SetResumeTask)；planner 的 ToolSet 为 nil，故 planner 自己调 add_intent 时此段
			// no-op，不影响其正常产意图。意图节点已在上面建好(open)，复活时不会被误判抽干。
			if createdAny && t.resumeTask != nil {
				t.resumeTask()
			}

			if !batch { // 单条：保持原返回
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("intent created: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

func (t *ToolSet) listGoals() actool.CoreTool {
	return t.readExpTool("list_goals", "このタスクの目標ノードとその状態（open/met）を一覧表示します。達成したかどうかの判断に使います。",
		obj(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			g, _ := t.ts.ListByKind(db.KindGoal, 100)
			return jsonResult(g)
		})
}

func (t *ToolSet) proveGoal() actool.CoreTool {
	return t.writeExpTool("prove_goal", "ある検出事項/事実が目標の達成を証明していると判断したときに呼びます：証拠ノードを目標ノードにつなぎ、目標を met にします。",
		obj(map[string]any{
			"goal_id":     idp("目標ノードの id"),
			"evidence_id": idp("目標を証明する検出事項/事実ノードの id"),
			"reason":      str("この証拠がなぜ目標を満たすのか"),
		}, "goal_id", "evidence_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				GoalID     json.RawMessage `json:"goal_id"`
				EvidenceID json.RawMessage `json:"evidence_id"`
				Reason     string          `json:"reason"`
			}
			_ = json.Unmarshal(in, &a)
			goal, ev := pid(a.GoalID), pid(a.EvidenceID)
			if goal == 0 || ev == 0 {
				return actool.Errorf("goal_id 和 evidence_id 必填"), nil
			}
			goalNode, err := t.ts.GetNode(goal)
			if err != nil || goalNode == nil || goalNode.Kind != db.KindGoal {
				return actool.Errorf("goal_id 必须是本任务的目标节点（关联任务目标只读）"), nil
			}
			evidenceNode, err := t.ts.GetNodeWithSources(ev)
			if err != nil || evidenceNode == nil || (evidenceNode.Kind != db.KindFact && evidenceNode.Kind != db.KindFinding) {
				return actool.Errorf("evidence_id 必须是本任务或直接关联任务的事实/漏洞节点"), nil
			}
			_ = t.ts.Link(ev, db.RelProves, goal)
			_ = t.ts.SetNodeState(goal, "met")
			// 每标记一个目标 met，就检查本任务是否【所有目标】都已 met；若是，自动判定
			// 任务完成（置 GoalMet），无需再依赖模型显式调 goal_met。
			if goals, err := t.ts.ListByKind(db.KindGoal, 1000); err == nil && len(goals) > 0 {
				allMet := true
				for _, g := range goals {
					if g.State != "met" {
						allMet = false
						break
					}
				}
				if allMet {
					t.GoalMet = true
					t.Reason = fmt.Sprintf("所有 %d 个目标均已 met（最后由 goal %d 触发）", len(goals), goal)
					return actool.Text(fmt.Sprintf("goal %d marked met；本任务所有目标均已达成，任务自动判定完成", goal)), nil
				}
			}
			return actool.Text(fmt.Sprintf("goal %d marked met", goal)), nil
		})
}

func (t *ToolSet) goalMet() actool.CoreTool {
	return writeTool("goal_met", "【タスク全体をただちに終了する】——タスクの【すべての目標が本当に達成され、全体が完了した】と確認できた場合にだけ呼びます（タスクの【全体】の完了が条件です。目標の 1 つ、flag の 1 つ、脆弱性の 1 つを達成しただけでは【該当しません】。その場合は prove_goal でその目標を記録してください）。⚠️これは「今回の計画を終わらせる」ためのものではありません。今回は新しく発行するインテントがない、または worker の成果を待っているという場合は、【そのまま今回を終了すれば十分で、このツールは呼ばないでください】（インテントが 0 個なのは完全に正常です）。通常の判定では prove_goal で目標を 1 つずつ証明するのを優先します。goal_met は、1 つずつの証明を飛ばして全体から完了させる手段にすぎません。",
		obj(map[string]any{"reason": str("達成の理由（目標が本当に達成された証拠でなければならず、「今回は新しい方向がない」のような今回を終える理由は不可）")}, "reason"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct{ Reason string }
			_ = json.Unmarshal(in, &a)
			t.GoalMet = true
			t.Reason = a.Reason
			return actool.Text("acknowledged: goal marked met"), nil
		})
}

// --- worker write tools ---

func (t *ToolSet) addFinding() actool.CoreTool {
	return writeTool("report_finding", "確認した脆弱性を記録します。evidence には、コマンド出力やログなど、検証できる証拠を渡します。タスクのコンテキストでは現在の intent_id を渡します。返される finding_id は独立した脆弱性記録の ID、finding_node_id は探索ノードの ID です（最初の行にそのノード番号を残します）。", obj(map[string]any{
		"vulnclass": str("脆弱性のカテゴリ"), "name": str("脆弱性の名前"), "severity": str("critical|high|medium|low"), "summary": str("検出事項の要約"),
		"intent_id": idp("現在のタスクのインテント id"), "asset_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "影響を受ける資産 id"},
		"evidence":         str("証拠/PoC のテキスト"),
		"evidence_hint_id": idp("任意：このタスクで、この脆弱性に対応するヒントノードの ID。その構造化された traffic_refs を自動で引き継ぎます。継承したヒントや他の脆弱性のヒントは参照できません"),
		"traffic_refs": map[string]any{"type": "array", "description": "任意。HTTP/HTTPS の脆弱性は、先に検索してリクエスト/レスポンスが脆弱性の結論を本当に裏付けているかを 1 件ずつ確認し、再現の順序で実際の ID を書きます。TCP など HTTP 以外の脆弱性、未採取のとき、または該当する記録が見つからないときは、省略するか [] を渡します。報告は妨げられません。evidence に理由を書き、他の検証できる証拠を提供してください。ID を推測したり、ドメインや時刻から関連を推定したり、パケットを補うためだけに再探索したりしないでください。用途：baseline=正常時の比較対象 / proof=脆弱性の証明 / verification=補足検証 / supporting=補助証拠。",
			"items": obj(map[string]any{"traffic_id": str("traffic_search が返した実際のトラフィック ID"), "role": map[string]any{"type": "string", "enum": []string{"baseline", "proof", "verification", "supporting"}}, "note": str("このトラフィックが脆弱性の結論をどう裏付けるか")}, "traffic_id")},
	}, "vulnclass", "severity", "summary"), func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
		var a struct {
			VulnClass, Name, Severity, Summary, Evidence string
			IntentID                                     json.RawMessage   `json:"intent_id"`
			AssetIDs                                     []json.RawMessage `json:"asset_ids"`
			TrafficRefs                                  []db.TrafficRef   `json:"traffic_refs"`
			EvidenceHintID                               json.RawMessage   `json:"evidence_hint_id"`
		}
		if err := json.Unmarshal(in, &a); err != nil {
			return actool.Errorf(err.Error()), nil
		}
		if t.ts == nil {
			return actool.Errorf("report_finding 需要任务上下文；平台对话请通过 add_task_hint 向对应任务交接漏洞，并在提示中携带已有的 traffic_refs，由任务 Agent 登记。已登记漏洞可用 bind_finding_traffic 补绑。"), nil
		}
		// Auto-binding off: ignore the evidence params instead of rejecting the call.
		// stripTrafficParameters already removes them from the advertised schema, but
		// models routinely emit fields anyway — failing here would discard a confirmed
		// finding over a stray parameter. The success path below reports evidence_status
		// "not_bound" with the "已关闭，可在页面人工关联" note, which is what the caller needs.
		if !findingTrafficBindingEnabled() {
			a.TrafficRefs, a.EvidenceHintID = nil, nil
		}
		if len(a.EvidenceHintID) > 0 && pid(a.EvidenceHintID) <= 0 {
			return actool.Errorf("evidence_hint_id 必须为有效的提示节点 ID；无交接提示时省略"), nil
		}
		refs, err := t.findingRefsFromHint(pid(a.EvidenceHintID), a.TrafficRefs)
		if err != nil {
			return actool.Errorf(err.Error()), nil
		}
		input := db.RecordFindingInput{TaskID: t.taskID, ExplorationID: t.ts.ID(), IntentID: pid(a.IntentID), VulnClass: a.VulnClass, Name: a.Name, Severity: a.Severity, Summary: a.Summary, Evidence: a.Evidence, Worker: t.worker, AssetIDs: pidList(a.AssetIDs)}
		var recorded *db.RecordedFinding
		if t.findingRecorder != nil {
			recorded, err = t.findingRecorder.Record(ctx, input, refs)
		} else if len(refs) > 0 {
			return actool.Errorf("流量证据存储不可用；未登记漏洞"), nil
		} else {
			recorded, err = t.ts.RecordFinding(ctx, input)
		}
		if err != nil {
			return actool.Errorf(err.Error()), nil
		}
		if t.notifyFinding != nil {
			iid := input.IntentID
			if iid <= 0 {
				iid = t.ownerNode
			}
			t.notifyFinding(iid, a.Summary)
		} else if t.notify != nil {
			t.notify()
		}
		t.writes.Findings++
		// Keep the first line's node-ID contract for existing reporter triggers.
		for i := range recorded.Traffic.Bindings {
			recorded.Traffic.Bindings[i].Snapshot.ReqHead = ""
			recorded.Traffic.Bindings[i].Snapshot.RespHead = ""
		}
		result := struct {
			*db.RecordedFinding
			EvidenceStatus string `json:"evidence_status"`
			EvidenceNote   string `json:"evidence_note,omitempty"`
		}{RecordedFinding: recorded, EvidenceStatus: "bound"}
		if len(recorded.Traffic.Bindings) == 0 {
			result.EvidenceStatus = "not_bound"
			result.EvidenceNote = "漏洞已保存，未绑定流量。TCP/无包情形可正常继续；若已有核实的 HTTP 流量，请用可用的 bind_finding_traffic 或漏洞页面补绑，再完成证据交接。不要重复创建漏洞。"
			if !findingTrafficBindingEnabled() {
				result.EvidenceNote = "漏洞已保存。Agent 自动绑定流量已关闭，可在页面人工关联流量。"
			}
		}
		raw, _ := json.Marshal(result)
		return actool.Text(fmt.Sprintf("finding recorded: %d\n%s", recorded.NodeID, raw)), nil
	})
}

// recordFact writes a general exploration RESULT/conclusion (not a vuln, not a
// new asset) into the EXPLORATION graph, chained to the intent that produced it.
// This is the home for observations and — importantly — negative results
// ("port closed", "param not injectable", "no login found"). Such conclusions
// must NOT be stuffed into the asset graph via upsert_asset.
// factItem 是 record_fact 批量/单条的一条事实。
type factItem struct {
	Summary    string            `json:"summary"`
	Detail     string            `json:"detail"`
	Evidence   string            `json:"evidence"`   // 一行关键证据（命令+关键输出行），支撑结论、便于事后核对
	Confidence string            `json:"confidence"` // observed（直接看到）| inferred（据现象推断）
	IntentID   json.RawMessage   `json:"intent_id"`
	AssetIDs   []json.RawMessage `json:"asset_ids"`
}

// recordOneFact 写一条 fact 节点并连到意图（intent→yields→fact）。defaultIntent 为
// 批量时的默认意图（本条未给 intent_id 时用）。
func (t *ToolSet) recordOneFact(it factItem, defaultIntent int64) (int64, error) {
	if strings.TrimSpace(it.Summary) == "" {
		return 0, fmt.Errorf("summary 不能为空")
	}
	payload := map[string]any{"summary": it.Summary}
	if it.Detail != "" {
		payload["detail"] = it.Detail
	}
	if e := strings.TrimSpace(it.Evidence); e != "" {
		payload["evidence"] = e
	}
	if c := strings.TrimSpace(it.Confidence); c != "" {
		payload["confidence"] = c
	}
	intent := pid(it.IntentID)
	if intent <= 0 {
		intent = defaultIntent
	}
	if intent > 0 {
		node, err := t.ts.GetNode(intent)
		if err != nil || node == nil || node.Kind != db.KindIntent {
			return 0, fmt.Errorf("intent_id 必须是本任务的意图（关联任务意图只读）")
		}
	}
	// a fact is its OWN node kind (distinct from a vuln finding).
	id, err := t.ts.AddNode(db.KindFact, payload, 5, "confirmed", t.worker, pidList(it.AssetIDs))
	if err != nil {
		return 0, err
	}
	if intent > 0 {
		_ = t.ts.Link(intent, db.RelYields, id) // chain: intent -> fact
	}
	t.writes.Facts++
	return id, nil
}

func (t *ToolSet) recordFact() actool.CoreTool {
	return t.writeExpTool("record_fact", "探索の【事実/結論】を探索グラフに書き込み、それを生んだインテント（intent_id）につなぎます。探索結果の記録に使います。フィンガープリントや列挙などの【肯定的な結論】と、「ポートが閉じている」「パラメータにインジェクションできない」「ログインの入口が見つからない」などの【否定的な結論】を含みます。\n⚠️1 回の探索で得た複数の観察は【1 件の事実にまとめて】ください。複数に分けないでください。1 件にまとめられるものはできるだけ 1 件の事実で表します：summary=今回の結論の要約を一文で、detail=関連する詳細（複数の具体的な項目を含められる）。例：フィンガープリントのインテント→1 件の事実 {summary:'X サイトの技術スタックとレスポンスの特徴を識別した', detail:'nginx 1.25 / Vue3 / 200 / title=.. / body_len=..'}。ステータスコード、フィンガープリント、タイトルを別々に記録してはいけません。1 つのインテントが生む事実は通常 1 件で、細かく分けすぎるとグラフが際限なく膨らみます。\n★facts 配列は、【互いに異なる】結論を一度に複数書くためのものです（各項目の intent_id は省略でき、デフォルトはトップレベルの intent_id）。戻り値の ids 配列は facts と同じ長さ・同じ順序です。\n⚠️ツールの出力で【実際に見た】結論だけを書き、推測で補わないでください。evidence と confidence は、不正確な結論でグラフが汚れるのを防ぎます：\n  · evidence=この結論を裏付ける【1 行】の重要な証拠（コマンド + 最もよく証明する 1〜2 行の出力）。**必ず簡潔に**。詳細は detail にあるので、ここに長い出力を貼らないでください。\n  · confidence=observed（出力で直接見た）| inferred（現象から推測）。\n  · **否定的な結論**（インジェクション不可/ポートが閉じている/入口が見つからないなど）は、「観察 + 暫定的な読み」だけを書きます。実際に見たものを述べ、方向を諦めるかどうかは、プランナーが全体を見て決めます。必ず evidence を付け、手段を尽くしていない、または証拠が弱い場合（1 回しか調べていない、らしく見える、を含む）は inferred、尽くしたうえで直接見たときだけ observed にします。",
		obj(map[string]any{
			"facts":      map[string]any{"type": "array", "description": "【異なる結論が複数あるときに使う】事実の配列。各要素のフィールドは、下のトップレベルのフィールド(summary/detail/evidence/confidence/intent_id/asset_ids)と同じです。intent_id を省略すると、トップレベルの intent_id を使います。戻り値の ids はこの配列と同じ長さ・同じ順序です。", "items": map[string]any{"type": "object"}},
			"summary":    str("今回の探索結論の【一文での要約】（detail を要約したもの）"),
			"intent_id":  idp("この事実を生んだインテントの id（受け取ったインテント。バッチのときは各項目のデフォルト）"),
			"detail":     str("この事実の関連する詳細：今回の探索の複数の観察事実をすべてここに書く"),
			"evidence":   str("【1 行】の重要な証拠：コマンド + 結論を最もよく証明する 1〜2 行の出力。必ず簡潔にし、長い出力を貼らない（詳細は detail に書く）。"),
			"confidence": str("observed（出力で直接見た）| inferred（現象から推測）。否定的な結論は必ず正直に記載してください。"),
			"asset_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "関連する資産 id（任意。0/1/複数）：この事実がどの資産に関係するか"},
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Facts    []factItem `json:"facts"`
				factItem            // 单条模式 + 批量默认 intent_id
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Facts) > 0
			items := a.Facts
			if !batch {
				items = []factItem{a.factItem}
			}
			defaultIntent := pid(a.factItem.IntentID) // 顶层 intent_id = 批量默认

			ids := make([]int64, len(items))
			errs := map[string]string{}
			for i, it := range items {
				id, err := t.recordOneFact(it, defaultIntent)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
			}

			if !batch { // 单条：保持原返回
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("fact recorded: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

type hintItem struct {
	Text        string            `json:"text"`
	AssetIDs    []json.RawMessage `json:"asset_ids"`
	TrafficRefs []db.TrafficRef   `json:"traffic_refs"`
}

// addOneHint 挂一条 hint 节点(active/human)到探索图,可锚定资产,返回 id。
func (t *ToolSet) addOneHint(it hintItem) (int64, error) {
	if len(it.TrafficRefs) > 0 && !findingTrafficBindingEnabled() {
		return 0, fmt.Errorf("Agent 自动绑定流量已关闭，未保存携带 traffic_refs 的提示；可在系统设置开启，或仅交接文字")
	}
	if strings.TrimSpace(it.Text) == "" {
		return 0, fmt.Errorf("text 不能为空")
	}
	var anchors []int64
	for _, raw := range it.AssetIDs {
		if tid := pid(raw); tid > 0 {
			anchors = append(anchors, tid)
		}
	}
	refs, err := db.NormalizeTrafficRefs(it.TrafficRefs)
	if err != nil {
		return 0, err
	}
	payload := map[string]any{"text": it.Text}
	if len(refs) > 0 {
		payload["traffic_refs"] = refs
	}
	// 唤醒 planner 不在此处逐条做——由 addHint 在整批写完后统一触发一次（带上提示文本），
	// 避免一次 add_hint 多条提示逐条刷屏 planner 的触发行。
	return t.ts.AddNode(db.KindHint, payload, 0, "active", "human", anchors)
}

type goalItem struct {
	Text      string `json:"text"`
	VulnClass string `json:"vulnclass"`
}

// addOneGoal 挂一条 goal 节点(open)到探索图:连到任务根(origin fact,rel spawns)。
// origin 取 t.worker(缺省 system):goals 拆解器写入的记 "goals"、主 agent 运行时记
// "human"。唤醒 planner 由 setGoals 在整批写完后统一做(见下),这里只负责落库。
func (t *ToolSet) addOneGoal(it goalItem) (int64, error) {
	text := strings.TrimSpace(it.Text)
	if text == "" {
		return 0, fmt.Errorf("text 不能为空")
	}
	payload := map[string]any{"text": text}
	if vc := strings.TrimSpace(it.VulnClass); vc != "" {
		payload["vulnclass"] = vc
	}
	origin := t.worker
	if origin == "" {
		origin = "system"
	}
	id, err := t.ts.AddNode(db.KindGoal, payload, 0, "open", origin, nil)
	if err != nil {
		return 0, err
	}
	if of, _ := t.ts.OriginFactID(); of > 0 && id > 0 {
		_ = t.ts.Link(of, db.RelSpawns, id) // goals descend from the task root (origin fact)
	}
	return id, nil
}

// setGoals 给【本任务】新增探索目标(goal 节点)。既是目标拆解器的提交工具,也是主
// agent 运行时补目标的工具——同一个受管工具,可在 web 端改描述/schema、按 agent 绑定。
func (t *ToolSet) setGoals() actool.CoreTool {
	return writeTool("set_goals",
		"【このタスク】に探索目標(goal)を追加します。目標=最終的に提出でき、検証できる成果であり、攻撃の手順や偵察の動作ではありません。\n★バッチ優先:複数の目標は goals 配列にまとめて一度に送ってください。戻り値の ids は配列と同じ長さ・同じ順序です(失敗した項目は id=0、詳細は errors)。1 件だけなら goals を省略し、トップレベルの text を直接渡します。\nvulnclass は任意:対応する脆弱性クラス(例 SQLi/IDOR)。ビジネスロジック系の目標は空にします。目標が達成されたかどうかはシステムが判定して met を付けます。このツールは追加だけを担当します。",
		obj(map[string]any{
			"goals":     map[string]any{"type": "array", "description": "【こちらを優先して使う】追加する目標の配列。順番に処理されます。各要素:text(必須。独立して検証できる最終目標)+ vulnclass(任意)。戻り値の ids はこの配列と同じ長さ・同じ順序です。", "items": map[string]any{"type": "object"}},
			"text":      str("[単一] 独立して検証できる最終目標"),
			"vulnclass": str("[単一] 対応する脆弱性クラス(明確な場合)。例 SQLi/IDOR。ビジネスロジックの目標は空にできます"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.ts == nil {
				return actool.Errorf("set_goals 未启用: ExplorationStore 未初始化"), nil
			}
			var a struct {
				Goals    []goalItem `json:"goals"`
				goalItem            // 单条模式:顶层 text/vulnclass
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Goals) > 0
			items := a.Goals
			if !batch {
				items = []goalItem{a.goalItem}
			}

			ids := make([]int64, len(items))
			errs := map[string]string{}
			var addedTexts []string
			for i, it := range items {
				id, err := t.addOneGoal(it)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
				addedTexts = append(addedTexts, strings.TrimSpace(it.Text))
			}
			if len(addedTexts) > 0 {
				// 唤醒 planner(整批一次)。优先 notifyGoal:一次 set_goals 记一条「人新增了
				// N 个目标:…」触发,不逐条刷屏;拆解器/worker 无此回调 → 退回纯 notify(拆解器
				// round-0 连 notify 也没接,即无操作,因为此时 planner 尚未启动)。
				switch {
				case t.notifyGoal != nil:
					t.notifyGoal(addedTexts)
				case t.notify != nil:
					t.notify()
				}
				// 主 agent 运行时新增目标 → 把已完成/暂停的任务拉回 running 继续跑(终态门会
				// 吞掉普通 notify,必须显式复活)。仅 mainagent 接了此回调;拆解器/worker 为 nil。
				if t.resumeTask != nil {
					t.resumeTask()
				}
			}

			if !batch { // 单条:保持原返回
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("goal added: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

type constraintItem struct {
	Text string `json:"text"`
	Type string `json:"type"` // allow | deny
}

// addOneConstraint 落一条操作约束到 task_constraints。origin 取 t.worker(缺省 system):
// 拆解器写 "goals"、主 agent 写 "human"。
func (t *ToolSet) addOneConstraint(it constraintItem) (int64, error) {
	text := strings.TrimSpace(it.Text)
	if text == "" {
		return 0, fmt.Errorf("text 不能为空")
	}
	kind := strings.TrimSpace(strings.ToLower(it.Type))
	if kind == "" {
		kind = "deny" // 默认按禁止处理:未标注类型时更保守
	}
	if kind != "allow" && kind != "deny" {
		return 0, fmt.Errorf("type 必须是 allow 或 deny")
	}
	return t.ts.AddConstraint(kind, text, t.worker)
}

// setConstraints 给【本任务】新增操作约束(allow=允许做什么 / deny=禁止做什么)。既是目标
// 拆解器 round-0 抽约束的提交工具,也是主 agent 运行时补约束的工具——同一受管工具,可在 web
// 端改描述/schema、按 agent 绑定。约束会被注入 planner/worker 的系统提示以约束探索边界。
func (t *ToolSet) setConstraints() actool.CoreTool {
	return writeTool("set_constraints",
		"【このタスク】に操作の制約を追加し、探索の範囲を定めます:type=allow(許可する操作)または deny(禁止する操作)。\n制約=『どの操作をしてよいか/いけないか』の取り決めです(例『現在のポートだけをテストし、他のポートはスキャンしない』『本番の DB への書き込み操作は禁止』『パッシブ偵察だけを許可』)。目標でも攻撃の手順でもありません。\n★バッチ優先:複数の制約は constraints 配列にまとめて一度に送ってください。戻り値の ids は配列と同じ長さ・同じ順序です(失敗した項目は id=0、詳細は errors)。1 件だけなら constraints を省略し、トップレベルの text/type を直接渡します。\nタスクの目標/説明に【明記されている】制約だけを登録し、でっち上げないでください。種類に迷うときは deny(より保守的)を使います。",
		obj(map[string]any{
			"constraints": map[string]any{"type": "array", "description": "【こちらを優先して使う】追加する制約の配列。順番に処理されます。各要素:text(必須。制約 1 件)+ type(allow|deny)。戻り値の ids はこの配列と同じ長さ・同じ順序です。", "items": map[string]any{"type": "object"}},
			"text":        str("[単一] 操作の制約 1 件の内容"),
			"type":        str("[単一] allow(許可)または deny(禁止)。省略時は deny として扱います"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.ts == nil {
				return actool.Errorf("set_constraints 未启用: ExplorationStore 未初始化"), nil
			}
			var a struct {
				Constraints    []constraintItem `json:"constraints"`
				constraintItem                  // 单条模式:顶层 text/type
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Constraints) > 0
			items := a.Constraints
			if !batch {
				items = []constraintItem{a.constraintItem}
			}
			ids := make([]int64, len(items))
			errs := map[string]string{}
			for i, it := range items {
				id, err := t.addOneConstraint(it)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
			}
			if !batch { // 单条:保持简单返回
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("constraint added: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

func (t *ToolSet) addHint() actool.CoreTool {
	return t.writeExpTool("add_hint", "人間/メイン agent の戦略的なヒントを探索グラフに付けます。プランナーが次にインテントを生成するときに読みます。\n★バッチ優先：複数のヒントは hints 配列にまとめて一度に送ってください（1 件ずつ呼ぶより往復が減ります）。戻り値の ids 配列は hints と同じ長さ・同じ順序です（失敗した項目は id=0、詳細は errors）。1 件だけなら hints を省略し、トップレベルの text を直接渡します。",
		obj(map[string]any{
			"hints":        map[string]any{"type": "array", "description": "【こちらを優先して使う】追加するヒントの配列。順番に処理されます。各要素のフィールドは、下のトップレベルのフィールド(text/asset_ids/traffic_refs)と同じです。戻り値の ids はこの配列と同じ長さ・同じ順序です。", "items": obj(map[string]any{"text": str("ヒントの内容"), "asset_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}}, "traffic_refs": HintTrafficSchema()})},
			"text":         str("[単一] ヒントの内容。例『認証後の API を重点的に掘る』"),
			"traffic_refs": HintTrafficSchema(),
			"asset_ids":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "紐付ける資産 id（任意。0/1/複数）"},
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Hints    []hintItem `json:"hints"`
				hintItem            // 单条模式：顶层 text/asset_ids
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Hints) > 0
			items := a.Hints
			if !batch {
				items = []hintItem{a.hintItem}
			}

			ids := make([]int64, len(items))
			errs := map[string]string{}
			var addedTexts []string
			for i, it := range items {
				id, err := t.addOneHint(it)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
				addedTexts = append(addedTexts, strings.TrimSpace(it.Text))
			}
			if len(addedTexts) > 0 {
				// 唤醒 planner（整批一次）。优先 notifyHint：一次 add_hint 记一条「人新增了
				// N 条战略提示：…」触发，让 planner 明确"本轮由新增 hint 触发"并看到提示内容；
				// 未接该回调时退回纯 notify（bare wake，hint 仍折在图里供其自行读取）。
				switch {
				case t.notifyHint != nil:
					t.notifyHint(addedTexts)
				case t.notify != nil:
					t.notify()
				}
			}

			if !batch { // 单条：保持原返回
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("hint added: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

// killWorkTool lets the planner terminate a single running work (by intent id).
func (t *ToolSet) killWorkTool() actool.CoreTool {
	return t.writeExpTool("kill_work", "実行中のインテント(work)を終了します。方向がずれた、または意味のない探索を止めるために使います。終了したインテントは stopped になり、自動では再取得されません。先に get_worker_output で何をしているかを確認してから判断してください。",
		obj(map[string]any{"intent_id": idp("終了するインテントの id（= work ハンドル）")}, "intent_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.killWork == nil {
				return actool.Errorf("kill_work 当前不可用"), nil
			}
			var a struct {
				IntentID json.RawMessage `json:"intent_id"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.IntentID)
			if id <= 0 {
				return actool.Errorf("intent_id 必填"), nil
			}
			node, err := t.ts.GetNode(id)
			if err != nil || node == nil || node.Kind != db.KindIntent {
				return actool.Errorf("intent_id 必须是本任务的意图（关联任务意图只读）"), nil
			}
			if err := t.killWork(id); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text(fmt.Sprintf("已向意图 %d 的 work 发送终止信号", id)), nil
		})
}

// steerWorkTool lets the planner inject a mid-run course-correction into a running
// work WITHOUT killing it: the message reaches the worker before its next tool call,
// which re-plans its next step (already-gathered context is kept). For in-intent
// nudges ("停做 X、聚焦 Y"); if the whole direction is wrong use kill_work + a new intent.
func (t *ToolSet) steerWorkTool() actool.CoreTool {
	return t.writeExpTool("steer_work", "実行中のインテント(work)に、軌道修正の指示をリアルタイムで注入します。中断せず、それまでの進捗も失いません。worker は次の動作の前にあなたの指示を受け取り、それに従って調整します。『X はもうやめて、Y に集中』のような【インテント内】の軌道修正に使います。方向が根本から間違っているなら、kill_work を使って新しいインテントを作り直してください。先に get_worker_output で何をしているかを確認するのを推奨します。",
		obj(map[string]any{
			"intent_id": idp("軌道修正するインテントの id（= work ハンドル）"),
			"message":   str("worker への軌道修正の指示。何をやめて何に切り替えるかを明確に書きます"),
		}, "intent_id", "message"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.steerWork == nil {
				return actool.Errorf("steer_work 当前不可用"), nil
			}
			var a struct {
				IntentID json.RawMessage `json:"intent_id"`
				Message  string          `json:"message"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.IntentID)
			if id <= 0 {
				return actool.Errorf("intent_id 必填"), nil
			}
			node, err := t.ts.GetNode(id)
			if err != nil || node == nil || node.Kind != db.KindIntent {
				return actool.Errorf("intent_id 必须是本任务的意图（关联任务意图只读）"), nil
			}
			if strings.TrimSpace(a.Message) == "" {
				return actool.Errorf("message 必填"), nil
			}
			if err := t.steerWork(id, a.Message); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text(fmt.Sprintf("已向意图 %d 的 work 注入纠偏指令（下一步生效）", id)), nil
		})
}

// getWorkerOutput returns a work's final (or截至中止时的) conclusion text by intent id.
func (t *ToolSet) getWorkerOutput() actool.CoreTool {
	return t.readExpTool("get_worker_output", "このタスクまたは直接関連するタスクのインテント(work)の、最終的な出力の結論を取得します。関連タスクの結果には source_task_id/inherited=true が付き、読み取り専用です。正常に終了した場合はその要約を返し、終了させられた(stopped)/異常終了した work は、中断時点の最後の出力を返します。",
		obj(map[string]any{"intent_id": idp("インテント id（= work ハンドル）")}, "intent_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				IntentID json.RawMessage `json:"intent_id"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.IntentID)
			if id <= 0 {
				return actool.Errorf("intent_id 必填"), nil
			}
			intentNode, err := t.ts.GetNodeWithSources(id)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if intentNode == nil || intentNode.Kind != db.KindIntent {
				return actool.Errorf("intent_id 不属于本任务或其直接关联任务"), nil
			}
			acts, _, err := t.ts.ActivityListWithSources(id, 0, 1000)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			var chosen, fallback *db.Activity
			for i := range acts {
				switch acts[i].Kind {
				case "result":
					chosen = &acts[i]
					fallback = &acts[i]
				case "text":
					fallback = &acts[i]
				}
			}
			pick := chosen
			if pick == nil {
				pick = fallback
			}
			if pick == nil {
				if intentNode.Inherited {
					return jsonResult(inheritedMap(map[string]any{
						"intent_id": id, "final_text": "（该 work 尚无任何输出）",
					}, intentNode.SourceTaskID))
				}
				return actool.Text("（该 work 尚无任何输出）"), nil
			}
			detail, _ := t.ts.ActivityDetailWithSources(pick.ID)
			if detail == "" {
				detail = pick.Summary
			}
			result := map[string]any{
				"intent_id": id, "final_text": detail,
				"summary": pick.Summary, "is_error": pick.IsError,
			}
			if intentNode.Inherited {
				inheritedMap(result, intentNode.SourceTaskID)
			}
			return jsonResult(result)
		})
}

// traceSteps renders summary-only trace rows, re-truncating each summary to 100
// chars — the stored summary is capped at 200 for the UI transcript; the trace
// tools want it tighter since a whole work's step list is many rows.
func traceSteps(acts []db.Activity) []map[string]any {
	steps := make([]map[string]any, 0, len(acts))
	for i := range acts {
		step := map[string]any{
			"step_id": acts[i].ID, "kind": acts[i].Kind, "tool": acts[i].Tool,
			"is_error": acts[i].IsError, "summary": firstLine(acts[i].Summary, 100),
		}
		if acts[i].Inherited {
			inheritedMap(step, acts[i].SourceTaskID)
		}
		steps = append(steps, step)
	}
	return steps
}

// getWorkerTrace exposes a work's execution PROCESS (not just its final output):
// list step summaries, keyword-search within one work, or pull full detail of a
// few specific steps. Thinking steps are excluded everywhere.
func (t *ToolSet) getWorkerTrace() actool.CoreTool {
	return t.readExpTool("get_worker_trace",
		"インテント(work)の【実行過程】を確認します（最終的な結論だけを返す get_worker_output とは異なります）。使い方は 3 通りです：\n① intent_id だけを渡す → その work の各ステップの要約の流れ(summary≤100 文字、step_id を含む。動作の概要だけで、完全な出力は含まない)を返します。\n② intent_id + q → キーワードに一致したステップの要約だけを返します(要約と完全な出力の両方を検索。返すのは summary のみで、内容を見るには③)。\n③ intent_id + step_ids → それらのステップの完全な内容(detail)を返します。一度に最大 5 件で、超えた分は先頭 5 件だけを返し、notice/omitted_step_ids で取得しなかった分を知らせます。\n典型的な流れ：まず①/②で怪しいステップの step_id を特定し、③でその完全な出力を取得します。思考(thinking)のステップは含みません。直接関連するタスクの過去の trace にも対応し、その結果には source_task_id/inherited=true が付き、読み取り専用です。",
		obj(map[string]any{
			"intent_id": idp("インテント id（= work ハンドル）"),
			"q":         str("キーワード：要約/完全な出力が一致したステップだけを返します（任意。step_ids とは同時に指定できません）"),
			"step_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "完全な内容を取得する step_id（①/②の戻り値から。一度に最大 5 件で、それ以上渡すと先頭 5 件だけを返し、残りは omitted_step_ids に列挙されます）"},
			"limit":     intp("要約の流れ/検索の返却件数の上限（任意）"),
		}, "intent_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				IntentID json.RawMessage   `json:"intent_id"`
				Q        string            `json:"q"`
				StepIDs  []json.RawMessage `json:"step_ids"`
				Limit    int               `json:"limit"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.IntentID)
			if id <= 0 {
				return actool.Errorf("intent_id 必填"), nil
			}
			intentNode, nodeErr := t.ts.GetNodeWithSources(id)
			if nodeErr != nil {
				return actool.Errorf(nodeErr.Error()), nil
			}
			if intentNode == nil || intentNode.Kind != db.KindIntent {
				return actool.Errorf("intent_id 不属于本任务或其直接关联任务"), nil
			}
			// ③ detail drill-down by step ids, thinking excluded by the store.
			if len(a.StepIDs) > 0 {
				// Dedup + drop invalid ids first so garbage/duplicates don't eat into
				// the per-call cap. detail is returned in full (untruncated), so the
				// cap bounds one tool result; over the cap we serve the first N and
				// tell the model exactly which ids were deferred, instead of erroring
				// and forcing it to re-plan the call.
				const maxStepIDs = 5
				var ids []int64
				seen := make(map[int64]bool)
				for _, raw := range a.StepIDs {
					if v := pid(raw); v > 0 && !seen[v] {
						seen[v] = true
						ids = append(ids, v)
					}
				}
				var omitted []int64
				if len(ids) > maxStepIDs {
					omitted = append(omitted, ids[maxStepIDs:]...)
					ids = ids[:maxStepIDs]
				}
				acts, err := t.ts.ActivityByIDsWithSources(ids)
				if err != nil {
					return actool.Errorf(err.Error()), nil
				}
				steps := make([]map[string]any, 0, len(acts))
				for i := range acts {
					if acts[i].NodeID == nil || *acts[i].NodeID != id || acts[i].Inherited != intentNode.Inherited ||
						(acts[i].Inherited && acts[i].SourceTaskID != intentNode.SourceTaskID) {
						continue
					}
					step := map[string]any{
						"step_id": acts[i].ID, "kind": acts[i].Kind, "tool": acts[i].Tool,
						"is_error": acts[i].IsError, "detail": acts[i].Detail,
					}
					if acts[i].Inherited {
						inheritedMap(step, acts[i].SourceTaskID)
					}
					steps = append(steps, step)
				}
				result := map[string]any{"intent_id": id, "steps": steps, "returned_step_ids": ids}
				if len(omitted) > 0 {
					// returned_step_ids/omitted_step_ids let the model decide programmatically
					// whether another call is worth it; the notice states the same in prose.
					result["omitted_step_ids"] = omitted
					result["notice"] = fmt.Sprintf(
						"每次最多取 %d 个步骤的完整内容，本次已返回前 %d 个（%v），未取的 %d 个为 %v。"+
							"若这些内容已足够定位，则无需再取剩余步骤；确需继续时，用这些 step_id 再调一次。",
						maxStepIDs, len(ids), ids, len(omitted), omitted)
				}
				if intentNode.Inherited {
					inheritedMap(result, intentNode.SourceTaskID)
				}
				return jsonResult(result)
			}
			// ①/② summary stream, optionally keyword-filtered; 100-char summaries.
			var acts []db.Activity
			var err error
			if strings.TrimSpace(a.Q) != "" {
				acts, err = t.ts.ActivityTraceSearchWithSources(id, a.Q, a.Limit)
			} else {
				acts, err = t.ts.ActivityTraceWithSources(id, a.Limit)
			}
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			result := map[string]any{"intent_id": id, "steps": traceSteps(acts)}
			if intentNode.Inherited {
				inheritedMap(result, intentNode.SourceTaskID)
			}
			return jsonResult(result)
		})
}

// searchAllWorkerTraces keyword-searches EVERY work's process in this task — for
// finding what a worker saw but never wrote back as a fact. Returns only matching
// summaries (≤100 chars), each tagged with its intent_id for follow-up drill-down.
func (t *ToolSet) searchAllWorkerTraces() actool.CoreTool {
	return t.readExpTool("search_all_worker_traces",
		"【システムがすでにほとんどの情報を提供しているため、通常は使用を推奨しません】【このタスクの他の work の実行過程】をキーワード(q)で検索します。ある worker が見たのに fact に書かれなかったもの（パス/token/エラーなど）を取り戻すために使います。あなた自身のインテントのステップは自動で除外されます（それらはもともとあなたのコンテキストにあります）。一致したステップの要約(summary≤100 文字)だけを返し、各項目に intent_id が付きます。それをもとに get_worker_trace(intent_id, step_ids=[...]) で完全な内容を取得します。",
		obj(map[string]any{
			"q":     str("キーワード（すべての work のステップの要約 + 完全な出力から検索）"),
			"limit": intp("返却件数の上限。デフォルト 100（任意）"),
		}, "q"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Q     string `json:"q"`
				Limit int    `json:"limit"`
			}
			_ = json.Unmarshal(in, &a)
			if strings.TrimSpace(a.Q) == "" {
				return actool.Errorf("q 必填"), nil
			}
			// 排除调用者自身这条意图的步骤（worker 的自有 trace 已在其上下文里）。
			acts, err := t.ts.ActivityTraceSearchAllWithSources(t.ownerNode, a.Q, a.Limit)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			hits := make([]map[string]any, 0, len(acts))
			for i := range acts {
				var intent int64
				if acts[i].NodeID != nil {
					intent = *acts[i].NodeID
				}
				hit := map[string]any{
					"intent_id": intent, "step_id": acts[i].ID, "worker": acts[i].Worker,
					"kind": acts[i].Kind, "tool": acts[i].Tool, "is_error": acts[i].IsError,
					"summary": firstLine(acts[i].Summary, 100),
				}
				if acts[i].Inherited {
					inheritedMap(hit, acts[i].SourceTaskID)
				}
				hits = append(hits, hit)
			}
			return jsonResult(map[string]any{"query": a.Q, "hits": hits})
		})
}

// listWorkerTraces gives a worker (which has no graph_overview and can't see the
// intent graph) a lightweight index of the works in this task — intent_id +
// one-line summary + state — so it can DISCOVER which works to inspect via
// get_worker_trace. Without this a worker only knows intent_ids that come back
// from search_all_worker_traces hits. Excludes still-open intents (not yet run →
// no process to inspect).
func (t *ToolSet) listWorkerTraces() actool.CoreTool {
	return t.readExpTool("list_worker_traces",
		"【システムがすでにほとんどの情報を提供しているため、通常は使用を推奨しません】このタスクで【実行済みの work（インテント）】の索引を一覧表示します：intent_id + 一文の方向(summary) + 状態。あなた(worker)には探索グラフが見えないので、確認する価値のある work を見つけるために使い、そのあと get_worker_trace(intent_id) でステップを、get_worker_trace(intent_id, step_ids=[...]) で詳細を確認します。実行済みのもの(running/done/exhausted/blocked/stopped)だけを列挙し、まだ実行していない open は含みません。注意：あなたのタスクの範囲は、受け取ったインテントのままです。他の work を見るのは、観察を再利用したり、作業の重複を避けたりするためだけです。",
		obj(map[string]any{
			"q":     str("summary のキーワードで絞り込み（任意）"),
			"limit": intp("返却件数の上限。デフォルト 50（任意）"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Q     string `json:"q"`
				Limit int    `json:"limit"`
			}
			_ = json.Unmarshal(in, &a)
			limit := a.Limit
			if limit <= 0 {
				limit = 50
			}
			all, err := t.ts.ListByKindWithSources(db.KindIntent, 500)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			q := strings.ToLower(strings.TrimSpace(a.Q))
			out := make([]map[string]any, 0, limit)
			for _, n := range all {
				if n.Inherited && n.State == "running" {
					continue
				}
				switch n.State {
				case "running", "done", "exhausted", "blocked", "stopped": // has run → has a process
				default:
					continue
				}
				var p map[string]any
				_ = json.Unmarshal(n.Payload, &p)
				summary, _ := p["summary"].(string)
				if q != "" && !strings.Contains(strings.ToLower(summary), q) {
					continue
				}
				item := map[string]any{"intent_id": n.ID, "summary": summary, "state": n.State}
				if n.Inherited {
					inheritedMap(item, n.SourceTaskID)
				}
				out = append(out, item)
				if len(out) >= limit {
					break
				}
			}
			return jsonResult(map[string]any{"works": out})
		})
}

// PlannerTools is the read + intent-generation + goal-judgement tool set.
func (t *ToolSet) PlannerTools() []actool.CoreTool {
	return []actool.CoreTool{
		t.graphOverview(), t.listFindings(), t.listFacts(), t.nodeDetail(),
		// cold-digest §6.1: restore folded cold nodes (digest body → members → detail).
		t.expandDigest(),
		t.getWorkerOutput(), t.getWorkerTrace(), t.searchAllWorkerTraces(), t.listGoals(), t.addIntent(), t.proveGoal(), t.goalMet(),
		t.killWorkTool(), t.steerWorkTool(),
		// report_finding：规划态势研判时若自身已确证漏洞，可直接登记（与 worker 同工具）。
		t.addFinding(),
		// list_companies：查看企业列表 + scope + 资产数（拿 company_id / 理解归属范围）。
		t.listCompanies(),
		// list_assets：规划时按 DSL 检索全资产库（配合 list_untested_assets 的"范围内未测"视角，
		// 补上"按域名/指纹/端口/状态码等条件在整库里查"的能力）。
		t.listAssets(),
		// add_company_scope：规划时可把域名/IP/CIDR/ICP/关键词纳入某公司的资产范围（自动认领命中资产）。
		t.addCompanyScope(),
		// add_task_scope：主动把整根域/整公司/某子域/IP 纳入本任务测试范围(覆盖度分母)。
		t.addTaskScope(),
		// list_untested_assets：按需查本任务范围内未测资产(类型+分页)，自行决定补测。
		t.listUntestedAssets(),
	}
}
