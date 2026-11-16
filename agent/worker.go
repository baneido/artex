package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// Worker is an LLM work agent (docs §4.4): it claims ONE intent, completes it
// with real tools (Bash: kali tooling through the recording proxy), writes the
// FACTS it found back into the graph, and stops. It does NOT generate new
// directions (that is the planner's job) and does NOT keep exploring toward the
// goal on its own. Multiple workers run concurrently as goroutines.
// WebSearchOpts is the web-search backend selection the server pushes into each
// agent (planner/worker/main). Enabled=false leaves the web_search tool off.
// Backend is "ddgs" (no key), "brave-free" (BraveKey required), "tavily"
// (TavilyKey required), or "deepseek" (DeepSeek* required, filled from the
// active LLM profile). It maps directly onto agentcore.Options.
// Proxy is a dedicated egress proxy for the search request (http/https/socks5),
// independent of the traffic-recording MITM proxy — set it when the search endpoint
// is only reachable via a VPN/SOCKS proxy. Empty = direct.
//
// 注意。deepseek バックエンドは他の 3 つと性質が違う。DeepSeek には直接呼べる検索 API がなく、
// 検索はその Anthropic 互換 messages API の内部（web_search_20250305 server
// tool）にだけある。そのため検索のたびにモデル呼び出しを 1 回消費し、検索リクエストは DeepSeek のサーバーが出す。
// このマシンの Proxy を通らず、トラフィックの痕跡にも入らない。
type WebSearchOpts struct {
	Enabled   bool
	Backend   string
	BraveKey  string
	TavilyKey string
	Proxy     string
	// DeepSeek* は、現在有効な LLM 設定から来る（anthropic 形式の DeepSeek 公式エンドポイントだけ）。
	// 単独では設定せず、LLM 設定の切り替えに従って変わる。
	DeepSeekBaseURL string
	DeepSeekAPIKey  string
	DeepSeekModel   string
}

type Worker struct {
	findingRecorder FindingRecorder
	prov            llm.Provider
	model           string
	workDir         string
	proxyAddr       string
	proxyCACert     string            // recording proxy's CA cert path (for WebFetch HTTPS verify)
	webSearch       WebSearchOpts     // web_search tool backend selection (off by default)
	tx              *transcript.Store // raw LLM conversation persistence (nil = off)
	window          int               // context window in tokens (for compaction)
	windowFn        func() int        // optional dynamic task-chain minimum
	maxTurns        int               // max agent turns per run (0 = unlimited)
	// runTimeout is the wall-clock budget for the main exploration of one intent
	// (0 = unlimited). When it fires, the run is cut and a settlement round is
	// forced so already-identified facts get written back instead of being lost.
	runTimeout time.Duration
	// extraTools are host-provided tools (e.g. traffic query, oast) appended to
	// the worker's graph write-back tools.
	extraTools []actool.CoreTool
	// injectConstraints resolves whether this task's operation constraints get
	// injected into the worker system prompt. Read per run so the settings toggle
	// takes effect without rebuilding the agent. nil = inject (default).
	injectConstraints func() bool
	// nonStreamingFn resolves whether this run uses the non-streaming (Complete)
	// path. Read per run so a profile/task toggle takes effect without rebuilding
	// the agent. nil = streaming (default).
	nonStreamingFn func() bool
	// noaEnabledFn resolves whether this run uses the experimental noa context-
	// compression mechanism. Read per run, like nonStreaming. nil = off (built-in
	// compaction).
	noaEnabledFn func() bool
	// maxTokensFn resolves the per-reply output cap in tokens, on the same
	// per-run basis. nil or 0 = send no cap and let the endpoint decide.
	maxTokensFn func() int
}

// WorkerSessionID returns the stable transcript key used by a worker intent.
// Worker slots are reusable, so the intent id (rather than work#N) is the
// session identity. Keep this helper public so the Worker message API and UI
// can refer to exactly the conversation that will be resumed.
func WorkerSessionID(explorationID, intentID int64) string {
	return fmt.Sprintf("exp%d-worker-i%d", explorationID, intentID)
}

const workerChatMarkerPrefix = "<!-- ARTEX_WORKER_CHAT:"

func workerChatMarker(requestID string) string {
	return workerChatMarkerPrefix + requestID + " -->"
}

func hasWorkerChatMessage(messages []llm.Message, requestID string) bool {
	marker := workerChatMarker(requestID)
	for _, message := range messages {
		if message.Role == llm.RoleUser && strings.Contains(message.Text(), marker) {
			return true
		}
	}
	return false
}

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default). Read per
// run so a profile or task-chain toggle takes effect without rebuilding.
func (w *Worker) SetNonStreaming(fn func() bool) { w.nonStreamingFn = fn }

func (w *Worker) nonStreaming() bool { return w.nonStreamingFn != nil && w.nonStreamingFn() }

// SetNoaEnabled wires a resolver deciding whether runs use the experimental noa
// context-compression mechanism. nil/unset = off (built-in compaction). Read per
// run so the settings toggle takes effect without rebuilding the agent.
func (w *Worker) SetNoaEnabled(fn func() bool) { w.noaEnabledFn = fn }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (w *Worker) SetMaxTokens(fn func() int) { w.maxTokensFn = fn }

func (w *Worker) maxTokens() int {
	if w.maxTokensFn == nil {
		return 0
	}
	return w.maxTokensFn()
}

// SetConstraintInject wires a resolver deciding whether this task's operation
// constraints get injected into the worker system prompt. nil = inject (default).
func (w *Worker) SetConstraintInject(fn func() bool) { w.injectConstraints = fn }

// wantConstraints reports whether constraint injection is enabled (default yes).
func (w *Worker) wantConstraints() bool { return w.injectConstraints == nil || w.injectConstraints() }

// SetRunTimeout configures the per-intent wall-clock budget for the main
// exploration (0 = unlimited). When it fires, the SDK settlement phase still runs
// so facts are never lost to a timeout. Safe to call before Execute.
func (w *Worker) SetRunTimeout(run time.Duration) {
	w.runTimeout = run
}

// settleWrapUpPrompt is injected by the SDK settlement phase when a worker hits its
// turn/time budget: stop probing, write back what was found, then end with a
// plain-text one-liner (which becomes this run's displayed result).
const settleWrapUpPrompt = "予算を使い切って停止されます。コマンドや探査はもう実行しないでください。順に行ってください。(1) 上で識別済みだが、まだ書き戻していない内容を 1 件ずつ書き戻してください。新しい資産は insert_assets、探索の結論や事実は record_fact、確認した脆弱性は report_finding です。(2) **最後にプレーンテキストの 1 文だけ**で、何をしたか、どの重要な結論を得たかまとめてください（この文が今回の実行結果として表示されます。必ず出力してください）。"

func NewWorker(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int, extra ...actool.CoreTool) *Worker {
	return &Worker{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns, extraTools: extra}
}

// defaultToolsExcept returns actool.DefaultTools() minus the named tools (by
// CoreTool.Name()). Used to trim SDK default tools an agent shouldn't have.
func defaultToolsExcept(exclude ...string) []actool.CoreTool {
	drop := make(map[string]bool, len(exclude))
	for _, n := range exclude {
		drop[n] = true
	}
	all := actool.DefaultTools()
	out := make([]actool.CoreTool, 0, len(all))
	for _, t := range all {
		if !drop[t.Name()] {
			out = append(out, t)
		}
	}
	return out
}

func (w *Worker) SetCompactionWindowResolver(fn func() int) { w.windowFn = fn }

func (w *Worker) compactionWindow() int {
	if w.windowFn != nil {
		return w.windowFn()
	}
	return w.window
}

// SetProxy configures the recording proxy address that workers route target
// traffic through, plus the CA cert path WebFetch trusts to verify HTTPS through
// that MITM proxy. Empty addr disables the hint.
func (w *Worker) SetProxy(addr, caCert string) { w.proxyAddr, w.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for this worker (off by default).
func (w *Worker) SetWebSearch(o WebSearchOpts) { w.webSearch = o }

// proxyEnv builds the Bash-subprocess env that routes child-command HTTP through
// the egress proxy (the recording MITM when capture is on, or the global proxy
// directly when it is off) and, only when a MITM CA is present, makes the common
// toolchain trust it — so tools need no manual -x/--proxy/-k. Each ecosystem reads
// a different CA var (verified empirically): SSL_CERT_FILE→curl/urllib/Go/openssl,
// REQUESTS_CA_BUNDLE→python requests (it ignores SSL_CERT_FILE), CURL_CA_BUNDLE→curl,
// GIT_SSL_CAINFO→git, NODE_EXTRA_CA_CERTS→node; NODE_USE_ENV_PROXY makes Node 24+
// honor the proxy vars. ALL_PROXY is set too so a socks5 egress proxy (which curl
// only reads from ALL_PROXY, not HTTP(S)_PROXY) works in the capture-off path.
// Empty proxyAddr → nil (direct, unchanged env).
func proxyEnv(proxyAddr, caCert string) []string {
	if proxyAddr == "" {
		return nil
	}
	env := []string{
		"HTTP_PROXY=" + proxyAddr, "HTTPS_PROXY=" + proxyAddr,
		"http_proxy=" + proxyAddr, "https_proxy=" + proxyAddr,
		"ALL_PROXY=" + proxyAddr, "all_proxy=" + proxyAddr, // socks5 egress: curl reads only this
		"NODE_USE_ENV_PROXY=1", // Node 24+: honor HTTP(S)_PROXY in built-in fetch/http
	}
	if caCert != "" {
		env = append(env,
			"SSL_CERT_FILE="+caCert,
			"CURL_CA_BUNDLE="+caCert,
			"REQUESTS_CA_BUNDLE="+caCert,
			"GIT_SSL_CAINFO="+caCert,
			"NODE_EXTRA_CA_CERTS="+caCert,
		)
	}
	return env
}

// workerDefaultTmpl is the built-in EDITABLE body (区分 [A]) of the worker system
// prompt, seeded into agent_prompts. The trafficTool block and the 中間生成物の出力規約
// are NOT here — they are code-owned and appended by workerSystem after rendering
// (区分 [B]/[C]), so editing the DB body can never drop them.
const workerDefaultTmpl = `あなたは、サイバーセキュリティプラットフォーム上の認可された侵入テストシステムの「実行者」(work agent) です。受け取るのは【インテント 1 件】（探索方向の 1 文）です。唯一の責務は、**このインテント 1 件を完了し、発見をナレッジグラフへ書き戻し、それから止まって戻ること**です。

**境界（越えてはいけない線）**：
1. **受け取ったこのインテントだけを行う**。**このインテントを探っているときに、このインテントの外で深く掘る価値のある手がかりをちらりと見た**（エラーが漏らしたパス、他の資産と連動し得る点、別の利用チェーンの入口らしいもの）なら、**fact の summary に 1 言書き、プランナーへ渡してください**。
2. 最初に阻まれたこと（payload がフィルタされた、404、インジェクションが無エコー）は、探り切ったことを意味しません。このインテントの回避手段をすべて通してから結論を出してください。
3. 認可範囲の中だけで操作してください。システムプロンプトの先頭に【操作の制約】が付いていれば、それは最優先の越えてはいけない線です。コマンドや探査を 1 つ実行する前に自己確認し、違反するなら実行しないでください（受け取ったインテントの中に入っていてもです）。

**見つけたらその場で書き戻す**（グラフに書いたものだけが数えられます。頭や文章の中は数えられません。結果を 1 つ得るたびにすぐ書き、最後まで溜めてステップ数切れで捨てないでください）。書き戻しは 3 種類です。グラフを取り違えないでください。
- **新しい資産やリソース → insert_assets（資産グラフ）**：サブドメイン、service、endpoint、フィンガープリント、認証情報など、資産【そのもの】すべて。**ここでは資産だけを登録します。探索の結論や判断はここに書かず、record_fact を使ってください。**
- **探索の結論や事実 → record_fact（探索グラフ。intent_id を渡す）**：どれもこれを使います。**複数の観察は【1 件の】事実にまとめます**（summary は要約の 1 文、detail はその要約の展開。実際の実行過程に基づく）。属性 1 つを 1 件にせず、1 インテントは通常 1 件です。細かく割るとグラフが際限なく膨らみます。**既定は 1 件で、detail に入れられるものはすべて入れてください**。【互いに完全に独立し、まとめられない】結論が確かにあるときだけ facts 配列で分けます。これはごく少数の例外であり、通常ではありません。**増分だけを書く**：今回【新しく得た】ものだけを記し、既存の事実を言い換えて書き直さないでください（既存を裏付けるだけで新規がなければ、記す必要はありません）。**実際に見たものだけを書く**：evidence を付ける（1 行。コマンドと、最もよく証明する 1 から 2 行の出力。簡潔に。詳細は detail）、confidence を付ける（observed=直接見た、inferred=現象から推論）。
- **確認した脆弱性 → report_finding（探索グラフ。PoC を含む。intent_id を渡す）**：**今回本当に発動させ、再現できる証拠（リクエスト/レスポンス、またはコマンド出力）を取ったときだけ使う**。「バージョンやフィンガープリントが CVE に一致した」「パラメータがインジェクションできそうに見える」「外部の脆弱性データベース、更新ログ、コード diff からの推論」を確認済みにしてはいけません。CVE データベースの照会や、パッチバージョンの比較で、実際の発動を代替しないでください。発動できないが疑いがあるなら、record_fact で inferred の事実を 1 件記し（疑いの点と、なぜ発動しなかったか）、プランナーへ渡し、無理に finding にしないでください。


このインテントを終えたら、何をしたか、どの事実を書き戻したかを 1 文でまとめてください。`

// workerTrafficBlock is 区分 [B]: the traffic-tool note, code-injected only when
// traffic capture (recording) is on — i.e. the traffic_* tools actually exist.
// Gated on recording, NOT on the egress proxy: a global proxy with capture off
// routes traffic but records nothing, so the tools would not be there. Not stored,
// not editable.
func workerTrafficBlock(recording bool) string {
	if !recording {
		return ""
	}
	return "\n\n**トラフィックツール**：\n- traffic_search / traffic_get / traffic_blob：応答を見返し、訪問済みのリソースを探すときは、**先にトラフィックを調べ、同じ URL を curl で繰り返さないでください**。traffic_search は **host の指定が必須**で、既定ではごく軽い索引を 3 件だけ返します（id/method/url/status/resp_len。応答内容なし）。もっと必要なら limit を明示的に上げてください。body_contains でリクエスト/レスポンス本文を全文検索できます（3 文字以上。部分文字列と中国語に対応。パスワード、鍵、エラー、内部アドレスを探すなど）。ある件の原文を見るには traffic_get(id) を使います。大きすぎる本文は @blob sha256:<hash> と表示されるので、traffic_blob(hash) で分割して全文を取ります。"
}

// artifactSpec is 区分 [C]: the code-owned, non-editable tail appended to every
// pentest agent's prompt — intermediate artifacts must land in the shared work
// dir, never /tmp. Guaranteed present regardless of how the DB body is edited.
func artifactSpec(dir string) string {
	return "\n\n**中間生成物の出力規約**：スクリプト、payload、取得したレスポンス本文、一時データなど、中間生成物はすべて、**このタスクの作業ディレクトリ " + dir + " に書いてください**（相対パスはここに落ち、この絶対パスも使えます）。**/tmp や他の絶対パスには書かないでください**。"
}

// workerArtifactSpec is the worker's 区分 [C]: its per-intent run dir is pre-created
// by the engine (ensureRunDir), so it just writes relative paths there — no manual
// mkdir, no cross-worker name collisions.
func workerArtifactSpec(runDir string) string {
	return "\n\n**中間生成物の出力規約**：スクリプト、payload、取得したレスポンス本文、一時データなど、中間生成物はすべて、**今回のインテント専用の作業ディレクトリ " + runDir + " に書いてください**（自動で作成済みです。相対パスをここに直接書いてよく、ディレクトリを手で作る必要はありません）。**/tmp や他の絶対パスには書かないでください**。"
}

// ensureRunDir builds and creates an agent's working directory under base:
// <base>/tasks/<taskID> for planner/main; <base>/tasks/<taskID>/i<intentID> for a
// worker (intentID<=0 → task dir only). The "tasks/" segment groups per-task dirs
// symmetrically with the chat agent's "sessions/<sessionID>". Best-effort mkdir — on
// failure, writes fail the same way an unwritable CWD would.
func ensureRunDir(base string, taskID, intentID int64) string {
	dir := filepath.Join(base, "tasks", strconv.FormatInt(taskID, 10))
	if intentID > 0 {
		dir = filepath.Join(dir, "i"+strconv.FormatInt(intentID, 10))
	}
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// cmdOutDir is the SDK large-tool-output spill dir under an agent's run dir.
func cmdOutDir(dir string) string { return filepath.Join(dir, "cmd-output") }

func workerSystem(proxyAddr, caCert, dataDir string) string {
	body := renderSystem("worker", workerDefaultTmpl, WorkerVars{ProxyAddr: proxyAddr, DataDir: dataDir, Now: nowStr()})
	// caCert is present only when the recording MITM is on, which is exactly when
	// the traffic_* tools are registered — so it gates the traffic-tool note.
	// Optional finding guidance is added for every role after tool resolution.
	return body + workerTrafficBlock(caCert != "")
}

// renderIntentTask formats the claimed intent for the worker's launch USER message:
// the intent is the worker's whole job. It used to live in the system prompt; it now
// rides in the first user turn (together with the situational overview) so the system
// prompt stays static/role-only — same move as the planner's situational block.
// intentAssetIDs pulls the intent's target asset ids out of its payload
// (planner's add_intent stores them as a numeric asset_ids array). nil on absence
// or malformed payload.
func intentAssetIDs(intent *db.Node) []int64 {
	if intent == nil {
		return nil
	}
	var p struct {
		AssetIDs []int64 `json:"asset_ids"`
	}
	if err := json.Unmarshal(intent.Payload, &p); err != nil {
		return nil
	}
	return p.AssetIDs
}

func renderIntentTask(intent *db.Node) string {
	return fmt.Sprintf("\n\n【受け取ったインテント（今回の唯一のタスク：これだけを行い、事実だけを生み、終わったら止まる）】：\n%s\nインテント id: %d（record_fact / report_finding へ書き戻すときに渡す）", string(intent.Payload), intent.ID)
}

// renderWorkerGraphOverview folds the global situational snapshot into the worker's
// launch USER message for AWARENESS ONLY. The framing is deliberately strong: the overview
// must NOT widen the worker's job — it still does only its assigned intent. Its sole
// purpose is letting the worker read context (existing facts/assets/hints)
// so it avoids redundant work and doesn't re-derive what others already found.
func renderWorkerGraphOverview(data map[string]any) string {
	// coverage は、プランナーが「どの種類のテストが少ないか、範囲を広げるか」を判断する信号であり、worker の「受け取った
	// インテントだけを行い、未カバーの点を追わない」という責務の境界と相反する。worker のビューから除く。data はこの worker
	// 専用の新しい map なので、キーを消しても planner には影響しない。
	delete(data, "coverage")
	b, err := json.Marshal(data)
	if err != nil {
		return "" // fall back silently: the worker just won't have the global context
	}
	return "\n\n【全体の探索状況（読み取り専用。自分のこのインテントを大局の中に置くためのもの）】：\n" +
		"下は、タスク全体の現在の探索の概況です。用途は 2 つです。1 つは、他者が何を見つけたかを知り、繰り返さないこと。もう 1 つは、自分のこのインテントを探るとき、それと全体の関係を連想できるようにすることです。\n" +
		"**発散はよいことです**。このインテントを探るときは、深く考え、連想を広げてください。唯一の境界は、他のインテントを実際に実行しないことです（それは他の worker の仕事で、プランナーが割り当てます）。価値のある手がかりを連想したら（資産をまたぐ連動、別の利用チェーンの入口らしいもの、全体レベルの疑わしい点）、**必ず fact に書いてプランナーへ渡してください**。これは重要な成果であり、あってもなくてもよいものではありません。プランナーに判断させる 1 件を多く出すほうを選び、自分の中にしまい込まないでください。\n" +
		string(b)
}

// Execute runs one intent. hooks (the per-task Guard) gates every tool call; may
// be nil. emit, if non-nil, receives one ActivityRecord per execution step.
// notifyFinding, if non-nil, is called (intentID, summary) when this worker writes
// a finding (report_finding) so the task's planner wakes mid-flight — with context
// on which intent found what — instead of waiting for the worker to finish.
// Returns the terminal reason (so the engine can distinguish completed vs
// max_turns) and a per-kind breakdown of what was written back (so an intent that
// explored but persisted nothing isn't mistaken for done, and the engine can log
// facts/assets/findings separately instead of lumping them under "facts").
func (w *Worker) Execute(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), enr EnrichTrigger, notifyFinding func(int64, string)) (harness.TerminalReason, WriteCounts, error) {
	return w.execute(ctx, name, taskID, as, ts, intent, hooks, emit, enr, notifyFinding, "", "")
}

// ExecuteWithMessage runs the next turn in the same intent conversation with a
// human-authored message. The HTTP handler does not edit the transcript;
// agentcore records the message as a normal user turn when this Worker starts.
// This keeps Worker continuation identical to the regular agent chat flow.
func (w *Worker) ExecuteWithMessage(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), enr EnrichTrigger, notifyFinding func(int64, string), requestID, message string) (harness.TerminalReason, WriteCounts, error) {
	return w.execute(ctx, name, taskID, as, ts, intent, hooks, emit, enr, notifyFinding, strings.TrimSpace(requestID), strings.TrimSpace(message))
}

func (w *Worker) execute(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), enr EnrichTrigger, notifyFinding func(int64, string), requestID, message string) (harness.TerminalReason, WriteCounts, error) {
	tsx := NewToolSet(ts, name)
	tsx.SetFindingRecorder(w.findingRecorder)
	tsx.SetTaskID(taskID)
	coverageEnabled := as == nil || as.CoverageEnabled(taskID)
	tsx.SetCoverageEnabled(coverageEnabled)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetOwnerNode(intent.ID)         // assets this worker discovers anchor to its intent → visible to the task
	tsx.SetEnrich(enr)                  // async DNS/HTTP auto-completion for assets this worker writes
	tsx.SetNotifyFinding(notifyFinding) // report_finding の保存時にその場で planner を起こし、「どのインテントと finding か」を付ける
	// base = built-in worker tools ∪ host tools (traffic) ∪ default tools (incl. Bash);
	// then augment with the agent's visible skills/MCP. During the SDK settlement
	// phase, Bash is hidden via Settlement.DisabledTools (no local gating needed).
	base := append(tsx.WorkerTools(), w.extraTools...)
	// worker には MultiEdit/Glob/Grep を意図的に渡さない。ファイルの精密な修正は Edit、検索は Bash（grep/find）とする。
	// ツール面を絞り、価値の低い呼び出しを減らす。残りの SDK 既定ツール（Read/Write/Edit/LS/Bash/Sleep）はそのまま渡す。
	base = append(base, defaultToolsExcept("MultiEdit", "Glob", "Grep")...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts), IntentID: intent.ID})
	tools, def, cleanup := AugmentTools(ctx, "worker", base)
	defer cleanup()

	// インテントは worker の【唯一の責務であり、run 全体を貫く不変量】である。起動指示と、インテントが指す対象資産の
	// 生データとともに system prompt へ入れる。system は run のたびに組み直し、compaction で押し潰されない。
	// 長い run でもインテントは常にあり、再開時も transcript の履歴が先頭メッセージを残しているかに依存しない。代償は、system に
	// インテントごとの変わりやすいデータが混ざり、インテントをまたぐキャッシュ再利用を失うことである。これは意図的な取捨である（インテントを失うほうが、token を節約することより重大である）。
	// planner の「状況ブロックを user turn に置く」との分岐は意図的である。planner 自身がインテントを生む側で、単一の mandate を持たない。
	// worker は持つ。【全体状況の overview】だけを起動 user メッセージに残す。これは下げられ、古くても許容でき、押し潰されても差し支えない。
	// 今回のインテント専用の作業ディレクトリ <workDir>/tasks/<taskID>/i<intentID> は、エンジン側で先に作る。
	runDir := ensureRunDir(w.workDir, taskID, intent.ID)
	// The run-wide intent is not the current tool action. Do not forward it or
	// inherit a parent run's background into the action reviewer.
	ctx = intercept.WithReviewContext(ctx, runDir, intercept.ReviewBackground{})
	overview := renderWorkerGraphOverview(tsx.graphOverviewData())
	sysBody := workerSystem(w.proxyAddr, w.proxyCACert, w.workDir)
	if w.wantConstraints() {
		sysBody += constraintBlock(ts) // 操作の制約があればシステムプロンプトへ注入し、worker は実行時に厳守する
	}
	// インテントの塊、インテントが指す資産の塊、起動指示の順に、system の末尾へ追加する（constraintBlock と同じ追加の仕方）。
	intentTail := workerArtifactSpec(runDir) + renderIntentTask(intent)
	if as != nil {
		if ids := intentAssetIDs(intent); len(ids) > 0 {
			if assets, err := as.GetByIDs(ids); err == nil && len(assets) > 0 {
				if b, err := json.Marshal(assets); err == nil {
					intentTail += "\n\nこのインテントの asset_ids に対応する対象資産：\n" + string(b)
				}
				// インテントが明確に対象とするこれらの資産は、タスクのテスト範囲へ自動で入れる（insertAssets と同じ
				// 保守的な粒度）。upsertTaskScope の ON CONFLICT DO NOTHING と uq_task_scope
				// の一意索引が、重複追加を防ぐ。再実行や再試行も同様に冪等な no-op である。
				// 資産カバレッジ機能がオフのときは、テスト範囲（分母）をこれ以上積まない。
				if coverageEnabled {
					for _, a := range assets {
						_ = as.AddAutoScope(taskID, a.Type, a.Domain, a.URL, a.IP)
					}
				}
			}
		}
	}
	intentTail += "\n\n上のインテントの実行を始めてください。これだけを行い、事実、assets、finding だけを生み、終わったら止まってください。"
	system, boundary := deferredSystemWithTail(sysBody, intentTail, def)
	// タスク単位の deadline（ctx 経由で注入）が、この run の壁時計予算を挟み、締め文を決める（taskclock.go を参照）。
	tc := taskClockFrom(ctx)
	maxDur, clamped := clampMaxDuration(tc.DeadlineUnix, w.runTimeout)
	settle := wrapupSettlement("worker", []string{"Bash"})
	if tc.DeadlineUnix > 0 {
		settle = wrapupSettlementForTask("worker", []string{"Bash"}, clamped)
	}
	opts := agentcore.Options{
		Provider:        w.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		// WebFetch は記録プロキシを通り、その HTTP は curl と同じく痕跡が残る。プロキシ CA を読み、MITM が
		// 再署名した HTTPS 証明書を【通常どおり検証して通す】（検証を切るのではない）。proxy が空なら直接接続。
		EnableWebFetch: true,
		WebFetchProxy:  w.proxyAddr,
		WebFetchCACert: w.proxyCACert,
		// ネット検索（任意）。ddgs は key 不要。brave-free は BraveKey、tavily は TavilyKey が必要。
		// WebSearchProxy は独立した出口プロキシ（http/https/socks5）で、トラフィックを記録する MITM プロキシとは別。空なら直接接続。
		EnableWebSearch:       w.webSearch.Enabled,
		WebSearchBackend:      w.webSearch.Backend,
		BraveSearchAPIKey:     w.webSearch.BraveKey,
		TavilySearchAPIKey:    w.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: w.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  w.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   w.webSearch.DeepSeekModel,
		WebSearchProxy:        w.webSearch.Proxy,
		// Bash 子コマンドの HTTP は、既定で記録プロキシを通り、その CA を信頼する（ツールに -x/-k は不要）。
		BashEnv:    proxyEnv(w.proxyAddr, w.proxyCACert),
		WorkingDir: runDir,
		MaxTurns:   w.maxTurns, // 0 = unlimited (configurable in agent management)
		// 壁時計予算。ターン境界で判定し、途中は中断しない。0 = 無制限。タスク単位の deadline があるときは min（自身の予算、
		// deadline までの残り）まで挟み、この run がタスクの時刻に達したときに自然に締めへ入る（taskclock.go を参照）。
		MaxDuration: maxDur,
		// 予算（ラウンド数または時間）に達すると SDK が締めを 1 ラウンド実行し（Bash を隠す）、識別済みを書き戻して中途半端な終わりを避ける。
		// clamped（タスク deadline に挟まれた）のときは PromptByReason を使う。タイムアウトはタスクが時刻に達したのでタスクタイムアウト文。
		// ステップ数は、挟まれた窓の中で先に尽きたので per-run の文へ戻る。clamped でなければ純粋な per-run のまま。
		Settlement: settle,
		// large tool output spills to cmd-output/ with a head + pointer (SDK tool.Capture);
		// full output preserved on disk. 切り詰め上限は SDK の既定（30000 文字）。
		ToolOutputDir: cmdOutDir(runDir),
		Compaction:    compactionConfig(w.compactionWindow()), // long tool-heavy runs stay within the window
		Todos:         actool.NewTodoStore(),                  // セッション単位の一時的な TODO（TodoWrite）。計画専用で、終了時に捨てる
		NonStreaming:  w.nonStreaming(),                       // この profile が非ストリーミングを選んだときは Provider.Complete を使う
		MaxTokens:     w.maxTokens(),                          // 0 = 上限を送らない。サーバーの既定値に任せる
	}
	if hooks != nil { // typed-nil guard: only set when concrete (avoids harness panic)
		opts.Hooks = hooks
	}
	if w.tx != nil { // persist raw LLM conversation; one file per worked intent
		opts.Transcript = w.tx
		opts.SessionID = WorkerSessionID(ts.ID(), intent.ID)
	}
	intentID := intent.ID
	emitWrap := func(r db.Activity) {
		if emit != nil {
			r.NodeID, r.Worker = &intentID, name
			emit(r)
		}
	}
	// インテント、起動指示、インテントが指す資産は、system prompt と一緒に渡してある（上の sysBody の組み立てを参照）。
	// この起動 user メッセージが載せるのは【全体状況の overview】だけである。大局を知るための下げられる情報で、押し潰されても差し支えない。
	// overview がまれに marshal に失敗して空になるときは、起動の一文へ戻し、最初のラウンドが空の user メッセージにならないようにする。
	input := overview
	if strings.TrimSpace(input) == "" {
		input = "system で受け取ったインテントの実行を始めてください。これだけを行い、事実、assets、finding だけを生み、終わったら止まってください。"
	}

	// 実験機能。有効にすると noa がコンテキスト圧縮を担う（アーカイブは <workDir>/noa/<SessionID> に集まり、永続）。
	noaSession := WorkerSessionID(ts.ID(), intent.ID)
	enableNoa(&opts, w.noaEnabledFn, w.workDir, noaSession, noaWarn(noaSession))
	ctx = attachSideCapture(ctx, &opts)
	s := agentcore.NewSession(opts)
	defer s.Close() // release the session's background-task manager (temp dir + processes)

	// Resume prior conversation if this intent was paused/blocked/exhausted and is
	// being re-run. The transcript ID is deterministic per intent, so if a prior
	// session exists the worker continues from where it left off instead of
	// restarting from scratch.
	alreadyRecorded := false
	if w.tx != nil {
		_ = s.Resume(opts.SessionID)
		alreadyRecorded = requestID != "" && hasWorkerChatMessage(s.Messages(), requestID)
		if len(s.Messages()) > 0 && message == "" {
			seedUnlockFromHistory(s.Messages(), def.UnlockSkill)
			input = "実行を続けてください。"
		} else if len(s.Messages()) > 0 {
			seedUnlockFromHistory(s.Messages(), def.UnlockSkill)
		}
	}
	if message != "" {
		if alreadyRecorded {
			input = "前回の人手の対話で入力された新しいインテントの実行を続けてください。完了済みの動作を繰り返さないでください。"
		} else if len(s.Messages()) > 0 {
			input = workerChatMarker(requestID) + "\n【人手の対話で入力された新しいインテント】\n" + message +
				"\n\nこの人手の入力をすぐに実行してください。完了したあと、文脈に基づいて元のタスクを続ける必要があるか決めてください。"
		} else {
			input += "\n\n" + workerChatMarker(requestID) + "\n【人手の対話で入力された新しいインテント】\n" + message +
				"\n\nこの人手の入力を優先して実行してください。"
		}
	}

	// Budgets + settlement are owned by the SDK (MaxTurns/MaxDuration + Settlement):
	// on hit it runs a wrap-up turn and finishes with ReasonMaxTurns/ReasonTimeout.
	// MaxDuration now interrupts an in-flight tool at the wall-clock deadline and
	// enters the wrap-up phase on the live ctx, so a run whose tool overran the budget
	// still settles (no external hard-timeout backstop needed). ctx itself carries only
	// pause / planner kill / shutdown, which the engine distinguishes and re-queues/stops.
	_, reason, err := captureRunSession(ctx, s, input, emitWrap)
	return reason, tsx.Writes(), err
}
