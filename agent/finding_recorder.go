package agent

import (
	"context"

	"github.com/Autumn-27/artex/db"
)

// FindingRecorder is injected by the host; agents never synthesize or copy
// evidence bodies themselves. Its implementation owns the atomic write.
type FindingRecorder interface {
	Record(context.Context, db.RecordFindingInput, []db.TrafficRef) (*db.RecordedFinding, error)
}

// Tool-use guidance is appended without replacing the user's editable prompt.
// It does not require capture or claim that unavailable traffic tools exist.
const findingTrafficGuidance = "\n\n**脆弱性のトラフィック証拠（任意）**：report_finding で脆弱性を報告するとき、脆弱性の結論を支えると確認済みの HTTP リクエスト/レスポンスがあれば、traffic_refs で再現順に実際の ID を関連付けてください。ドメインと時刻は候補の絞り込みにだけ使い、関連の推定には使いません。TCP など HTTP 以外の脆弱性、未採取、または正確に一致するものがないときは、省略するか [] を渡し、evidence にコマンド出力やログなど、ほかの検証できる証拠を残してください。関連付けなかった理由を書くことを勧めます。ID を推測しないでください。パケットを補うためだけに、探索を繰り返さないでください。"

func (t *ToolSet) SetFindingRecorder(r FindingRecorder)   { t.findingRecorder = r }
func (w *Worker) SetFindingRecorder(r FindingRecorder)    { w.findingRecorder = r }
func (p *Planner) SetFindingRecorder(r FindingRecorder)   { p.findingRecorder = r }
func (m *MainAgent) SetFindingRecorder(r FindingRecorder) { m.findingRecorder = r }
