package db

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Autumn-27/artex/notify"
)

// このファイルのテストは実際に PostgreSQL へ接続する（DB が無いときはスキップする）。これらの SQL は
// FOR UPDATE SKIP LOCKED、make_interval、JSONB、複数行の IN(...) プレースホルダ連結を使っている。
// いずれも「コンパイルは通るが、実行時に失敗し得る」書き方であり、実走して初めて検証したことになる。

func notifyTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// newTestChannel はチャネルを 1 つ作り、テスト終了時に削除する。
func newTestChannel(t *testing.T, d *DB, kind, mode string, filter string) *NotificationChannel {
	t.Helper()
	if filter == "" {
		filter = `{}`
	}
	ch := &NotificationChannel{
		Name:       "テストチャネル-" + t.Name(),
		Kind:       kind,
		Mode:       mode,
		Config:     json.RawMessage(`{"webhook":"https://example.com/hook"}`),
		Filter:     json.RawMessage(filter),
		RatePerMin: 100,
	}
	id, err := d.SaveNotificationChannel(context.Background(), ch)
	if err != nil {
		t.Fatalf("チャネルの作成に失敗しました: %v", err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_channels WHERE id=$1`, id) })
	ch.ID = id
	return ch
}

// addTestEvent はイベントを 1 件直接書く（finding は経由しない）。振り分けと配信のテストに使う。
func addTestEvent(t *testing.T, d *DB, kind string, findingID int64, snap notify.Snapshot) int64 {
	t.Helper()
	snap.Kind = kind
	snap.FindingID = findingID
	id, err := d.AddNotificationEvent(context.Background(), kind, findingID, snap)
	if err != nil {
		t.Fatalf("イベントの書き込みに失敗しました: %v", err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_events WHERE id=$1`, id) })
	return id
}

func TestNotificationAssetNamesResolvesAndPreservesOrder(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	// 資産は種別ごとに表示の取り方が違う。ドメイン、IP、URL の 3 種である。
	insertAsset := func(query, value string) int64 {
		t.Helper()
		var id int64
		if err := d.QueryRow(query, value).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	domID := insertAsset(`INSERT INTO assets(type, domain) VALUES('subdomain',$1) RETURNING id`, "a.example.com")
	ipID := insertAsset(`INSERT INTO assets(type, ip) VALUES('ip',$1) RETURNING id`, "10.1.2.3")
	svcID := insertAsset(`INSERT INTO assets(type, url) VALUES('service',$1) RETURNING id`, "https://a.example.com/admin")
	t.Cleanup(func() {
		d.Exec(`DELETE FROM assets WHERE id IN ($1,$2,$3)`, domID, ipID, svcID)
	})

	// 渡す順序はわざと崩してあり、存在しない id も 1 つ含む。
	got, err := d.NotificationAssetNames(ctx, []int64{svcID, 999999999, domID, ipID, svcID})
	if err != nil {
		t.Fatalf("資産名の解決に失敗しました: %v", err)
	}
	want := []string{"https://a.example.com/admin", "a.example.com", "10.1.2.3"}
	if len(got) != len(want) {
		t.Fatalf("資産名の件数が一致しません。期待 %v 実際 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("順序または値が一致しません。期待 %v 実際 %v", want, got)
		}
	}
}

// TestRecordNotificationEventTxUnwindsOnFailure はセーブポイントの中心的なテストである。
// トランザクション内で notification_events への書き込みを必ず失敗させる（恒偽の制約を一時的に足す）。
// 断言は次の 2 点。① この関数が false を返す。② トランザクションが aborted にならず、後続の文がまだ実行できる。
//
// セーブポイントが無いと、PostgreSQL はトランザクション全体を無効にする。後続の文はすべて
// "current transaction is aborted" で失敗する。それが「通知テーブルの問題で
// 脆弱性を保存できない」障害経路である。
//
// ここでは意図的に COMMIT ではなく ROLLBACK で終える。PostgreSQL では ALTER TABLE もトランザクションに入る。
// コミットすると一時制約が schema に残り、後続のテストがすべて失敗する。
// ロールバックすれば DDL は自動で取り消され、手作業の掃除は要らない。断言に必要なのは「トランザクションがまだ生きている」ことだけで、
// 実際にコミットする必要はない。
func TestRecordNotificationEventTxUnwindsOnFailure(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	// 防御的な掃除。過去の実行がこの制約を残していたら、先に外す。
	if _, err := d.Exec(`ALTER TABLE notification_events DROP CONSTRAINT IF EXISTS notify_test_never`); err != nil {
		t.Fatal(err)
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck // 一時制約を取り消す。関数コメントを参照

	// NOT VALID なので、これ以降に書く行だけを制約し、既存の履歴イベントは検査しない
	// （既存行が違反していると制約を付けられない）。
	if _, err := tx.ExecContext(ctx, `ALTER TABLE notification_events ADD CONSTRAINT notify_test_never CHECK (false) NOT VALID`); err != nil {
		t.Fatalf("一時制約の追加に失敗しました: %v", err)
	}
	if RecordNotificationEventTx(ctx, tx, notify.EventFindingCreated, 1, notify.Snapshot{Severity: "high"}) {
		t.Fatal("必ず失敗する制約の下で、書き込み成功と報告しました")
	}
	// ここが要点である。トランザクションはまだ使える。
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
		t.Fatalf("トランザクションが汚染されています（セーブポイントが効いていません）: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("ロールバックに失敗しました: %v", err)
	}
	// DDL がロールバックで取り消されたことを確認する。後続のテストへ残さない。
	var exists bool
	if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_constraint WHERE conname='notify_test_never')`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("一時制約がロールバックで取り消されていません。後続のテストを汚染します")
	}
}

func TestFanOutRoutesEventsByFilter(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	all := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	onlyCritical := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{"min_severity":"critical"}`)
	sqlOnly := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{"vulnclass_include":["SQL"]}`)

	highSQL := addTestEvent(t, d, notify.EventFindingCreated, 1001, notify.Snapshot{Severity: "high", VulnClass: "SQLインジェクション"})
	lowXSS := addTestEvent(t, d, notify.EventFindingCreated, 1002, notify.Snapshot{Severity: "low", VulnClass: "XSS"})
	criticalXSS := addTestEvent(t, d, notify.EventFindingCreated, 1003, notify.Snapshot{Severity: "critical", VulnClass: "XSS"})

	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatalf("振り分けに失敗しました: %v", err)
	}

	cases := []struct {
		name    string
		eventID int64
		channel int64
		want    bool
	}{
		{"すべて受け取るチャネルが high を受信", highSQL, all.ID, true},
		{"すべて受け取るチャネルが low を受信", lowXSS, all.ID, true},
		{"深刻のみのチャネルは high をスキップ", highSQL, onlyCritical.ID, false},
		{"深刻のみのチャネルが critical を受信", criticalXSS, onlyCritical.ID, true},
		{"SQL のみのチャネルが SQL を受信", highSQL, sqlOnly.ID, true},
		{"SQL のみのチャネルは XSS をスキップ", lowXSS, sqlOnly.ID, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var exists bool
			if err := d.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM notification_deliveries WHERE event_id=$1 AND channel_id=$2)`,
				tc.eventID, tc.channel).Scan(&exists); err != nil {
				t.Fatal(err)
			}
			if exists != tc.want {
				t.Fatalf("配信の有無: 期待 %v 実際 %v", tc.want, exists)
			}
		})
	}

	// もう一度振り分けても配信は重複しない（fanned_out は冪等）。
	events, deliveries, err := d.FanOutPendingEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if events != 0 || deliveries != 0 {
		t.Fatalf("振り分け済みのイベントを再度処理してはいけません。実際は events=%d deliveries=%d", events, deliveries)
	}
}

// TestFanOutMarksEventsWithNoMatchingChannel は「どのチャネルにも当たらないイベント」を扱う。
// この種のイベントも振り分け済みにしなければならない。残すと未振り分けの集合に留まり、tick のたびに再走査される。
func TestFanOutMarksEventsWithNoMatchingChannel(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	pick := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{"vulnclass_include":["絶対に一致しない種別"]}`)
	_ = pick

	ev := addTestEvent(t, d, notify.EventFindingCreated, 2001, notify.Snapshot{Severity: "high", VulnClass: "XSS"})
	_, deliveries, err := d.FanOutPendingEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if deliveries != 0 {
		t.Fatalf("配信が生じてはいけません。実際は %d", deliveries)
	}
	var fanned bool
	if err := d.QueryRowContext(ctx, `SELECT fanned_out FROM notification_events WHERE id=$1`, ev).Scan(&fanned); err != nil {
		t.Fatal(err)
	}
	if !fanned {
		t.Fatal("チャネルに当たらないイベントも振り分け済みにしないと、無限に再走査されます")
	}
}

func TestClaimRealtimeDeliveriesHonorsLeaseAndMode(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	realtime := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	digest := newTestChannel(t, d, notify.KindDingTalk, NotifyModeDigest, `{}`)

	addTestEvent(t, d, notify.EventFindingCreated, 3001, notify.Snapshot{Severity: "high", VulnClass: "XSS"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}

	// リアルタイムの取得は realtime チャネルの 1 件だけを取り、digest チャネルには触れてはならない。
	got, err := d.ClaimRealtimeDeliveries(ctx, realtime.ID, 10, time.Minute)
	if err != nil {
		t.Fatalf("取得に失敗しました: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("1 件取得するはずです。実際は %d", len(got))
	}
	if got[0].State != NotifyStateSending || got[0].Attempts != 1 {
		t.Fatalf("取得後は sending かつ attempts=1 であるはずです。実際は state=%s attempts=%d", got[0].State, got[0].Attempts)
	}
	// 一緒に読んだ描画コンテキストが揃っていること（チャネル設定、イベントのスナップショット、finding id）。
	if got[0].Channel == nil || len(got[0].Channel.Config) == 0 {
		t.Fatal("取得結果にチャネル設定がありません。描画に失敗します")
	}
	if got[0].FindingID != 3001 {
		t.Fatalf("finding id がイベントから渡されていません。実際は %d", got[0].FindingID)
	}

	// リース期限前の 2 回目の取得は空である。これが「同じ行を 2 つの dispatcher が同時に配信しない」保証である。
	again, err := d.ClaimRealtimeDeliveries(ctx, realtime.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("リース期間中に重複取得してはいけません。実際は %d 件", len(again))
	}

	// digest チャネルの配信を、リアルタイム取得が取ってはならない。
	left, err := d.ClaimRealtimeDeliveries(ctx, digest.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("リアルタイム取得が digest チャネルの配信を取ってはいけません。実際は %d 件", len(left))
	}
}

// TestClaimExpiredLeaseRecovers はクラッシュ後の自己回復を扱う。配信の途中でプロセスが落ちると sending 行が残る。
// リース期限後に再び取得できないと、その配信は永久に止まる。
func TestClaimExpiredLeaseRecovers(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	addTestEvent(t, d, notify.EventFindingCreated, 4001, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	first, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil || len(first) != 1 {
		t.Fatalf("初回の取得に失敗しました: %v (%d 件)", err, len(first))
	}
	// リースを過去へ動かし、「リース期限切れ」を再現する。
	if _, err := d.Exec(`UPDATE notification_deliveries SET next_attempt_at = now() - interval '1 minute' WHERE id=$1`, first[0].ID); err != nil {
		t.Fatal(err)
	}
	second, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 {
		t.Fatalf("リース期限切れの sending 行は再取得できるはずです。実際は %d 件", len(second))
	}
	if second[0].Attempts != 2 {
		t.Fatalf("再取得で試行回数は増えるはずです。実際は %d", second[0].Attempts)
	}
}

func TestClaimSkipsDisabledChannel(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	addTestEvent(t, d, notify.EventFindingCreated, 5001, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	// 無効化すると、残っている未配信もまとめて skipped になる。
	if err := d.SetNotificationChannelEnabled(ctx, ch.ID, false); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := d.QueryRow(`SELECT state FROM notification_deliveries WHERE channel_id=$1`, ch.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStateSkipped {
		t.Fatalf("無効化したチャネルの未配信は skipped になるはずです。実際は %s", state)
	}
	got, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("無効化したチャネルは取得できてはいけません。実際は %d 件", len(got))
	}
}

func TestDigestBatchDueAndStableBatchID(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeDigest, `{}`)
	for i := 0; i < 3; i++ {
		addTestEvent(t, d, notify.EventFindingCreated, int64(6000+i), notify.Snapshot{Severity: "high"})
	}
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}

	// バッチを作った直後で経過時間は 0 なので、30 分周期では期限になってはならない。
	due, err := d.DigestBatchDue(ctx, ch.ID, 30*time.Minute)
	if err != nil {
		t.Fatalf("バッチ期限の判定に失敗しました: %v", err)
	}
	if due {
		t.Fatal("作った直後のバッチがすぐに期限になってはいけません")
	}

	// 3 件の配信の作成時刻をまとめて古くし、周期を満たしたバッチを再現する。
	if _, err := d.Exec(`UPDATE notification_deliveries SET created_at = now() - interval '40 minutes' WHERE channel_id=$1`, ch.ID); err != nil {
		t.Fatal(err)
	}
	due, err = d.DigestBatchDue(ctx, ch.ID, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !due {
		t.Fatal("周期を超えたバッチは期限と判定されるはずです")
	}

	batch, err := d.ClaimDigestBatch(ctx, ch.ID, MaxDigestBatchSize, time.Minute)
	if err != nil {
		t.Fatalf("まとめバッチの取得に失敗しました: %v", err)
	}
	if len(batch) != 3 {
		t.Fatalf("まとめは 3 件を一度に取得するはずです。実際は %d 件", len(batch))
	}
	if batch[0].BatchID == nil {
		t.Fatal("まとめバッチは batch_id を書かなければなりません。無いと、履歴でこれらが同時送信だと分かりません")
	}
	firstBatchID := *batch[0].BatchID
	for _, dl := range batch {
		if dl.BatchID == nil || *dl.BatchID != firstBatchID {
			t.Fatalf("同一バッチは batch_id を共有するはずです。実際は %v と %d", dl.BatchID, firstBatchID)
		}
	}

	// このバッチを**まとめて**失敗させて再スケジュールしたあと再取得しても、batch_id は元の値のままである（COALESCE の役割）。
	// そうでないと、再試行 1 回で「このバッチは一緒に送った」という事実が消える。
	//
	// 1 件だけ再スケジュールしてはならない。配信エンジンがまとめメッセージを送るときも同じ扱いである
	// （1 通がバッチ全体を代表し、成否も一緒）。1 件だけ再スケジュールすると、残りはリース期間内のままで、
	// 再取得はその 1 件だけになる。
	allIDs := make([]int64, 0, len(batch))
	for _, dl := range batch {
		allIDs = append(allIDs, dl.ID)
	}
	if err := d.RescheduleDeliveries(ctx, allIDs, time.Second, "失敗のシミュレート"); err != nil {
		t.Fatal(err)
	}
	// リースを過去へ動かし、バックオフ時間が来た状態を再現する。
	if _, err := d.Exec(`UPDATE notification_deliveries SET next_attempt_at = now() - interval '1 minute' WHERE channel_id=$1`, ch.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := d.ClaimDigestBatch(ctx, ch.ID, MaxDigestBatchSize, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(reclaimed) != 3 {
		t.Fatalf("再取得は 3 件すべてを取るはずです。実際は %d", len(reclaimed))
	}
	if reclaimed[0].BatchID == nil || *reclaimed[0].BatchID != firstBatchID {
		t.Fatalf("再試行後も batch_id は元の値 %d のままであるはずです。実際は %v", firstBatchID, reclaimed[0].BatchID)
	}
}

func TestDeliveryStateTransitions(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	addTestEvent(t, d, notify.EventFindingCreated, 7001, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	got, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil || len(got) != 1 {
		t.Fatalf("取得に失敗しました: %v (%d)", err, len(got))
	}
	id := got[0].ID

	if err := d.RescheduleDeliveries(ctx, []int64{id}, time.Second, "ネットワークの揺らぎ"); err != nil {
		t.Fatal(err)
	}
	var state, lastErr string
	if err := d.QueryRow(`SELECT state, last_error FROM notification_deliveries WHERE id=$1`, id).Scan(&state, &lastErr); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStatePending || lastErr != "ネットワークの揺らぎ" {
		t.Fatalf("再スケジュール後は pending になり、理由が記録されるはずです。実際は state=%s err=%q", state, lastErr)
	}

	if err := d.FailDeliveries(ctx, []int64{id}, "再試行を使い切りました"); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT state FROM notification_deliveries WHERE id=$1`, id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStateFailed {
		t.Fatalf("failed であるはずです。実際は %s", state)
	}

	// 手動再送は再試行回数を 0 に戻し、すぐに期限切れにする。古い失敗予算を引き継いではならない。
	if err := d.RetryNotificationDelivery(ctx, id); err != nil {
		t.Fatalf("再送に失敗しました: %v", err)
	}
	var attempts int
	var next time.Time
	if err := d.QueryRow(`SELECT state, attempts, next_attempt_at FROM notification_deliveries WHERE id=$1`, id).Scan(&state, &attempts, &next); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStatePending || attempts != 0 {
		t.Fatalf("再送後は pending かつ attempts=0 であるはずです。実際は state=%s attempts=%d", state, attempts)
	}
	if next.After(time.Now().Add(time.Second)) {
		t.Fatal("再送はすぐに取得できるはずです")
	}

	// 配信済みは再送できてはならない。
	if err := d.MarkDeliveriesSent(ctx, []int64{id}); err != nil {
		t.Fatal(err)
	}
	if err := d.RetryNotificationDelivery(ctx, id); err == nil {
		t.Fatal("配信済みを再送できてはいけません")
	}
}

func TestListNotificationDeliveriesPagingAndFilter(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	for i := 0; i < 5; i++ {
		addTestEvent(t, d, notify.EventFindingCreated, int64(8000+i), notify.Snapshot{Severity: "high", Name: "ページングテスト"})
	}
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute); err != nil {
		t.Fatal(err)
	}

	page1, total, err := d.ListNotificationDeliveries(ctx, NotificationDeliveryFilter{ChannelID: ch.ID, State: NotifyStateSending}, 1, 2)
	if err != nil {
		t.Fatalf("照会に失敗しました: %v", err)
	}
	if total != 5 {
		t.Fatalf("総数は 5 であるはずです。実際は %d", total)
	}
	if len(page1) != 2 {
		t.Fatalf("1 ページ 2 件であるはずです。実際は %d", len(page1))
	}
	// 新しいものが先。1 ページ目の先頭 id は 2 ページ目の先頭より大きい。
	page2, _, err := d.ListNotificationDeliveries(ctx, NotificationDeliveryFilter{ChannelID: ch.ID, State: NotifyStateSending}, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 2 || page2[0].ID >= page1[0].ID {
		t.Fatalf("ページは新しいものが先であるはずです。実際は page1[0]=%d page2[0]=%d", page1[0].ID, page2[0].ID)
	}
	// 描画コンテキストは履歴と一緒に返る。無いと一覧で「何を送ったか」を出せない。
	if page1[0].ChannelName == "" || page1[0].FindingID == 0 {
		t.Fatalf("履歴項目に表示用フィールドがありません: %+v", page1[0])
	}

	// 状態で絞る。pending は無い。
	pending, totalPending, err := d.ListNotificationDeliveries(ctx, NotificationDeliveryFilter{ChannelID: ch.ID, State: NotifyStatePending}, 1, 50)
	if err != nil {
		t.Fatal(err)
	}
	if totalPending != 0 || len(pending) != 0 {
		t.Fatalf("pending の配信があってはいけません。実際は %d 件 (total=%d)", len(pending), totalPending)
	}
}

func TestSetFindingStatusWithNotifyOnlyEmitsOnRealChange(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	tk, err := d.CreateTask("通知の状態変更テスト", "目標", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(tk.ID)
	es := d.Exploration(tk.ExplorationID)
	f, err := es.RecordFinding(ctx, RecordFindingInput{
		TaskID: tk.ID, Worker: "test", VulnClass: "SQLインジェクション", Name: "状態変更のケース",
		Severity: "high", Summary: "要約",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_events WHERE finding_id=$1`, f.FindingID) })

	// 保存時に finding_created イベントが 1 件登録される。まずそれを数えて基準にする。
	var base int
	if err := d.QueryRow(`SELECT count(*) FROM notification_events WHERE finding_id=$1`, f.FindingID).Scan(&base); err != nil {
		t.Fatal(err)
	}
	if base < 1 {
		t.Fatal("脆弱性の保存は、同じトランザクションで通知イベントを 1 件登録するはずです")
	}

	// 同じ状態への変更ではイベントを出してはならない（再送で通知がうるさくなるのを避ける）。
	from, found, notified, err := d.SetFindingStatusWithNotify(ctx, f.FindingID, "pending")
	if err != nil || !found {
		t.Fatalf("状態の設定に失敗しました: found=%v err=%v", found, err)
	}
	if notified {
		t.Fatal("状態が変わっていないときは通知イベントを登録してはいけません")
	}
	if from != "pending" {
		t.Fatalf("変更前の状態 pending を返すはずです。実際は %q", from)
	}

	// 実際に変わったときは、イベントを登録し from と to を記録する。
	from, found, notified, err = d.SetFindingStatusWithNotify(ctx, f.FindingID, "fixed")
	if err != nil || !found {
		t.Fatalf("状態の設定に失敗しました: found=%v err=%v", found, err)
	}
	if !notified {
		t.Fatal("状態が実際に変わったときは通知イベントを登録するはずです")
	}
	if from != "pending" {
		t.Fatalf("from は pending であるはずです。実際は %q", from)
	}
	var snapshot []byte
	if err := d.QueryRow(`SELECT snapshot FROM notification_events WHERE finding_id=$1 AND kind=$2`,
		f.FindingID, notify.EventFindingStatusChanged).Scan(&snapshot); err != nil {
		t.Fatalf("状態変更イベントが見つかりません: %v", err)
	}
	var snap notify.Snapshot
	if err := json.Unmarshal(snapshot, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.FromStatus != "pending" || snap.ToStatus != "fixed" {
		t.Fatalf("スナップショットの状態遷移が違います: %s → %s", snap.FromStatus, snap.ToStatus)
	}
	// スナップショットには描画に必要なフィールドが要る。無いと状態変更メッセージが空になる。
	if snap.VulnClass != "SQLインジェクション" || snap.Severity != "high" || snap.Name != "状態変更のケース" {
		t.Fatalf("スナップショットに描画用フィールドがありません: %+v", snap)
	}
	var status string
	if err := d.QueryRow(`SELECT status FROM findings WHERE id=$1`, f.FindingID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "fixed" {
		t.Fatalf("状態は fixed に更新されているはずです。実際は %s", status)
	}

	// 存在しない脆弱性は found=false で、エラーにはしない。
	if _, found, _, err := d.SetFindingStatusWithNotify(ctx, 999999999, "fixed"); err != nil || found {
		t.Fatalf("存在しない脆弱性は found=false かつエラー無しであるはずです。実際は found=%v err=%v", found, err)
	}
}

func TestNotificationStatsSnapshot(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	addTestEvent(t, d, notify.EventFindingCreated, 9001, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	stats, err := d.NotificationStatsSnapshot(ctx)
	if err != nil {
		t.Fatalf("集計に失敗しました: %v", err)
	}
	if stats.Channels < 1 || stats.ChannelsOn < 1 {
		t.Fatalf("チャネル件数が違います: %+v", stats)
	}
	if stats.Pending < 1 {
		t.Fatalf("未配信が集計されるはずです: %+v", stats)
	}
	// 作った直後の配信の滞留時間は 0 に近いはずで、負や巨大な値ではない。
	if stats.BacklogAgeMS < 0 || stats.BacklogAgeMS > int64(time.Hour/time.Millisecond) {
		t.Fatalf("滞留時間が不正です: %d ms", stats.BacklogAgeMS)
	}
	_ = ch
}

func TestNotificationChannelCRUDRoundTrip(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	ch := &NotificationChannel{
		Name:       "CRUD 往復",
		Kind:       notify.KindEmail,
		Mode:       NotifyModeDigest,
		Config:     json.RawMessage(`{"host":"smtp.example.com","port":587,"from":"a@b.c","to":["x@y.z"]}`),
		Filter:     json.RawMessage(`{"min_severity":"medium","on_status_change":true}`),
		RatePerMin: 42,
	}
	id, err := d.SaveNotificationChannel(ctx, ch)
	if err != nil {
		t.Fatalf("新規作成に失敗しました: %v", err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_channels WHERE id=$1`, id) })

	got, err := d.NotificationChannelByID(ctx, id)
	if err != nil {
		t.Fatalf("読み取りに失敗しました: %v", err)
	}
	if got.Mode != NotifyModeDigest || got.RatePerMin != 42 || got.Name != "CRUD 往復" {
		t.Fatalf("往復したフィールドが一致しません: %+v", got)
	}
	if !got.IsEnabled() {
		t.Fatal("既定は有効であるはずです")
	}
	var cfg map[string]any
	if err := json.Unmarshal(got.Config, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["host"] != "smtp.example.com" {
		t.Fatalf("設定が正しく保存されていません: %v", cfg)
	}
	var filter notify.Filter
	if err := json.Unmarshal(got.Filter, &filter); err != nil {
		t.Fatal(err)
	}
	if filter.MinSeverity != "medium" || !filter.OnStatusChange {
		t.Fatalf("フィルタ条件が正しく保存されていません: %+v", filter)
	}

	// 更新してから読み直す。
	got.Name = "名前を変えた"
	off := false
	got.Enabled = &off
	if _, err := d.SaveNotificationChannel(ctx, got); err != nil {
		t.Fatal(err)
	}
	after, err := d.NotificationChannelByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Name != "名前を変えた" || after.IsEnabled() {
		t.Fatalf("更新が反映されていません: %+v", after)
	}

	// 削除後は「存在しない」になり、黙って成功してはならない。
	if err := d.DeleteNotificationChannel(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := d.NotificationChannelByID(ctx, id); err != ErrNotificationChannelNotFound {
		t.Fatalf("期待は ErrNotificationChannelNotFound、実際は %v", err)
	}
	if err := d.DeleteNotificationChannel(ctx, id); err != ErrNotificationChannelNotFound {
		t.Fatalf("再削除は存在しない旨になるはずです。実際は %v", err)
	}
}

// TestSaveNotificationChannelKeepsExplicitZeroRate は、かつて書き間違えた箇所を固定する。
// **0 は正当な設定で、意味は「レート制限なし」である。db 層が「未指定」と見なして既定値で上書きしてはならない**。
//
// 過去のバグ: SaveNotificationChannel が `if RatePerMin <= 0 { 既定値を取る }` と書いていた。
// そのため文書、UI の説明、takeTokens はいずれも「0 はレート制限なし」と解釈するのに、書き込みだけが黙って
// 20（DingTalk、企業 WeChat、Telegram）または 100（Feishu）へ変えていた。操作者は制限を外したつもりでも実際は止まっており、
// その旨の表示も無い。「未指定」と「明示的な 0」の違いはリクエスト本体でしか表せない。
// だから既定値は server 層で埋める（notifyCreateChannel を参照）。db 層は保存するだけである。
func TestSaveNotificationChannelKeepsExplicitZeroRate(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	// 明示的な 0（レート制限なし）は、そのまま保存しなければならない。
	unlimited := &NotificationChannel{
		Name: "レート制限なし", Kind: notify.KindDingTalk, RatePerMin: 0,
		Config: json.RawMessage(`{"webhook":"https://example.com/h"}`),
	}
	id, err := d.SaveNotificationChannel(ctx, unlimited)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_channels WHERE id=$1`, id) })
	got, err := d.NotificationChannelByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.RatePerMin != 0 {
		t.Fatalf("明示的な 0 はレート制限なしを意味し、そのまま保存しなければなりません。実際は %d", got.RatePerMin)
	}
	if got.Mode != NotifyModeRealtime {
		t.Fatalf("既定モードは realtime であるはずです。実際は %s", got.Mode)
	}

	// 負の値は不正な入力である。別の値へ黙って変えるのではなく、拒否しなければならない。
	bad := &NotificationChannel{
		Name: "負のレート制限", Kind: notify.KindDingTalk, RatePerMin: -1,
		Config: json.RawMessage(`{"webhook":"https://example.com/h"}`),
	}
	if _, err := d.SaveNotificationChannel(ctx, bad); err == nil {
		t.Fatal("負のレート制限は拒否されるはずです")
	}
}

// TestDeleteChannelCascadesDeliveries は外部キーの挙動を固定する。チャネル削除後、その配信履歴も一緒に消える
// （設定が無くなれば、履歴は解釈できない）。イベント自体は残す。別のチャネルがまだ参照していることがある。
func TestDeleteChannelCascadesDeliveries(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	ev := addTestEvent(t, d, notify.EventFindingCreated, 9101, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := d.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, ch.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before == 0 {
		t.Fatal("前提が成り立ちません。配信が作られていません")
	}
	if err := d.DeleteNotificationChannel(ctx, ch.ID); err != nil {
		t.Fatal(err)
	}
	var after int
	if err := d.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, ch.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != 0 {
		t.Fatalf("チャネル削除後、その配信はカスケード削除されるはずです。まだ %d 件あります", after)
	}
	var evExists bool
	if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM notification_events WHERE id=$1)`, ev).Scan(&evExists); err != nil {
		t.Fatal(err)
	}
	if !evExists {
		t.Fatal("チャネルを削除しても、イベント自体を一緒に削除してはいけません")
	}
}

// TestClaimDigestBatchHonorsCallerLimit は、監査が指摘した抜けを扱う。
// まとめチャネルは以前、トークンバケットを完全に迂回していた。allow は takeTokens が差し引くのに誰も使わず、
// rate_per_min は digest モードに全く効かなかった。今は limit も制約に入る。
func TestClaimDigestBatchHonorsCallerLimit(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeDigest, `{}`)
	for i := 0; i < 10; i++ {
		addTestEvent(t, d, notify.EventFindingCreated, int64(7000+i), notify.Snapshot{Severity: "high"})
	}
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	// limit=3 だと 3 件しか取れず、残りは DB に残る。
	got, err := d.ClaimDigestBatch(ctx, ch.ID, 3, time.Minute)
	if err != nil {
		t.Fatalf("取得に失敗しました: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("呼び出し側のレート制限枠どおり 3 件だけ取得するはずです。実際は %d", len(got))
	}
	// limit=0 は今ラウンドの枠を使い切ったことを示す。1 件も取らず、エラーにもしてはならない。
	if got, err := d.ClaimDigestBatch(ctx, ch.ID, 0, time.Minute); err != nil || len(got) != 0 {
		t.Fatalf("枠が 0 のときは 0 件で、エラーにもしてはいけません。実際は %d 件 err=%v", len(got), err)
	}
}

// TestFinishFindingRetestEmitsStatusChange は、監査が指摘した完全性の欠けを扱う。
// 再テストの結論が「修正済み」のとき、状態は確かに変わる。しかしその UPDATE は直接 DB へ書いており、
// 通知付きの版を迂回していた。そのため on_status_change を設定したチャネルは、この状態遷移の通知を
// 全く受け取れない。画面上の状態は静かに変わり、運用者はプラットフォームを開くまで気付けない。
//
// このテストは「状態を変える経路はすべて、状態変更イベントを登録する」ことを固定する。
func TestFinishFindingRetestEmitsStatusChange(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	tk, err := d.CreateTask("再テスト通知テスト", "目標", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(tk.ID)
	es := d.Exploration(tk.ExplorationID)
	f, err := es.RecordFinding(ctx, RecordFindingInput{
		TaskID: tk.ID, Worker: "test", VulnClass: "SQLインジェクション", Name: "再テスト対象",
		Severity: "high", Summary: "要約",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_events WHERE finding_id=$1`, f.FindingID) })

	// 再テスト記録を 1 件作り、そのまま完了状態まで進める。
	rt, _, _, err := d.CreateFindingRetest(ctx, f.FindingID, "再確認")
	if err != nil {
		t.Fatal(err)
	}
	if rt.ConversationID == nil {
		t.Fatal("再テストはセッションを 1 つ関連付けるはずです")
	}
	// 再テストは先に running へ入らないと結論を書けない（実際の流れと同じ）。
	if ok, err := d.StartFindingRetest(ctx, rt.ID); err != nil || !ok {
		t.Fatalf("再テストの開始に失敗しました: ok=%v err=%v", ok, err)
	}
	if err := d.RecordFindingRetestResult(ctx, *rt.ConversationID, "fixed", "修正済み", "証拠"); err != nil {
		t.Fatal(err)
	}
	if err := d.FinishFindingRetest(rt.ID, "completed", ""); err != nil {
		t.Fatalf("再テストの終了に失敗しました: %v", err)
	}

	var status string
	if err := d.QueryRow(`SELECT status FROM findings WHERE id=$1`, f.FindingID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != FindingFixed {
		t.Fatalf("再テストが修正済みと判定したあとの状態は fixed であるはずです。実際は %s", status)
	}

	// 要点は、状態変更イベントが 1 件あり、from と to が正しいことである。
	var snapshot []byte
	err = d.QueryRow(`SELECT snapshot FROM notification_events WHERE finding_id=$1 AND kind=$2 ORDER BY id DESC LIMIT 1`,
		f.FindingID, notify.EventFindingStatusChanged).Scan(&snapshot)
	if err != nil {
		t.Fatalf("再テストが修正済みと判定したら状態変更の通知イベントを登録するはずです（無いと on_status_change のチャネルは受け取れません）: %v", err)
	}
	var snap notify.Snapshot
	if err := json.Unmarshal(snapshot, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.FromStatus != "pending" || snap.ToStatus != FindingFixed {
		t.Fatalf("スナップショットの状態遷移が違います: %s → %s", snap.FromStatus, snap.ToStatus)
	}
	// スナップショットには描画に必要なフィールドが要る。無いと通知が空になる。
	if snap.Name != "再テスト対象" || snap.Severity != "high" {
		t.Fatalf("スナップショットに描画用フィールドがありません: %+v", snap)
	}
}
