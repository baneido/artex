package server

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

// 空転ターン（思考だけで、本文もツールも無い）の判定と継続。steerHooks.Stop を参照。

func assistantThinking(text string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{Type: llm.BlockThinking, Thinking: text, Signature: "sig"},
	}}
}

func TestIsThinkingOnlyTurn(t *testing.T) {
	toolUse := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{Type: llm.BlockThinking, Thinking: "まずポートをスキャンする"},
		{Type: llm.BlockToolUse, ID: "t1", Name: "run_nuclei"},
	}}
	cases := []struct {
		name string
		msgs []llm.Message
		want bool
	}{
		{"思考のみ", []llm.Message{llm.UserText("開始"), assistantThinking("考えてみる")}, true},
		{"思考とツール", []llm.Message{llm.UserText("開始"), toolUse}, false},
		{"思考と本文", []llm.Message{assistantThinking("考える"), {
			Role:    llm.RoleAssistant,
			Content: []llm.ContentBlock{{Type: llm.BlockThinking, Thinking: "x"}, llm.TextBlock("結論")},
		}}, false},
		{"本文が空白文字だけ", []llm.Message{{
			Role:    llm.RoleAssistant,
			Content: []llm.ContentBlock{{Type: llm.BlockThinking, Thinking: "x"}, llm.TextBlock("  \n ")},
		}}, true},
		{"assistant のターンが完全に空", []llm.Message{{Role: llm.RoleAssistant}}, true},
		// ツール結果は user ロールである。判定は直前の assistant まで戻る。近くのメッセージで誤判定しない。
		{"最後がツール結果", []llm.Message{toolUse, {
			Role:    llm.RoleUser,
			Content: []llm.ContentBlock{{Type: llm.BlockToolResult, ToolUseID: "t1"}},
		}}, false},
		{"assistant のメッセージがない", []llm.Message{llm.UserText("開始")}, false},
		{"履歴が空", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isThinkingOnlyTurn(c.msgs); got != c.want {
				t.Fatalf("isThinkingOnlyTurn = %v, want %v", got, c.want)
			}
		})
	}
}

// fakeHooks はプログラムできる inner の HookRunner で、steerHooks が inner の決定を尊重するかを確かめる。
type fakeHooks struct {
	prevent  bool
	blocking []string
	msg      string
}

func (f fakeHooks) PreToolUse(context.Context, string, []byte) (bool, string, []byte) {
	return false, "", nil
}
func (f fakeHooks) PostToolUse(context.Context, string, []byte, []byte, bool) {}
func (f fakeHooks) Stop(context.Context, []llm.Message) (bool, []string, string) {
	return f.prevent, f.blocking, f.msg
}

func TestSteerHooksStopNudgesEmptyTurn(t *testing.T) {
	empty := []llm.Message{assistantThinking("まずサブドメインを列挙すべきだ")}

	t.Run("空転ターンに継続の指示を注入する", func(t *testing.T) {
		h := steerHooks{nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges, label: "worker-1 · #1"}
		prevent, blocking, _ := h.Stop(context.Background(), empty)
		if prevent {
			t.Fatal("空転ターンで強制停止してはいけません")
		}
		if len(blocking) != 1 || blocking[0] != emptyTurnNudge {
			t.Fatalf("blocking = %v, want [emptyTurnNudge]", blocking)
		}
	})

	t.Run("本文かツールがあるときは介入しない", func(t *testing.T) {
		h := steerHooks{nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges}
		normal := []llm.Message{{
			Role:    llm.RoleAssistant,
			Content: []llm.ContentBlock{llm.TextBlock("スキャンは終わり、開いているポートは見つからなかった")},
		}}
		if _, blocking, _ := h.Stop(context.Background(), normal); blocking != nil {
			t.Fatalf("正常な終了を空転と誤判定しました: %v", blocking)
		}
		if n := h.nudges.Load(); n != 0 {
			t.Fatalf("介入していないのにカウントしました。got %d", n)
		}
	})

	t.Run("上限に達したあとは終了を許可する", func(t *testing.T) {
		const limit = 5 // ユーザーが「空応答の再試行回数」を 5 に設定した
		h := steerHooks{nudges: &atomic.Int64{}, limit: limit}
		for i := 1; i <= limit; i++ {
			if _, blocking, _ := h.Stop(context.Background(), empty); len(blocking) != 1 {
				t.Fatalf("%d 回目はまだ枠内であるべきです。blocking = %v", i, blocking)
			}
		}
		if _, blocking, _ := h.Stop(context.Background(), empty); blocking != nil {
			t.Fatalf("上限を超えても注入しています: %v", blocking)
		}
	})

	// 「空応答の再試行回数」を -1 にするとこの層はオフになり、emptyTurnNudgeLimit は 0 になる。
	t.Run("設定がオフのときは介入しない", func(t *testing.T) {
		h := steerHooks{nudges: &atomic.Int64{}, limit: 0}
		if _, blocking, _ := h.Stop(context.Background(), empty); blocking != nil {
			t.Fatalf("オフなのに注入しています: %v", blocking)
		}
	})

	t.Run("inner が強制停止するときは重ねない", func(t *testing.T) {
		h := steerHooks{inner: fakeHooks{prevent: true, msg: "guard が終了を拒否"}, nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges}
		prevent, blocking, msg := h.Stop(context.Background(), empty)
		if !prevent || msg != "guard が終了を拒否" || blocking != nil {
			t.Fatalf("inner の強制停止が書き換わりました: prevent=%v blocking=%v msg=%q", prevent, blocking, msg)
		}
		if n := h.nudges.Load(); n != 0 {
			t.Fatalf("inner に譲るときは枠を消費してはいけません。got %d", n)
		}
	})

	t.Run("inner がすでに継続を求めているときは重ねない", func(t *testing.T) {
		h := steerHooks{inner: fakeHooks{blocking: []string{"guard の継続理由"}}, nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges}
		_, blocking, _ := h.Stop(context.Background(), empty)
		if len(blocking) != 1 || blocking[0] != "guard の継続理由" {
			t.Fatalf("inner の継続メッセージが書き換わりました: %v", blocking)
		}
	})

	t.Run("カウンタが無いときの挙動は変わらない", func(t *testing.T) {
		h := steerHooks{limit: defaultEmptyTurnNudges} // たとえば将来、別の呼び出し元が nudges を渡し忘れたとき
		if _, blocking, _ := h.Stop(context.Background(), empty); blocking != nil {
			t.Fatalf("カウンタが無いのに注入しました: %v", blocking)
		}
	})
}
