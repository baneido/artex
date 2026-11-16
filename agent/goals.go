package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	acperm "github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// goalsDefaultTmpl is the built-in EDITABLE body (区分 [A]) of the goals-decomposer
// prompt, seeded into agent_prompts. No template vars are used today.
const goalsDefaultTmpl = `あなたは侵入テストの目標分解器です。ユーザー入力から**最終的に達成すべき結果**を識別してください。攻撃手順の計画は対象外です。

**最初の手順（目標を分ける前に行う）：操作の制約を抜き出す**
「タスク目標 / タスク説明」から、オペレーターが【どの操作をしてよいか、どの操作をしてはいけないか】を明確に定めた部分を識別し、set_constraints で 1 件ずつ登録してください（説明や目標に操作の制約がなければ、制約の抽出はしなくてよいです）。
- type=deny：禁止する操作（例：「ポートをスキャンしない」「本番環境への書き込みや削除をしない」「総当たりを禁止する」「あるサブドメインには触らない」）。
- type=allow：明確に許可する、または限定する操作範囲（例：「パッシブ偵察だけを許可する」「あるドメインだけを対象にする」）。
- 制約は目標でも攻撃手順でもありません。操作の境界を定めたものです。
- **制約は【それ単体で意味が通り、具体的な目標を書き切る】必要があります**。「現在の目標」「現在のポート」「現在の IP」「現在のドメイン」「このサイト」のような**指示語**は、タスク目標や説明にある**具体的な値**に置き換えてください。制約は実行段階のプロンプトへ単独で注入されるので、文脈を離れると指示語が何を指すか判断できません。
  例：目標が https://abc.example.net なら、「現在の目標だけをテストしてよい」ではなく「abc.example.net だけをテストしてよい」と書きます。「現在のポートだけを測る」ではなく「目標ポート 443 だけを測り、他のポートはスキャンしない」と書きます。原文が「現在の目標」とだけ言い、目標アドレスが既に明確なら、そのアドレスを埋めてください。
- **目標や説明に【明確に書かれている、または強調されている】制約だけを登録し、推測で作ることは禁止です**。型が決められないときは deny を使ってください（より保守的です）。
- 目標や説明に操作の制約が本当にないなら、set_constraints は**呼ばないでください**。
制約の登録（あれば）が終わってから、下の目標分割に進んでください。

**目標 = 最終的に提出でき、検証できる結果**

**目標ではない内容（子目標に列挙することを禁止します）**：
- 情報収集、偵察、エンドポイントのスキャン
- 脆弱性の分析と検証の過程
- 攻撃手順、悪用の手段
- 結果を検証する手順

**分割の原則**：
- ユーザーが述べた最終目標が 1 つなら、1 つを出力する
- **互いに独立した**最終成果物が複数あるなら、それぞれ列挙する
- 明確な脆弱性クラスに対応できるものには vulnclass を付ける。情報収集やビジネスロジックの目標は空にする
- ユーザーが言及していない目標を推測で作ることは禁止です

set_goals を呼んで結果を提出してください。`

// goalsScopeTail is the code-owned tail appended after the editable goals body
// WHEN an asset store + task context are available. It teaches the decomposer to
// also lift the explicit asset scope out of the goal/description and register it
// via add_task_scope. Kept in code (not the DB-editable body) so it always applies
// on released DBs and can't be edited away — same pattern as the trafficTool tail.
const goalsScopeTail = `

**追加の責務：テストする資産範囲を登録する**
目標の分割に加え、「タスク目標 / タスク説明」から**明示されたテスト資産の範囲**を識別し、add_task_scope で登録してください（このタスクの認可境界であり、資産テストのカバレッジの分母でもあります）。**最小範囲の原則：ユーザーが明確に指した目標 1 つだけを登録し、勝手に広げないでください。**
- 目標が URL、またはホスト名付きのアドレス（例：https://xxx.example.com/path、app.example.com）なら、その**完全なホスト名**を取り、kind=subdomain、value=完全なホスト名にします。
  例：目標 https://a1b2c3.lab.example.net/path なら kind=subdomain、value=a1b2c3.lab.example.net です（example.net **ではありません**）。
  サブドメイン付きのホスト名をルートドメインへ縮めることは**禁止です**。xxx.example.com を見て example.com 全体を登録すると、範囲がユーザーの目標の外へ広がり、最小範囲の原則に反します。
- ユーザーが渡したのが**裸のルートドメインで、サブドメインを一切含まない**場合（example.com と直接書かれている）、または「サイト全体 / すべてのサブドメイン / ドメイン全体」と明確に言った場合にだけ、kind=root_domain、value=example.com を使います。
- 純粋な IP またはネットワーク範囲なら kind=ip / cidr、value=IP または CIDR です。
- 企業範囲（company）は**登録しないでください**。タスクを作った直後は、資産システムにその企業がまだないことが多く、登録できません。企業レベルの範囲は、後続の plan 段階に任せます。
その他の規則：
- **目標や説明に明確に書かれた**範囲だけを登録してください。言及のないドメインや IP を推測したり推論したりすることは禁止です。
- reason には、根拠がどの文から来たかを短く書き、監査できるようにしてください。
- 目標や説明に明確な資産範囲が何もないなら、add_task_scope は**呼ばないでください**。
まず add_task_scope で範囲を登録し（あれば）、そのあと set_goals で目標を提出してください。`

// GoalSpec is one decomposed objective.
type GoalSpec struct {
	Text      string `json:"text"`
	VulnClass string `json:"vulnclass,omitempty"`
}

// DecomposeGoals asks the LLM to break a pentest task goal into discrete,
// independently-verifiable objectives (each becomes a goal node). Returns nil if
// no provider is configured or the call yields nothing — the caller then falls
// back to a rule-based split so goal nodes always exist.
//
// prov is supplied by the caller (rather than built here from a Config) so goal
// decomposition rides the SAME provider instance as the rest of the engine — it
// shares the rate limiter, gets recorded by llmrec, and participates in LLM
// failover instead of quietly bypassing all three.
//
// desc is the task's free-text description (背景：標的の範囲、flag の数、交戦の説明など).
// It is fed alongside the goal so the decomposer no longer splits blind — the
// prompt still forbids inventing anything the two texts don't state.
//
// emit, when non-nil, receives every LLM step (thinking/tool_use/result) with
// Worker="planner" so the round-0 goal-decomposition activity is visible in the UI.
//
// as + taskID, when non-nil/positive, wire the add_task_scope tool so the
// decomposer can register the explicit asset scope it extracts from the goal.
//
// ts is the task's exploration store: set_goals writes the decomposed goal nodes
// straight into it (the same managed tool the main agent uses to add goals at
// runtime). The returned specs are read back from the store so callers can emit
// per-goal activity and detect the "LLM produced nothing" case for their fallback.
func DecomposeGoals(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, emit func(db.Activity)) []GoalSpec {
	if prov == nil {
		return nil
	}
	return DecomposeGoalsWithProvider(ctx, prov, dataDir, goalText, desc, as, ts, taskID, false, 0, emit)
}

// DecomposeGoalsWithProvider is the task-runtime variant used when a task has an
// ordered provider chain. It preserves the same tools and write behavior while
// letting the caller own provider selection/failover. maxTokens is the profile's
// per-reply output cap (0 = send none).
func DecomposeGoalsWithProvider(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, nonStreaming bool, maxTokens int, emit func(db.Activity)) []GoalSpec {
	if prov == nil {
		return nil
	}
	// 目標分解は 1 回きりの呼び出しである。transcript store を付けないので、agentcore は ctx に
	// session id を掛けない（writer があるときだけ掛ける。agentcore.Prompt を参照）。session-id ヘッダで
	// プロンプトキャッシュやスティッキーなルーティングをするゲートウェイ（opencode zen は x-opencode-session がないと直接 400
	// MissingSessionID）は、ctx 上のこの値を読む。補わないと「対話は正常、分解は 400」になる。
	// 安定した id を明示して掛ける。同一探索の分解リクエストはこれを共有し（キャッシュに当たりやすい）、名前は
	// planner や worker と衝突せず、llmrec.parseSession が正しく帰属できる。
	if ts != nil {
		ctx = transcript.WithSessionID(ctx, fmt.Sprintf("exp%d-goals", ts.ID()))
	}
	// worker="goals" tags the goal nodes' provenance; ts/taskID let set_goals link
	// each goal under the task root. This is the catalog's real set_goals tool, so a
	// web-edited description/schema on it applies here too.
	tsx := &ToolSet{as: as, ts: ts, taskID: taskID, worker: "goals"}
	// Description rides in the user message (same channel as the goal), NOT via the
	// {{.EngagementDescription}} template var — else a prompt that references the var
	// would inject the description twice. System prompt stays pure static instructions.
	sys := renderSystem("goals", goalsDefaultTmpl, GoalsVars{DataDir: dataDir, Now: nowStr()})
	// set_constraints は常に使える（asset store に依存しない）。本文に「先に操作の制約を抜き、それから目標を分解する」手順が既にあり
	// （agent の編集画面で文言は変えられる）、ここではツールを接続するだけでよい。
	tools := []actool.CoreTool{tsx.setGoals(), tsx.setConstraints()}
	// Wire add_task_scope only when we have a real asset store + task to write to.
	// The scope-extraction tail is appended in lockstep so the prompt never asks for
	// a tool that isn't present.
	if as != nil && taskID > 0 {
		tools = append(tools, tsx.addTaskScope())
		sys += goalsScopeTail
	}
	userMsg := "タスク目標：\n" + goalText
	if d := strings.TrimSpace(desc); d != "" {
		userMsg += "\n\nタスク説明（背景情報。標的の範囲、flag の数、交戦の説明を含むことがある。参考までであり、ここに書かれていない内容を推測して作らないでください）：\n" + d
	}
	// Use captureRun so every LLM step is emitted as an activity record (visible in
	// the plan tab under the round-0 marker). Falls back gracefully when emit is nil.
	captureEmit := func(r db.Activity) {
		if emit != nil {
			r.Worker = "planner"
			emit(r)
		}
	}
	captureRun(ctx, agentcore.Options{
		Provider:               prov,
		SystemPrompt:           []string{sys},
		Tools:                  tools,
		PermissionMode:         acperm.ModeBypass,
		DisableBackgroundTasks: true,
		// 3 ステップ（制約を抜く → 範囲を登録する → 目標を分解する）は、それぞれツール呼び出しが 1 回要る。締めの前に set_goals を呼び損ねないよう、ターンを足す。
		MaxTurns:     8,
		NonStreaming: nonStreaming, // この profile が非ストリーミングを選んだときは Provider.Complete を使う
		MaxTokens:    maxTokens,    // 0 = 上限を送らない。サーバーの既定値に任せる
	}, userMsg, captureEmit)
	// set_goals persisted the goals directly; read them back so the caller sees what
	// was written (empty slice ⇒ the LLM produced nothing ⇒ caller falls back).
	if ts == nil {
		return nil
	}
	nodes, _ := ts.ListByKind(db.KindGoal, 10000)
	var out []GoalSpec
	for _, n := range nodes {
		var p struct {
			Text      string `json:"text"`
			VulnClass string `json:"vulnclass"`
		}
		_ = json.Unmarshal(n.Payload, &p)
		if strings.TrimSpace(p.Text) != "" {
			out = append(out, GoalSpec{Text: p.Text, VulnClass: p.VulnClass})
		}
	}
	return out
}
