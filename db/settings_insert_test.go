package db

import (
	"fmt"
	"testing"
	"time"
)

// InsertSettingIfAbsent は auth.password_hash の歯止めである。呼び出し側の GetSetting 検査は、
// データベースのエラーで効かなくなることがあり、並行リクエストに割り込まれることもある（bcrypt は数十ミリ秒かかる）。
// 「初回だけ設定できる」保証は主キー制約に置き、上の if 判定には置かない。
func TestInsertSettingIfAbsentDoesNotOverwrite(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer d.Close()

	// テスト専用の key を使う。開発庫の本物の auth.password_hash には触れない。
	key := fmt.Sprintf("test.insert_if_absent.%d", time.Now().UnixNano())
	defer func() { _, _ = d.Exec(`DELETE FROM settings WHERE key=$1`, key) }()

	inserted, err := d.InsertSettingIfAbsent(key, "first")
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatal("初回の書き込みは inserted=true を返す必要があります")
	}

	inserted, err = d.InsertSettingIfAbsent(key, "second")
	if err != nil {
		t.Fatal(err)
	}
	if inserted {
		t.Fatal("key が既にあるときは inserted=false を返す必要があります")
	}

	got, ok, err := d.GetSetting(key)
	if err != nil || !ok {
		t.Fatalf("GetSetting: ok=%v err=%v", ok, err)
	}
	if got != "first" {
		t.Fatalf("値が %q に上書きされました。%q のままである必要があります", got, "first")
	}
}
