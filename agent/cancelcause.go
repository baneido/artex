package agent

import (
	"context"
	"errors"
	"fmt"
)

// AbortCause names why an agent run's context was cancelled. Every cancellation
// site should attach one so the activity trace can report the real initiator.
type AbortCause struct {
	Code  string
	Short string
	Text  string
}

func (c *AbortCause) Error() string { return c.Text }

func cause(code, short, text string) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: text}
}

// Causef builds a cause that includes runtime-specific detail.
func Causef(code, short, format string, args ...any) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: fmt.Sprintf(format, args...)}
}

var (
	// Task-level execution context.
	AbortPausedByUser = cause("paused_by_user", "ユーザーがタスクを一時停止しました",
		"ユーザーがタスク制御 API（POST /api/tasks/{id}/control、action=pause）でタスクを一時停止しました。今回の Planner/Worker 実行は明示的に取り消されます。実行中のインテントは frontier(open) に戻り、タスク再開後に取り直して最初から実行します")
	AbortPausedByOrchestrator = cause("paused_by_orchestrator", "編成 Agent がタスクを一時停止しました",
		"編成 Agent が pause_task ツールでこのタスクを一時停止しました。今回の Planner/Worker 実行は明示的に取り消されます。実行中のインテントは frontier(open) に戻り、再開後に再実行します")
	AbortTaskDeleted = cause("task_deleted", "タスクが削除されています",
		"タスクを削除中です（DELETE /api/tasks/{id}）。削除バリアが、そのタスクで実行中の Planner、Worker、メイン Agent を取り消しました。今回の実行結果はもう使われません")
	AbortPausedOnReload = cause("paused_on_reload", "バックエンドがタスクの一時停止を復元しました",
		"バックエンド起動時に、データベースへ永続化されていた状態からタスクの一時停止を復元しました。今回の実行は取り消されます。通常、復元の段階で実行中の Agent はありません")
	AbortGoalMet = cause("goal_met", "プランナーがタスク目標の達成を判定しました",
		"プランナーがタスク目標の達成を判定してタスクを done にし、その後も実行中の Worker を取り消しました。これらのインテントは失敗ではなく stopped になります")
	AbortSettleDrainTimeout = cause("settle_drain_timeout", "タスクタイムアウト時の締めの待ち時間が尽きました",
		"タスクが timeout に達したあと、実行中の Worker の締めを待ちましたが、90 秒の drain 猶予でも足りなかったため、強制取消を実行しました。インテントは exhausted になります。締めの段階で書き込んだ事実と資産は残します")

	// Per-work context.
	AbortKilledByPlanner = cause("killed_by_planner", "プランナーがこのインテントを終了しました",
		"プランナーが kill_work でこのインテントを明示的に終了しました。方向が逸れた、または続ける価値がなくなった、という意味であることが多いです。インテントは stopped になり、自動では取り直しません")
	AbortWorkPausedByUser = cause("work_paused_by_user", "ユーザーがこの Worker インテントを一時停止しました",
		"ユーザーが実行中の Worker を一時停止しました。今回の呼び出しは取り消され、インテントは paused になります。登録済みのインテント、事実、脆弱性、アクティビティ記録はすべて残し、再開後は最初から再実行します")
	AbortWorkCancelledByUser = cause("work_cancelled_by_user", "ユーザーがこの Worker インテントを削除しました",
		"ユーザーが実行中の Worker を削除しました。今回の呼び出しは取り消されます。Worker が書き込み領域を出たあと、サーバーはユーザーが選んだ削除モードでこのインテントを処理します。仮削除は削除済みの印を付けるだけで、成果物はすべて残します。真の削除は、このインテントと、これだけが支えている下流ノードを連鎖して除きます")
	AbortWorkFinished = cause("work_finished", "Worker は正常終了し context を解放しました",
		"Worker は正常終了し、エンジンが detachWork でその context を解放しています。これは実行の中断ではありません。中断メッセージに出るなら、取消と片付けのイベントが競合しています")
	AbortPausedRaceGuard = cause("paused_race_guard", "一時停止中は新しい実行を拒否します",
		"タスクが一時停止中のとき、エンジンは新しい実行 context を出しません。claim と一時停止の競合で Worker が起動し続けるのを防ぐためです。取得済みのインテントは frontier に戻します")

	// Main Agent and standalone conversation contexts.
	AbortChatStoppedByUser = cause("chat_stopped_by_user", "ユーザーがこの対話を停止しました",
		"ユーザーが停止を押し、このラウンドのメイン Agent またはセッション Agent の実行を明示的に止めました。すでに生まれたアクティビティ記録は残ります。次のメッセージは送れます")
	AbortChatPausedWithTask = cause("chat_paused_with_task", "タスク一時停止でメイン Agent の対話も止めました",
		"ユーザーがタスクを一時停止したとき、実行中のメイン Agent の対話も同時に取り消しました。すでに生まれたアクティビティ記録は残ります。タスクを再開しても、このラウンドのメッセージは自動では再送しません")
	AbortChatTurnFinished = cause("chat_turn_finished", "この対話は正常終了し context を解放しました",
		"この対話は正常終了し、サーバーがそのラウンドの context を解放しています。これは実行の中断ではありません。中断メッセージに出るなら、取消と片付けのイベントが競合しています")

	// Process-level and per-run hard backstop.
	AbortShutdown = cause("shutdown", "バックエンドプロセスが終了しています",
		"バックエンドプロセスが SIGINT または SIGTERM を受け、再起動、アップデート、または終了の途中です。実行中の Agent はすべて取り消されます。再起動後、残った running のインテントは open に戻して再実行します")
	AbortRunHardTimeout = cause("run_hard_timeout", "単発実行のハードタイムアウトが発動しました",
		"単発の実行が、柔らかい壁時計予算と追加の猶予を超えました。モデル要求、またはいずれかのツールが長く結果を返さず、通常のターン境界での締めが実行できていません。中断前に最後まで結果を返していないツール呼び出しを重点的に確認してください")
)

// AbortReason resolves the named cause attached to a cancelled run context.
func AbortReason(ctx context.Context) (code, short, text string, ok bool) {
	c := context.Cause(ctx)
	if c == nil {
		return "", "", "", false
	}
	var ac *AbortCause
	if errors.As(c, &ac) {
		return ac.Code, ac.Short, ac.Text, true
	}
	switch {
	case errors.Is(c, context.DeadlineExceeded):
		return "deadline_exceeded", "上流 context が deadline に達しました",
			"上流 context が deadline に達しましたが、設定側は WithTimeoutCause で名前付きの理由を付けていません: " + c.Error(), true
	case errors.Is(c, context.Canceled):
		return "canceled_no_cause", "取消側が名前付きの理由を付けていません",
			"上流 context は取り消されましたが、取消側は context.WithCancelCause で名前付きの理由を付けていません。agent/cancelcause.go に理由を登録し、その取消箇所へ接続してください", true
	default:
		return "other", firstLine(c.Error(), 80), c.Error(), true
	}
}
