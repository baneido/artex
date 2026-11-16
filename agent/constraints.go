package agent

import (
	"strings"

	"github.com/Autumn-27/artex/db"
)

// constraintBlock renders this task's operation constraints (task_constraints) as a
// high-priority block appended to the planner/worker system prompt. allow/deny are
// grouped; empty string when there are no constraints (or ts is nil). The framing
// deliberately puts these ABOVE the exploration/expansion heuristics so a declared
// boundary wins the tug-of-war against "chase another entry surface".
func constraintBlock(ts *db.ExplorationStore) string {
	if ts == nil {
		return ""
	}
	rows, err := ts.ListConstraints()
	if err != nil || len(rows) == 0 {
		return ""
	}
	var allow, deny []string
	for _, c := range rows {
		text := strings.TrimSpace(c.Text)
		if text == "" {
			continue
		}
		if c.Kind == "allow" {
			allow = append(allow, "- "+text)
		} else {
			deny = append(deny, "- "+text)
		}
	}
	if len(allow) == 0 && len(deny) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n【操作の制約（最優先。下にある探索や範囲拡大のヒューリスティックより上です。インテントを 1 件生成する前、動作を 1 つ実行する前に、違反していないか必ず自己確認してください。違反するなら実行しません）】：")
	if len(allow) > 0 {
		b.WriteString("\n許可する操作：\n")
		b.WriteString(strings.Join(allow, "\n"))
	}
	if len(deny) > 0 {
		b.WriteString("\n禁止する操作：\n")
		b.WriteString(strings.Join(deny, "\n"))
	}
	b.WriteString("\n（制約の外にある新しい目標、新しいポート、新しいホストを見つけても、認可されたことにはなりません。上記の許可範囲に入らない限り、out-of-scope の事実として記録して飛ばしてください。その対象にインテントを派生させたり、動作を実行したりしません。）")
	return b.String()
}
