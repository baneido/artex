package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Autumn-27/norma/harness"
)

// runTrace retains the latest tool call so an interrupted run can identify the
// operation that was still in flight.
type runTrace struct {
	startedAt time.Time
	id        string
	name      string
	input     string
	at        time.Time
	pending   bool
}

func (t *runTrace) start(id, name, input string) {
	t.id, t.name, t.input, t.at, t.pending = id, name, input, time.Now(), true
}

func (t *runTrace) done(id string) {
	if id == t.id {
		t.pending = false
	}
}

var reasonHint = map[harness.TerminalReason]string{
	harness.ReasonCompleted:         "モデルはこのラウンドを正常に終えましたが、文章の要約は残していません。事実と資産は、このラウンドのツール呼び出し記録を正とします",
	harness.ReasonMaxTurns:          "ステップ数の上限 (MaxTurns) に達しました。SDK は締めを実行し、事実と資産を書き戻しています。インテントは exhausted になり、失敗としては扱わず、プランナーが方向を変えて続けます",
	harness.ReasonTimeout:           "単発実行の壁時計予算 (MaxDuration) に達しました。時刻になると、実行中のツールを中断してその場で締めに入り、識別済みの事実と資産を書き戻します。インテントは exhausted になります",
	harness.ReasonModelError:        "モデルまたは API 呼び出しが失敗しました（ネットワーク、認証、レート制限、ベンダーの 5xx など）。再試行を使い切るとインテントは blocked になります。トランスポート層の障害で、このインテントはほぼ実際には探れていません。実行過程（get_worker_trace）を見てから、再送するか方法を変えるか決めてください",
	harness.ReasonBlockingLimit:     "コンテキスト長がハード上限に達し、リクエストは送信前にインターセプトされました。インテントの粒度を狭めるか、ツールの戻りを圧縮してください",
	harness.ReasonPromptTooLong:     "プロンプトが長すぎ、コンテキスト圧縮の再試行も使い切りました。実行を続けられません",
	harness.ReasonImageError:        "現在のモデルは、このラウンドのマルチモーダル内容に対応していません。視覚に対応するモデルへ切り替えるか、ツールが画像を返さないようにしてください",
	harness.ReasonStopHookPrevented: "Stop フックがこのラウンドの終了を止め、その後も続けられませんでした。タスクの Guard ルールが厳しすぎないか確認してください",
	harness.ReasonHookStopped:       "ツールまたはフックが、実行の継続を自ら止めました。範囲外の目標や、禁止されたコマンドなどです。最後の tool_result のインターセプト説明を確認してください",
	harness.ReasonAbortedStreaming:  "実行は、モデル出力のストリーミング生成中に取り消されました",
	harness.ReasonAbortedTools:      "実行は、ツール実行中に取り消されました",
}

// terminalText renders a terminal event with no final text into a compact summary
// and a Markdown detail block.
func terminalText(ctx context.Context, term *harness.Terminal, tr *runTrace) (string, string) {
	reason := term.Reason
	aborted := reason == harness.ReasonAbortedStreaming || reason == harness.ReasonAbortedTools
	// Prompt may return ctx.Err directly without a terminal event. Preserve the
	// cancellation cause instead of falling back to an empty/unknown terminal reason.
	if reason == "" && ctx.Err() != nil {
		aborted = true
	}

	var sum string
	if aborted {
		_, short, _, ok := AbortReason(ctx)
		if !ok {
			short = "取消理由を取得できませんでした"
		}
		stage := "実行の途中"
		switch reason {
		case harness.ReasonAbortedStreaming:
			stage = "モデル出力の段階"
		case harness.ReasonAbortedTools:
			stage = "ツール実行の段階"
		}
		sum = "（実行は中断されました：" + short + "。停止位置は" + stage + progressSuffix(term, tr) + "。未完了）"
	} else if reason == harness.ReasonMaxTurns || reason == harness.ReasonTimeout {
		sum = "（実行予算の上限 (" + string(reason) + ") に達し、締めとして事実を書き戻しました" + progressSuffix(term, tr) + "。今回は文章の要約がありません）"
	} else {
		hint := terminalReasonHint(reason)
		sum = "（文章の要約はありません。終態 " + terminalReasonLabel(reason) + "：" + firstLine(hint, 80) + "）"
	}

	var b strings.Builder
	b.WriteString(sum)
	b.WriteString("\n\n")
	displayReason := terminalReasonLabel(reason)
	fmt.Fprintf(&b, "- **終態**: `%s` - %s\n", displayReason, terminalReasonHint(reason))
	if aborted {
		code, _, why, ok := AbortReason(ctx)
		if ok {
			fmt.Fprintf(&b, "- **中断理由** (`%s`): %s\n", code, why)
		} else {
			b.WriteString("- **中断理由**: 取得できません。取消側が context.WithCancelCause で名前付きの理由を付けていない可能性があります\n")
		}
	}
	if term.Err != nil {
		fmt.Fprintf(&b, "- **下層のエラー**: `%v`\n", term.Err)
	}
	if aborted && strings.TrimSpace(term.Text) != "" {
		b.WriteString("- **取消前に生成済みの部分出力**:\n\n")
		b.WriteString(term.Text)
		b.WriteString("\n\n")
	}
	if term.Turns > 0 {
		fmt.Fprintf(&b, "- **実行済み**: %d ターンのモデル往復\n", term.Turns)
	}
	if !tr.startedAt.IsZero() {
		fmt.Fprintf(&b, "- **今回の実行時間**: %s\n", roundDur(time.Since(tr.startedAt)))
	}
	if u := term.Usage; u.InputTokens+u.OutputTokens+u.CacheReadTokens+u.CacheWriteTokens > 0 {
		fmt.Fprintf(&b, "- **累計 token**: 入力 %d / 出力 %d / キャッシュ読取 %d / キャッシュ書込 %d\n",
			u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens)
	}
	if tr.name == "" {
		b.WriteString("- **ツール呼び出し**: 今回の実行は、ツール呼び出しを出す前に終わりました\n")
	} else if tr.pending {
		fmt.Fprintf(&b, "- **中断時に実行中だったツール**: `%s`（%s 実行済み、**結果は返っていません**）\n\n  ```json\n  %s\n  ```\n",
			tr.name, roundDur(time.Since(tr.at)), firstLine(tr.input, 300))
	} else {
		fmt.Fprintf(&b, "- **中断前の最後のツール**: `%s`（正常に返っています）\n", tr.name)
	}
	return sum, b.String()
}

func terminalReasonLabel(reason harness.TerminalReason) string {
	if reason == "" {
		return "context_canceled"
	}
	return string(reason)
}

func terminalReasonHint(reason harness.TerminalReason) string {
	if hint := reasonHint[reason]; hint != "" {
		return hint
	}
	if reason == "" {
		return "実行の context は取り消されましたが、下層は Terminal イベントを出していません"
	}
	return "未知の終態です。harness が TerminalReason を追加した可能性があるので、reasonHint を補ってください"
}

func progressSuffix(term *harness.Terminal, tr *runTrace) string {
	var parts []string
	if term.Turns > 0 {
		parts = append(parts, fmt.Sprintf("%d ターン", term.Turns))
	}
	if !tr.startedAt.IsZero() {
		parts = append(parts, roundDur(time.Since(tr.startedAt)))
	}
	if len(parts) == 0 {
		return ""
	}
	return "。実行済み " + strings.Join(parts, " / ")
}

func roundDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return d.Round(100 * time.Millisecond).String()
	case d < time.Hour:
		return d.Round(time.Second).String()
	default:
		return d.Round(time.Minute).String()
	}
}
