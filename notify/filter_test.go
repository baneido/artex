package notify

import "testing"

func TestParseFilterMalformedFallsBackToMatchAll(t *testing.T) {
	// 壊れた JSON、空入力、型の違うフィールドは、すべてゼロ値の Filter、
	// つまり「フィルタしない」に退化する。この不変条件は「漏らすより多く送る」の着地点だ。
	// ここをエラーや部分解析に変えると、ユーザーが 1 文字間違えただけで、重大な通知をすべて黙って落とす。
	cases := []struct {
		name string
		raw  string
	}{
		{"空入力", ""},
		{"不正な JSON", `{not json`},
		{"途中で切れた JSON", `{"min_severity":`},
		{"型が一致しない", `{"min_severity": 123, "task_ids": "abc"}`},
		{"最上位が配列", `[1,2,3]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := ParseFilter([]byte(tc.raw))
			if f.MinSeverity != "" || len(f.TaskIDs) != 0 || len(f.AssetIDs) != 0 {
				t.Fatalf("壊れた設定はゼロ値の Filter に退化する必要があります。実際は %+v", f)
			}
			// ゼロ値の Filter は任意のイベントに一致する。
			ev := Snapshot{Kind: EventFindingCreated, Severity: "low", VulnClass: "XSS"}
			if !Match(f, ev) {
				t.Fatal("ゼロ値の Filter はすべてのイベントに一致する必要があります")
			}
		})
	}
}

func TestMatchSeverityThreshold(t *testing.T) {
	ev := func(sev string) Snapshot {
		return Snapshot{Kind: EventFindingCreated, Severity: sev}
	}
	cases := []struct {
		min    string
		sev    string
		expect bool
	}{
		{"", "low", true},
		{"", "critical", true},
		{"high", "critical", true},
		{"high", "high", true},
		{"high", "medium", false},
		{"high", "low", false},
		{"critical", "high", false},
		{"critical", "critical", true},
		// 未知の深刻度の序数は 0 で、空でない下限にはすべて止められる（疑わしいときは送らない）。
		{"low", "", false},
		{"low", "unknown", false},
		{"", "", true},
	}
	for _, tc := range cases {
		got := Match(Filter{MinSeverity: tc.min}, ev(tc.sev))
		if got != tc.expect {
			t.Errorf("min=%q sev=%q: 期待 %v 実際 %v", tc.min, tc.sev, tc.expect, got)
		}
	}
}

func TestMatchScopeRestrictions(t *testing.T) {
	ev := Snapshot{
		Kind:      EventFindingCreated,
		Severity:  "high",
		TaskID:    7,
		AssetIDs:  []int64{10, 20},
		VulnClass: "SQLインジェクション",
	}
	cases := []struct {
		name   string
		filter Filter
		expect bool
	}{
		{"空の範囲は制限なし", Filter{}, true},
		{"タスクが一致", Filter{TaskIDs: []int64{7}}, true},
		{"タスクが不一致", Filter{TaskIDs: []int64{8}}, false},
		{"タスクの複数選択に一致を含む", Filter{TaskIDs: []int64{8, 7}}, true},
		{"資産に交差がある", Filter{AssetIDs: []int64{20, 99}}, true},
		{"資産に交差がない", Filter{AssetIDs: []int64{99}}, false},
		{"タスクと資産が同時に一致", Filter{TaskIDs: []int64{7}, AssetIDs: []int64{10}}, true},
		{"タスクは一致し資産は不一致", Filter{TaskIDs: []int64{7}, AssetIDs: []int64{99}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(tc.filter, ev); got != tc.expect {
				t.Errorf("期待 %v 実際 %v", tc.expect, got)
			}
		})
	}
}

func TestMatchVulnClassKeywords(t *testing.T) {
	ev := func(class string) Snapshot {
		return Snapshot{Kind: EventFindingCreated, Severity: "high", VulnClass: class}
	}
	cases := []struct {
		name   string
		filter Filter
		class  string
		expect bool
	}{
		{"include が空ならすべて受ける", Filter{}, "任意の種別", true},
		{"include が一致", Filter{VulnClassInclude: []string{"SQL"}}, "SQLインジェクション", true},
		{"include が不一致", Filter{VulnClassInclude: []string{"コマンド実行"}}, "SQLインジェクション", false},
		{"include の複数語のどれかが一致", Filter{VulnClassInclude: []string{"コマンド実行", "SQL"}}, "SQLインジェクション", true},
		{"大文字小文字を区別しない", Filter{VulnClassInclude: []string{"sql"}}, "SQLインジェクション", true},
		{"exclude が一致したら除外", Filter{VulnClassExclude: []string{"情報漏えい"}}, "情報漏えい", false},
		{"exclude が不一致なら通す", Filter{VulnClassExclude: []string{"情報漏えい"}}, "SQLインジェクション", true},
		// 除外は包含より優先する。同時に一致したときは外れる。
		{"除外は包含より優先", Filter{
			VulnClassInclude: []string{"SQL"},
			VulnClassExclude: []string{"インジェクション"},
		}, "SQLインジェクション", false},
		// 空白だけのキーワードは無視する。無視しないと「空白を含む文字列すべてに一致」へ退化する。
		{"空白のキーワードは無視する", Filter{VulnClassInclude: []string{"", "  "}}, "SQLインジェクション", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(tc.filter, ev(tc.class)); got != tc.expect {
				t.Errorf("期待 %v 実際 %v", tc.expect, got)
			}
		})
	}
}

func TestMatchStatusChangeRequiresOptIn(t *testing.T) {
	ev := Snapshot{Kind: EventFindingStatusChanged, Severity: "critical", FromStatus: "pending", ToStatus: "fixed"}
	// 既定はオフだ。ほとんどの人が言う「脆弱性を通知する」は新しい脆弱性の検出であり、状態の流水ではない。
	if Match(Filter{MinSeverity: "low"}, ev) {
		t.Fatal("状態変更イベントは、有効にしていないときは飛ばす必要があります")
	}
	if !Match(Filter{OnStatusChange: true}, ev) {
		t.Fatal("on_status_change を有効にしたあと、状態変更イベントは一致する必要があります")
	}
	// 作成イベントは on_status_change に依存しない。
	created := Snapshot{Kind: EventFindingCreated, Severity: "critical"}
	if !Match(Filter{MinSeverity: "low"}, created) {
		t.Fatal("作成イベントは on_status_change に依存してはいけません")
	}
}
