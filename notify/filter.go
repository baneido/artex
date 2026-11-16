package notify

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Filter は notification_channels.filter という JSONB 列の契約で、チャネルインスタンスのフィルタ条件です。
// どのフィールドも任意です。欠けると「フィルタしない」になります。壊れた設定の受け方は ParseFilter を参照してください。
type Filter struct {
	// MinSeverity は最低の深刻度（low/medium/high/critical）です。空は下限なしです。
	MinSeverity string `json:"min_severity"`
	// TaskIDs / AssetIDs が空配列なら制限なしです。空でなければ、イベントと交差することを要求します。
	TaskIDs  []int64 `json:"task_ids"`
	AssetIDs []int64 `json:"asset_ids"`
	// VulnClassInclude が空ならすべて受けます。空でなければ、vulnclass がいずれかのキーワードに一致することを要求します。
	// VulnClassExclude は、いずれかのキーワードに一致したら除外します（除外は包含より優先します）。
	// 照合は大文字小文字を区別しない部分文字列です。正規表現より安全です。正規表現の設定ミスでチャネルが黙って無効になりません。
	VulnClassInclude []string `json:"vulnclass_include"`
	VulnClassExclude []string `json:"vulnclass_exclude"`
	// OnStatusChange は、このチャネルが脆弱性の状態変更イベントを受けるかを決めます（realtime モードだけで意味があります）。
	OnStatusChange bool `json:"on_status_change"`
}

// ParseFilter はチャネルのフィルタ設定を解析します。
//
// error は返しません。これは意図した設計です。フィルタ条件が壊れているときは、常にゼロ値の
// Filter（フィルタしない、すべて一致）に退きます。脆弱性の通知では、1 件多く送る方が、
// 重大な 1 件を黙って落とすよりよいからです。解析失敗を「送らない」にすると、設定できたように見えて
// 実際は何も送らないチャネルになります。これが最悪の失敗です。
func ParseFilter(raw []byte) Filter {
	var f Filter
	if len(raw) == 0 {
		return f
	}
	// 解析に失敗したとき f はゼロ値のままで、フィルタしません。
	_ = json.Unmarshal(raw, &f)
	return f
}

// ValidMinSeverity は、s が正当な深刻度の下限か（空文字は下限なし）を報告します。
func ValidMinSeverity(s string) bool {
	if s == "" {
		return true
	}
	_, ok := severityRank[s]
	return ok
}

// Validate は、フィルタ設定のうち値が制限されているフィールドを検証します。チャネル保存時に呼びます。
//
// 書き込み時に止める理由: Match は未知の下限を `rank >= 0` と判定し、常に真です。
// つまり min_severity を 1 字間違える（"hgih"）と、フィルタは黙って無効になり
// 「全部送る」になります。このパッケージの「漏らすより多く送る」と同じ方向です（漏れは起きません）。
// ただし、深刻度で分けて送っているつもりが、全部の脆弱性をグループへ流し込み、
// 設定ミスを示す兆候がありません。この「黙った降格」は入口で止めます。
//
// Validate は書き込み経路だけに使います。読み取りは ParseFilter の寛容な意味のままです。
// 過去データに既にある壊れた値で、チャネル全体が読めなくならないようにします。
func (f Filter) Validate() error {
	if !ValidMinSeverity(f.MinSeverity) {
		return fmt.Errorf("最低の深刻度 %q は無効です。指定できる値は low / medium / high / critical で、空欄は制限なしです", f.MinSeverity)
	}
	return nil
}

// Match は、このフィルタ条件のチャネルへイベントを配信するかを判定します。
//
// error は返しません。理由は ParseFilter と同じで、内部の異常は「一致」として扱います。
// 判定順は、イベント種別、深刻度の下限、タスクと資産の範囲、脆弱性種別のキーワードです。
func Match(f Filter, s Snapshot) bool {
	// 状態変更イベントは、明示的に有効にしたチャネルだけが受けます。既定はオフです。ほとんどの利用者は
	// 「通知」を「新しい脆弱性の検出」の意味で使い、状態遷移を逐一追う使い方はしません。
	if s.Kind == EventFindingStatusChanged && !f.OnStatusChange {
		return false
	}
	if !AtLeast(s.Severity, f.MinSeverity) {
		return false
	}
	if len(f.TaskIDs) > 0 && !slices.Contains(f.TaskIDs, s.TaskID) {
		return false
	}
	if len(f.AssetIDs) > 0 && !intersectsInt(f.AssetIDs, s.AssetIDs) {
		return false
	}
	// 除外が優先です。除外キーワードのどれかに一致したら外れます。包含リストにも同時に一致していても外れます。
	if len(f.VulnClassExclude) > 0 && containsAnyFold(s.VulnClass, f.VulnClassExclude) {
		return false
	}
	if len(f.VulnClassInclude) > 0 && !containsAnyFold(s.VulnClass, f.VulnClassInclude) {
		return false
	}
	return true
}

func intersectsInt(a, b []int64) bool {
	// 小さい集合は線形走査で足ります。どちらも「人が選んだ数十件」の規模で、
	// map を作るコストの方が大きいです。
	for _, v := range b {
		if slices.Contains(a, v) {
			return true
		}
	}
	return false
}

// containsAnyFold は、s が keywords のいずれかを含むか（大文字小文字を区別しない）を報告します。
func containsAnyFold(s string, keywords []string) bool {
	lower := strings.ToLower(s)
	for _, kw := range keywords {
		kw = strings.ToLower(strings.TrimSpace(kw))
		if kw != "" && strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}
