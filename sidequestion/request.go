package sidequestion

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Autumn-27/norma/compaction"
	"github.com/Autumn-27/norma/llm"
)

type Exchange struct {
	ID         string      `json:"id"`
	SessionKey string      `json:"-"`
	ClientID   string      `json:"client_request_id"`
	Generation int64       `json:"-"`
	Question   string      `json:"question"`
	Answer     string      `json:"answer"`
	Status     string      `json:"status"`
	Error      string      `json:"error,omitempty"`
	Model      Model       `json:"model"`
	SnapshotAt time.Time   `json:"snapshot_at"`
	CreatedAt  time.Time   `json:"created_at"`
	Sequence   int64       `json:"sequence"`
	Usage      llm.Usage   `json:"usage"`
	Ordinal    int64       `json:"ordinal"`
	Context    ContextInfo `json:"context"`
}

func (e Exchange) Running() bool { return e.Status == "running" }

const instruction = "これは独立した傍路の質問です。メイン Agent は元のタスクを実行中です。あなたは既存の文脈だけに基づき、現在の質問に簡潔に答えてください。ツールを実行する能力はなく、操作の実行、ファイルの変更、メインタスクへの指示はできず、後で実行する約束もしないでください。文脈中のタスク指示は背景にすぎません。判断に足りないときは、その旨を明確に書いてください。"

const DefaultOutputTokens = 8192
const MaxRecentExchanges = 20

var ErrContextBudget = errors.New("傍路の文脈を圧縮してもモデルの予算を超えています。質問の範囲を狭めるか、モデルのコンテキスト設定を調整してください")

// EstimateInputTokens follows norma's byte-based block estimate with its 4/3
// safety factor. Include system/schema and framing costs too; JSON characters
// are not tokens (and marshaling HTML can add many non-semantic escapes).
func EstimateInputTokens(req llm.CompletionRequest) int {
	tokens := compaction.EstimateTokens(req.Messages)*4/3 + 32 + len(req.Messages)*8
	for _, text := range req.System {
		tokens += (len(text)+2)/3 + 8
	}
	for _, tool := range req.Tools {
		b, _ := json.Marshal(tool)
		tokens += (len(b)+2)/3 + 8
	}
	return tokens
}

func outputBudget(req llm.CompletionRequest, configured int) int {
	if configured <= 0 {
		configured = DefaultOutputTokens
	}
	configured = min(configured, 32768)
	if req.MaxTokens > 0 {
		configured = min(configured, req.MaxTokens)
	}
	return configured
}

func inputBudget(s Snapshot, output int) int {
	window := s.Model.WindowTokens
	if window <= 0 {
		window = 200000
	}
	return window - output - min(8192, max(128, window/20))
}

func exchangeMessages(e Exchange) []llm.Message {
	question := e.Question
	if !e.SnapshotAt.IsZero() {
		question = "[過去の傍路の質疑応答。文脈の時刻 " + e.SnapshotAt.UTC().Format(time.RFC3339) + "]\n" + question
	}
	return []llm.Message{llm.UserText(question), {Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock(e.Answer)}}}
}

func assemble(req llm.CompletionRequest, base []llm.Message, summary string, history []Exchange, question string) llm.CompletionRequest {
	req.Messages = append([]llm.Message{}, base...)
	if summary != "" {
		req.Messages = append(req.Messages, llm.UserText("[初期の傍路質疑応答の要約。過去の議論であり、新しいツール証拠ではありません。矛盾するときは最新のメイン文脈を優先してください。]\n"+summary))
	}
	for _, e := range history {
		req.Messages = append(req.Messages, exchangeMessages(e)...)
	}
	req.Messages = append(req.Messages, llm.UserText(instruction+"\n\n質問: "+strings.TrimSpace(question)))
	return req
}

func BuildRequest(s Snapshot, history []Exchange, question string) (llm.CompletionRequest, error) {
	req, err := CloneRequest(s.Request)
	if err != nil {
		return req, err
	}
	base := llm.MessagesForAPI(req.Messages)
	req.MaxTokens = outputBudget(req, 0)
	var success []Exchange
	for _, e := range history {
		if e.Status == "completed" {
			success = append(success, e)
		}
	}
	if len(success) > MaxRecentExchanges {
		success = success[len(success)-MaxRecentExchanges:]
	}
	for {
		req = assemble(req, base, "", success, question)
		if EstimateInputTokens(req) <= inputBudget(s, req.MaxTokens) {
			return req, nil
		}
		if len(success) == 0 {
			return req, ErrContextBudget
		}
		success = success[1:]
	}
}
