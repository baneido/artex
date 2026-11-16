package agent

import (
	"strings"

	"github.com/Autumn-27/norma/harness"
)

// 締めのプロンプト (wrap-up / settlement prompt)。agent が【ステップ数の尽きたとき (MaxTurns)】または
// 【タイムアウト (run_seconds/MaxDuration)】で止められるとき、SDK の settlement 段階がこのプロンプトを注入する。
// agent は、識別済みでまだ書き戻していない内容を先に保存し、そのあと要約を 1 文出して、中途半端な終わりを避ける。
//
// 各 agent の締めプロンプトは管理画面で必要に応じて上書きできる（agents.wrapup_prompt）。空ならここの
// 組み込み既定を使う。編集できるのは【プロンプト本文】だけである。どのツールを無効にするか、締め自身に何ラウンド渡すかは、コードで固定する方針である。

// WrapupOverride, if set, returns the stored wrap-up prompt for an agent key and
// whether a non-empty one exists. Wired by the server to the agents table (like
// PromptOverride for system prompts). nil / empty → the built-in default is used.
var WrapupOverride func(agentKey string) (string, bool)

// WrapupMaxTurnsOverride, if set, returns the admin-configured turn budget for the
// wrap-up phase of an agent and whether a positive one exists. Wired to the agents
// table. nil / ≤0 → the built-in per-agent default (wrapupTurnDefaults) is used.
var WrapupMaxTurnsOverride func(agentKey string) (int, bool)

// 組み込みの既定の締めプロンプト。agent key で引く。worker は、以前ハードコードしていた settleWrapUpPrompt
// （worker.go で定義）を再利用する。planner と mainagent はそれぞれ 1 版ある。当たらないもの（カスタム agent）は汎用の予備へ回る。
var wrapupDefaults = map[string]string{
	"worker":    settleWrapUpPrompt,
	"planner":   plannerWrapUpDefault,
	"mainagent": mainAgentWrapUpDefault,
}

// wrapupTurnDefaults: 各 agent の締め段階【自身】のラウンド数予算の組み込み既定（管理画面の >0 で上書きできる）。
// いずれも 10 ラウンドとし、締めの段階で保存に足りるステップ数を確保する。当たらなければ genericWrapupTurns へ回る。
var wrapupTurnDefaults = map[string]int{
	"worker":    10,
	"planner":   10,
	"mainagent": 10,
}

const genericWrapupTurns = 10

const plannerWrapUpDefault = "このラウンドの計画のステップ数が尽きます。終わるのは【このラウンド】だけです。システムはその後も状況の変化に応じてあなたを起こし、計画を続けます。タスクの終了ではないので、ここで計画全体を畳む必要はありません。このラウンドで既に固めた結論は保存し、このラウンドを無駄にしないでください。ただし【締めのためにインテントを無理に埋めないでください】（このラウンドのインテントが 0 件でも、完全に正常な結果です）。(1) 【今すぐ出す】探索方向が判断できているなら、add_intent を 1 回でまとめて出してください（決めたものを出さずに溜めないでください）。(2) いずれかの検出事項や事実が達成を証明している目標は、prove_goal で met にしてください（判定漏れをしないでください）。(3) 段階に分ける直列の利用チェーンが見えたら、TodoWrite に残し、次に起きたときに続きを出せるようにしてください。終わったらこのラウンドをそのまま終了し、要約テキストは不要です。"

const mainAgentWrapUpDefault = "ステップ数が尽きます。今回の対話は終わります。新しい探索や操作を始めないでください。**プレーンテキストの 1 文だけ**で、現在の進捗、重要な結論、次に勧める一手をユーザーへまとめてください。"

const genericWrapUpDefault = "予算を使い切って停止されます。まず、完了しているのに未保存の結果を書き戻してください。そのあと**プレーンテキストの 1 文だけ**で、何をしたか、どの重要な結論を得たかまとめてください（この文が今回の実行結果として表示されます）。"

// WrapupDefault returns the built-in default wrap-up prompt for an agent key —
// used by the admin UI as the "restore default" value and empty-field placeholder.
func WrapupDefault(agentKey string) string {
	if d, ok := wrapupDefaults[agentKey]; ok {
		return d
	}
	return genericWrapUpDefault
}

// WrapupTurnsDefault returns the built-in wrap-up turn budget for an agent key —
// used by the admin UI as the "0 = default N" hint.
func WrapupTurnsDefault(agentKey string) int {
	if n, ok := wrapupTurnDefaults[agentKey]; ok {
		return n
	}
	return genericWrapupTurns
}

// resolveWrapup returns the effective wrap-up prompt: the DB override (if set and
// non-empty) over the built-in default.
func resolveWrapup(agentKey string) string {
	if WrapupOverride != nil {
		if t, ok := WrapupOverride(agentKey); ok && strings.TrimSpace(t) != "" {
			return t
		}
	}
	return WrapupDefault(agentKey)
}

// resolveWrapupTurns returns the effective wrap-up turn budget: a positive DB
// override over the built-in per-agent default.
func resolveWrapupTurns(agentKey string) int {
	if WrapupMaxTurnsOverride != nil {
		if v, ok := WrapupMaxTurnsOverride(agentKey); ok && v > 0 {
			return v
		}
	}
	return WrapupTurnsDefault(agentKey)
}

// wrapupSettlement builds the settlement config for an agent's run. Prompt and the
// turn budget are admin-editable per agent; disabled tools are code-owned policy so
// a user can't edit away the "stop probing" guardrail. Resolved fresh each run
// (reads DB live), so edits apply on the next run without a restart.
func wrapupSettlement(agentKey string, disabledTools []string) *harness.Settlement {
	return &harness.Settlement{
		Prompt:        resolveWrapup(agentKey),
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
}

// ---------- タスク単位のタイムアウト締め文（docs/任务级超时与收尾设计.md を参照）----------
//
// per-run の締め文とは【別の 2 系統】である。per-run は「この 1 回の run の予算を使い切った」。タスクのタイムアウトは
// 「タスク全体が時刻に達し、終わろうとしている」。意味はしばしば逆になる（特に planner。per-run は「止まらず計画を続けよ」と言い、
// タスクのタイムアウトは「時刻なので計画を止め、最後の判定をせよ」と言う）。設定するのは worker と planner だけである。

// WrapupTaskTimeoutOverride / …TurnsOverride は、タスクタイムアウトの締め文とラウンド数の DB 上書きである
// （agents.task_timeout_wrapup_prompt / _max_turns へつなぐ。worker と planner だけ）。
var (
	WrapupTaskTimeoutOverride      func(agentKey string) (string, bool)
	WrapupTaskTimeoutTurnsOverride func(agentKey string) (int, bool)
)

var taskTimeoutWrapupDefaults = map[string]string{
	"worker":  workerTaskTimeoutDefault,
	"planner": plannerTaskTimeoutDefault,
}

const workerTaskTimeoutDefault = "**タスク全体がタイムアウト上限に達し、終わろうとしています**（今回の run の予算ではなく、探索全体が時刻に達しました）。これが最後の機会です。(1) 識別済みでまだ書き戻していない内容を【すべて】保存してください。新しい資産は insert_assets、探索の結論や事実は record_fact、確認した脆弱性は report_finding です。(2) 新しいコマンドや探索を始めないでください。(3) **最後にプレーンテキストの 1 文だけ**で、このインテントでの重要な結論をまとめてください。"

const plannerTaskTimeoutDefault = "**タスク全体がタイムアウト上限に達し、終わろうとしています**（このラウンドではなく、タスク全体の終了です）。現在の【すべての】事実と検出事項に基づき、目標の最終判定をしてください。証拠が達成を証明している目標は prove_goal で met にしてください（判定漏れをしないでください）。**新しいインテントはもう生成しないでください**（この時点で出しても実行されません）。判定が終わったら畳み、要約テキストは不要です。"

// TaskTimeoutWrapupDefault は、ある agent のタスクタイムアウト用の組み込み既定の締め文を返す（管理画面のプレースホルダと既定復元用）。
func TaskTimeoutWrapupDefault(agentKey string) string {
	return taskTimeoutWrapupDefaults[agentKey] // 未設定（mainagent/chat）は空文字を返す
}

// resolveTaskTimeoutWrapup は、DB 上書き（空でないもの）を組み込み既定より優先する。空文字は、その agent にタスクタイムアウト文がないことを表す
// （worker/planner 以外）。そのとき呼び出し側は per-run の文へ戻す。
func resolveTaskTimeoutWrapup(agentKey string) string {
	if WrapupTaskTimeoutOverride != nil {
		if t, ok := WrapupTaskTimeoutOverride(agentKey); ok && strings.TrimSpace(t) != "" {
			return t
		}
	}
	return TaskTimeoutWrapupDefault(agentKey)
}

func resolveTaskTimeoutTurns(agentKey string) int {
	if WrapupTaskTimeoutTurnsOverride != nil {
		if v, ok := WrapupTaskTimeoutTurnsOverride(agentKey); ok && v > 0 {
			return v
		}
	}
	return resolveWrapupTurns(agentKey) // 既定では per-run のラウンド数をそのまま使う
}

// wrapupSettlementForTask builds settlement for a worker/planner run that is aware
// of the task deadline. See §5 of the design doc:
//   - clamped=true  → 今回の run はタスクの deadline に挟まれている。Timeout で締めるときはタスクが時刻に達したのでタスクタイムアウト文。
//     MaxTurns で締めるときは、挟まれた窓の中でステップ数が先に尽きただけで、タスクにはまだ数分残るので per-run の文へ戻る。
//   - clamped=false → タスクはまだ早い。どちらの reason も per-run の文を使う（wrapupSettlement へ退化する）。
//
// harness に渡す PromptByReason は、締めのときに【実際の】reason でその場で選ぶ。build 時に取り違えない。
func wrapupSettlementForTask(agentKey string, disabledTools []string, clamped bool) *harness.Settlement {
	perRun := resolveWrapup(agentKey)
	st := &harness.Settlement{
		Prompt:        perRun, // 予備（clamped でないとき、両方の reason が取る値でもある）
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
	if clamped {
		if tt := resolveTaskTimeoutWrapup(agentKey); tt != "" {
			st.PromptByReason = map[harness.TerminalReason]string{
				harness.ReasonTimeout:  tt,     // タスクが時刻に達した
				harness.ReasonMaxTurns: perRun, // ステップ数が先に尽き、タスクにはまだ時間がある
			}
			st.MaxTurns = resolveTaskTimeoutTurns(agentKey)
		}
	}
	return st
}
