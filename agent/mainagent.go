package agent

import (
	"context"
	"fmt"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// MainAgent is the thin human-interface orchestrator (docs §4.2 / §7). The human
// chats with it; it observes (read tools), and steers by injecting hints
// (→planner) or direct high-priority intents (→frontier). It does NOT run the
// autonomous intent-generation loop (that is the planner's job).
type MainAgent struct {
	findingRecorder FindingRecorder
	prov            llm.Provider
	model           string
	tx              *transcript.Store                      // raw LLM conversation persistence (nil = off)
	window          int                                    // context window in tokens (for compaction)
	windowFn        func() int                             // optional dynamic task-chain minimum
	maxTurns        int                                    // max agent turns per run (0 = unlimited)
	proxyAddr       string                                 // recording proxy for WebFetch (empty = direct)
	proxyCACert     string                                 // recording proxy's CA cert path (HTTPS verify)
	webSearch       WebSearchOpts                          // web_search tool backend selection (off by default)
	workDir         string                                 // shared work dir (surfaced in prompt as artifact-output target)
	steerWork       func(intentID int64, msg string) error // engine callback: steer a running work (nil = off)
	nonStreamingFn  func() bool                            // resolver: use non-streaming (Complete) path? (nil = streaming)
	noaEnabledFn    func() bool                            // resolver: use experimental noa compaction? (nil = off)
	maxTokensFn     func() int                             // resolver: per-reply output cap (nil/0 = send no cap)
}

// SetNoaEnabled wires a resolver deciding whether runs use the experimental noa
// context-compression mechanism. nil/unset = off (built-in compaction). Read per
// run so the settings toggle takes effect without rebuilding the agent.
func (m *MainAgent) SetNoaEnabled(fn func() bool) { m.noaEnabledFn = fn }

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default).
func (m *MainAgent) SetNonStreaming(fn func() bool) { m.nonStreamingFn = fn }

func (m *MainAgent) nonStreaming() bool { return m.nonStreamingFn != nil && m.nonStreamingFn() }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (m *MainAgent) SetMaxTokens(fn func() int) { m.maxTokensFn = fn }

func (m *MainAgent) maxTokens() int {
	if m.maxTokensFn == nil {
		return 0
	}
	return m.maxTokensFn()
}

func NewMainAgent(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int) *MainAgent {
	return &MainAgent{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns}
}

func (m *MainAgent) SetCompactionWindowResolver(fn func() int) { m.windowFn = fn }

func (m *MainAgent) compactionWindow() int {
	if m.windowFn != nil {
		return m.windowFn()
	}
	return m.window
}

// SetProxy points the main agent's WebFetch at the recording proxy plus the CA
// cert it trusts to verify HTTPS through it (empty addr = direct).
func (m *MainAgent) SetProxy(addr, caCert string) { m.proxyAddr, m.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for the main agent (off by default).
func (m *MainAgent) SetWebSearch(o WebSearchOpts) { m.webSearch = o }

// SetSteerWork wires the engine callback that lets the main agent's steer_work
// tool inject a mid-run course-correction into a running work (nil = tool off).
func (m *MainAgent) SetSteerWork(fn func(intentID int64, msg string) error) { m.steerWork = fn }

// mainAgentDefaultTmpl is the built-in EDITABLE body (区分 [A]) of the main agent
// prompt, seeded into agent_prompts. Goal is a {{.Goal}} template var; the 中間
// 生成物の出力規約 tail is code-owned (artifactSpec), appended after rendering.
const mainAgentDefaultTmpl = `あなたは、認可された侵入テストシステムの「メイン agent」であり、人間のオペレーターとの窓口です。自分では探索せず、インテントを自律的に連続生成もしません（それはプランナーの仕事です）。責務は次のとおりです。

1. 観察：graph_overview / list_findings / list_facts / list_assets / get_worker_output を使い、現在の進捗についての人の質問に答えます。
2. 操舵（人の意図をシステムへ落とす）：
   - 人が「方向を変える / ある種の脆弱性を強調する / ある領域を重点にする」と言うなら、add_hint でヒントを書きます（プランナーが次回読みます）。
   - 人が「特定の具体的な目標をすぐ測りたい」と言うなら、add_intent で高優先度のインテントを 1 件直接注入します（priority 8-10）。システムは完了済みのタスクを実行状態へ自動で戻し、worker にこのインテントを取らせて実行させ、終わると完了状態へ戻します。
     **タスク目標がすべて達成済みのとき**（graph_overview の goals がすべて met）：出す前に、このインテントの背後に「新しく達成すべき結果」が隠れているか判断してください。隠れているなら、推測した目標を 1 文で人に復唱し、**正式な目標として登録するか問い返してください**。人が望むなら set_goals で登録します（タスクはその後、通常の計画に入り、プランナーが自律的に進めます）。人が望まない、または一時的に探りたいだけなら、add_intent でこの 1 件だけを出します。worker の実行が終わるとタスクは完了状態へ戻ります（自律的には続きません）。このインテントが明らかに一回限りの確認で、新しい目標を含まないなら、add_intent を直接呼べばよく、毎回問う必要はありません。
   - 人が「実行中のインテント (work) をリアルタイムで軌道修正したい（もう X には進むな、Y に絞れ）」と言うなら、steer_work を使います（中断せず、既存の進捗も捨てず、worker の次の動作の前に効きます）。先に get_worker_output で何をしているか見てください。方向が全体として間違っているなら、add_intent で別の新しいインテントを出してください。
   - 人が「達成すべき最終目標を追加したい」と言うなら、set_goals で目標を足します。システムはその目標をタスクグラフへ書き、**完了または一時停止のタスクを実行状態へ自動で戻して続けます**（プランナーはその後、達成したかどうかを再判定します）。人が再開を押し直す必要はありません。
   - 人が「テスト制約を追加または変更したい（ある種の操作を許可または禁止する。例：『現在のポートだけを測る』『総当たりを禁止する』『パッシブ偵察だけをする』）」と言うなら、set_constraints で登録します（type=allow は許可、type=deny は禁止）。制約は次の計画ラウンドで planner/worker のプロンプトへ注入され、探索の境界を定めます。概要の「制約管理」でも追加、変更、削除できます。
3. 人の言葉で簡潔に返し、何をしたかを説明します。

現在のタスク目標：{{.Goal}}

検出事項を捏造しないでください。ツールが返した実際のデータだけに基づいて答えてください。`

func mainAgentSystem(goal, dataDir, workDir string) string {
	body := renderSystem("mainagent", mainAgentDefaultTmpl, MainVars{Goal: goal, DataDir: dataDir, Now: nowStr()})
	return body + artifactSpec(workDir)
}

// Chat handles one human message and returns the assistant reply. emit, if
// non-nil, receives each execution step (thinking / tool_use / tool_result /
// text / result) so the main-agent session shows its work — exactly like the
// worker/planner sessions — not just the final answer.
func (m *MainAgent) Chat(ctx context.Context, taskID int64, mainSeg int, as *db.AssetStore, ts *db.ExplorationStore, goal, message string, emit func(db.Activity), notify, resume func(), notifyGoal, notifyHint func([]string)) (string, error) {
	tsx := NewToolSet(ts, "human")
	tsx.SetFindingRecorder(m.findingRecorder)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetTaskID(taskID)
	tsx.SetCoverageEnabled(as == nil || as.CoverageEnabled(taskID))
	tsx.SetNotify(notify)         // 汎用の起床（専用コールバックのない書き込みはこちら。debounced）
	tsx.SetResumeTask(resume)     // set_goals で目標を追加したら、完了または一時停止のタスクを running へ戻す
	tsx.SetNotifyGoal(notifyGoal) // set_goals で目標を追加したら、planner に「人が目標を追加した：…」を 1 件記録する
	tsx.SetNotifyHint(notifyHint) // add_hint でヒントを追加したら、planner に「人が戦略ヒントを N 件追加した：…」を 1 件記録する
	tsx.steerWork = m.steerWork   // enable steer_work tool (nil = unavailable)
	// ドメインツールと、基本の既定ツールセット（Read/Write/Edit/MultiEdit/LS/Glob/Grep/Bash）
	// 資産カバレッジ機能がオフのときは add_task_scope/list_untested_assets を除く（プロンプトに入れない）。
	base := append(tsx.DropCoverageTools(tsx.MainAgentTools()), actool.DefaultTools()...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts)})
	tools, def, cleanup := AugmentTools(ctx, "mainagent", base)
	defer cleanup()
	// このタスクの作業ディレクトリ <workDir>/tasks/<taskID> を先に作る。
	mainDir := ensureRunDir(m.workDir, taskID, 0)
	ctx = intercept.WithReviewWorkingDirectory(ctx, mainDir)
	system, boundary := deferredSystem(mainAgentSystem(goal, m.workDir, mainDir), def)
	opts := agentcore.Options{
		Provider:        m.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		EnableWebFetch:  true, // 記録プロキシ経由で痕跡を残す。プロキシ CA を読み、MITM が再署名した HTTPS 証明書を検証する
		WebFetchProxy:   m.proxyAddr,
		WebFetchCACert:  m.proxyCACert,
		// ネット検索（任意）。ddgs は key 不要。brave-free は BraveKey、tavily は TavilyKey が必要。
		// WebSearchProxy は独立した出口プロキシ（http/https/socks5）で、トラフィックを記録する MITM プロキシとは別。空なら直接接続。
		EnableWebSearch:       m.webSearch.Enabled,
		WebSearchBackend:      m.webSearch.Backend,
		BraveSearchAPIKey:     m.webSearch.BraveKey,
		TavilySearchAPIKey:    m.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: m.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  m.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   m.webSearch.DeepSeekModel,
		WebSearchProxy:        m.webSearch.Proxy,
		BashEnv:               proxyEnv(m.proxyAddr, m.proxyCACert), // Bash の子コマンドは既定でプロキシ経由とし、CA を信頼する
		WorkingDir:            mainDir,                              // このタスクの作業ディレクトリ <workDir>/tasks/<taskID>
		ToolOutputDir:         cmdOutDir(mainDir),
		MaxTurns:              m.maxTurns,                             // 0 = unlimited (configurable in agent management)
		Compaction:            compactionConfig(m.compactionWindow()), // long chats stay within the window
		Todos:                 actool.NewTodoStore(),                  // セッション単位の一時的な TODO（TodoWrite）。計画専用で、終了時に捨てる
		// 予算（ステップ数）に達すると SDK が締めを実行し、進捗の要約を 1 文ユーザーへ出す。プロンプトと締めのラウンド数は管理画面で編集できる（既定 10 ラウンド）。
		Settlement:   wrapupSettlement("mainagent", nil),
		NonStreaming: m.nonStreaming(), // この profile が非ストリーミングを選んだときは Provider.Complete を使う
		MaxTokens:    m.maxTokens(),    // 0 = 上限を送らない。サーバーの既定値に任せる
	}
	if m.tx != nil { // persist raw human↔AI conversation; one accumulating file per segment
		opts.Transcript = m.tx
		// Segment 0 keeps the legacy "exp%d-main" name so existing transcripts still
		// load; each new session (seg>=1) gets its own file for a clean context.
		opts.SessionID = fmt.Sprintf("exp%d-main", ts.ID())
		if mainSeg > 0 {
			opts.SessionID = fmt.Sprintf("exp%d-main-s%d", ts.ID(), mainSeg)
		}
	}
	// 実験機能。有効にすると noa がコンテキスト圧縮を担う（アーカイブは <workDir>/noa/<SessionID> に集まり、永続）。
	// session id は transcript と同じ規則（セグメントを認識する）とし、アーカイブと復元を揃える。
	noaSession := fmt.Sprintf("exp%d-main", ts.ID())
	if mainSeg > 0 {
		noaSession = fmt.Sprintf("exp%d-main-s%d", ts.ID(), mainSeg)
	}
	enableNoa(&opts, m.noaEnabledFn, m.workDir, noaSession, noaWarn(noaSession))
	ctx = attachSideCapture(ctx, &opts)
	s := agentcore.NewSession(opts)
	defer s.Close()
	// reload the prior conversation from the transcript so the agent has context
	// across turns (each Chat is a fresh session; without this it can't see earlier
	// messages). First turn: no file yet → Resume loads nothing and proceeds.
	if m.tx != nil {
		_ = s.Resume(opts.SessionID)
	}
	// C2: this session is fresh each turn; re-unlock skill-gated MCPs from prior
	// Skill() calls in the reloaded history so revealed tools stay callable.
	seedUnlockFromHistory(s.Messages(), def.UnlockSkill)
	text, _, err := captureRunSession(ctx, s, message, func(r db.Activity) {
		if emit != nil {
			r.Worker = "mainagent"
			emit(r)
		}
	})
	return text, err
}
