package db

import "testing"

// TestRenameBuiltinRulesMigratesOldLabels checks both the in-place rename of rows
// seeded with the former Chinese labels and that a second run is a no-op.
func TestRenameBuiltinRulesMigratesOldLabels(t *testing.T) {
	d := notifyTestDB(t)
	t.Cleanup(func() {
		d.Exec(`DELETE FROM intercept_rules WHERE name = '[組み込み] Fork ボム' AND pattern = 'x'`)
	})
	if _, err := d.Exec(`INSERT INTO intercept_rules(name, priority, match_target, match_type, pattern, action) VALUES ('[内置] Fork 炸弹', 1, 'tool_input', 'regex', 'x', 'deny')`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := d.renameBuiltinRules(); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM intercept_rules WHERE name = '[組み込み] Fork ボム'`).Scan(&n); err != nil || n < 1 {
		t.Fatalf("renamed rule missing: n=%d err=%v", n, err)
	}
	if err := d.QueryRow(`SELECT count(*) FROM intercept_rules WHERE name LIKE '[内置]%'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("old labels remain: n=%d err=%v", n, err)
	}
}
