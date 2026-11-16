package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/notify"
)

// 本ファイルは通知機能のエンドツーエンドの挙動を覆う。脆弱性の DB 登録、イベント、分配、実際の HTTP 送信。
//
// 安全上の注意。これらの用例は全体の Notifier.step() を呼ばない。自分で作った
// チャネルに対してだけ stepRealtime/stepDigest を呼ぶ。step() は DB 内の有効なチャネルを全部たどる。
// 本物の DingTalk や WeCom のボットを設定した開発用 DB でテストすると、全体の step はテスト中に
// 生まれた脆弱性を、それらのグループへ本当に送る。チャネル単位の呼び出しで、影響はテストが作った仮の受信側に限る。
//
// 後始末。用例の終わりに、この用例が作ったイベント（配信は連鎖削除）とチャネルを消し、本物のチャネルに滞留を残さない。
//
// アサーションの見方。stepRealtime/stepDigest は値を返さず、内部でログを残す。ここで見るのは
// 観測できる外の挙動（仮の受信側が何を受け取ったか、配信行がどの状態になったか）であり、関数の
// 戻り値ではない。戻り値へのスタブより、実際の呼び出し経路に近い。

// notifyFixture はこのファイルの用例の共通装置である。
type notifyFixture struct {
	s       *Server
	pg      *db.DB
	request func(method, path, body string) *httptest.ResponseRecorder
	n       *Notifier
	// 自前の task/exploration。用例は脆弱性をここへ記録し、他の用例のデータと分ける。
	taskID int64
	expID  int64
	// cleanupMark より後に生まれたイベントは、後始末でまとめて消す。
	cleanupMark int64
}

func newNotifyFixture(t *testing.T) *notifyFixture {
	t.Helper()
	// このファイルの仮の受信側はすべて 127.0.0.1 で動く。配信はデフォルトでループバックアドレスを拒む
	// （同じマシンのサービスやクラウドのメタデータへの SSRF を防ぐ）。テストはこのスイッチを明示的に開く。
	// 「デフォルトで拒む」挙動は notify パッケージの ssrf_test.go が覆う。
	t.Setenv(notify.AllowLocalTargetsEnv, "1")
	s, _, request := trafficEvidenceServer(t)
	pg := s.m.pg

	// task を自前で作る。共有装置 trafficEvidenceServer が作る task からは exploration id が取れず、
	// 脆弱性の記録にはそれが要る。
	task, err := s.m.CreateTask("通知配信テスト", "配信の動作確認", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := strconv.ParseInt(task.ID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.Exec(`DELETE FROM tasks WHERE id=$1`, taskID) })

	var mark int64
	if err := pg.QueryRow(`SELECT COALESCE(max(id),0) FROM notification_events`).Scan(&mark); err != nil {
		t.Fatal(err)
	}
	// 装置を自分で閉じる。fixture を作る前からあるイベントを、一度に分配済みにする。
	//
	// 必要な理由。FanOutPendingEvents は全体に効き、DB 内の未分配イベントをすべて
	// 一致するチャネルへ展開する。共有装置 trafficEvidenceServer 自身も脆弱性を 1 件記録する
	// （それが返す最初の finding である）。他の用例の残りもある。分けないと、
	// これらの余分なイベントがこの用例のチャネルへ分配され、「配信は N 件あるべき」というアサーションが
	// 当たったり外れたりする。外れ方は用例の実行順に依存し、すぐ失敗するより調べにくい。
	if _, err := pg.Exec(`UPDATE notification_events SET fanned_out = true WHERE id <= $1 AND NOT fanned_out`, mark); err != nil {
		t.Fatal(err)
	}

	f := &notifyFixture{s: s, pg: pg, request: request, n: newNotifier(s), taskID: taskID, expID: task.ExpID, cleanupMark: mark}
	t.Cleanup(func() {
		if _, err := pg.Exec(`DELETE FROM notification_events WHERE id > $1`, f.cleanupMark); err != nil {
			t.Logf("通知イベントの削除に失敗しました: %v", err)
		}
	})
	// 全体スイッチはオンにする（他の用例がオフにした可能性がある）。
	if err := pg.SetBool(settingNotifyEnabled, true); err != nil {
		t.Fatal(err)
	}
	return f
}

// record は本物の証拠書き込み経路で脆弱性を 1 件落とし、finding id を返す。
// この経路は同じトランザクションで通知イベントを登録する。この機能の接続点である。
func (f *notifyFixture) record(t *testing.T, vulnclass, severity string) int64 {
	t.Helper()
	out, err := f.s.evidenceStore().Record(context.Background(), db.RecordFindingInput{
		TaskID:        f.taskID,
		ExplorationID: f.expID,
		Worker:        "test",
		VulnClass:     vulnclass,
		Name:          vulnclass,
		Severity:      severity,
		Summary:       vulnclass + " の要約",
		Evidence:      "poc",
	}, nil)
	if err != nil {
		t.Fatalf("脆弱性の記録に失敗しました: %v", err)
	}
	return out.FindingID
}

// channel はチャネル設定を読み戻す（チャネル単位の stepX 呼び出しに使う）。
func (f *notifyFixture) channel(t *testing.T, id int64) *db.NotificationChannel {
	t.Helper()
	ch, err := f.pg.NotificationChannelByID(context.Background(), id)
	if err != nil {
		t.Fatalf("チャネルの読み取りに失敗しました: %v", err)
	}
	return ch
}

// deliver はイベントを分配し、指定したチャネルだけ 1 ラウンド配信する。
func (f *notifyFixture) deliver(t *testing.T, chID int64, baseURL string) {
	t.Helper()
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatalf("振り分けに失敗しました: %v", err)
	}
	f.n.stepRealtime(ctx, f.channel(t, chID), 50, baseURL)
}

// createChannel は HTTP API でチャネルを作り、API 自身の検証経路も覆う。
func (f *notifyFixture) createChannel(t *testing.T, payload map[string]any) int64 {
	t.Helper()
	raw, _ := json.Marshal(payload)
	r := f.request("POST", "/api/notify/channels", string(raw))
	if r.Code != 200 {
		t.Fatalf("チャネルの作成に失敗しました %d: %s", r.Code, r.Body)
	}
	var res struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &res); err != nil || res.ID == 0 {
		t.Fatalf("チャネル作成の応答が異常です: %s (%v)", r.Body, err)
	}
	t.Cleanup(func() { f.pg.Exec(`DELETE FROM notification_channels WHERE id=$1`, res.ID) })
	return res.ID
}

// fakeWebhook は、受け取ったリクエストボディを記録する仮の受信側である。
type fakeWebhook struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []map[string]any
}

func newFakeWebhook(t *testing.T) *fakeWebhook {
	t.Helper()
	f := &fakeWebhook{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeWebhook) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

func (f *fakeWebhook) body(t *testing.T, i int) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.bodies) {
		t.Fatalf("仮の受信側は %d 件しか受け取っておらず、%d 件目がありません", len(f.bodies), i)
	}
	return f.bodies[i]
}

func (f *fakeWebhook) last(t *testing.T) map[string]any {
	t.Helper()
	if f.count() == 0 {
		t.Fatal("仮の受信側はリクエストを 1 件も受け取っていません")
	}
	return f.body(t, f.count()-1)
}

// markdownText はリクエストボディから本文を取り出す。各社のフィールド名の違いに合わせる。
// DingTalk の markdown は `text`、ActionCard は `text`、WeCom の markdown は `content` を使う。
func markdownText(t *testing.T, body map[string]any) string {
	t.Helper()
	for _, key := range []string{"markdown", "actionCard"} {
		section, ok := body[key].(map[string]any)
		if !ok {
			continue
		}
		for _, field := range []string{"text", "content"} {
			if s, ok := section[field].(string); ok && s != "" {
				return s
			}
		}
	}
	t.Fatalf("リクエスト本文に識別できる本文がありません: %v", body)
	return ""
}

// agePendingBatch は、そのチャネルの未送信配信を古くし、まとめバッチの期限をテストする。
func (f *notifyFixture) agePendingBatch(t *testing.T, chID int64) {
	t.Helper()
	if _, err := f.pg.Exec(`UPDATE notification_deliveries SET created_at = now() - interval '2 hours'
WHERE channel_id=$1 AND state=$2`, chID, db.NotifyStatePending); err != nil {
		t.Fatal(err)
	}
}

func TestNotifyEndToEndRealtimeDelivery(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "即時配信",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	f.record(t, "SQLインジェクション", "high")
	f.deliver(t, chID, "")

	if hook.count() != 1 {
		t.Fatalf("メッセージは 1 件であるべきです。実際は %d", hook.count())
	}
	text := markdownText(t, hook.last(t))
	for _, want := range []string{"SQLインジェクション", "高", "要約"} {
		if !strings.Contains(text, want) {
			t.Fatalf("メッセージ本文に %q がありません:\n%s", want, text)
		}
	}
	// 配信は sent へ進むべきである。
	var pending int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1 AND state <> $2`,
		chID, db.NotifyStateSent).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("配信後も sent になっていないものが %d 件あります", pending)
	}
}

func TestNotifyChannelAPIMasksSecretsAndPreservesOnUpdate(t *testing.T) {
	f := newNotifyFixture(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "マスクの例",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": "https://oapi.dingtalk.com/robot/send?access_token=abc123456", "secret": "SECabcdef123456"},
	})

	r := f.request("GET", "/api/notify/channels", "")
	if r.Code != 200 {
		t.Fatalf("チャネル一覧の取得に失敗しました %d: %s", r.Code, r.Body)
	}
	if strings.Contains(r.Body.String(), "abc123456") || strings.Contains(r.Body.String(), "SECabcdef123456") {
		t.Fatalf("API の応答に認証情報が漏れています: %s", r.Body)
	}
	var listed struct {
		Channels []struct {
			ID         int64          `json:"id"`
			Config     map[string]any `json:"config"`
			SecretKeys []string       `json:"secret_keys"`
		} `json:"channels"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	var mine *struct {
		ID         int64          `json:"id"`
		Config     map[string]any `json:"config"`
		SecretKeys []string       `json:"secret_keys"`
	}
	for i := range listed.Channels {
		if listed.Channels[i].ID == chID {
			mine = &listed.Channels[i]
		}
	}
	if mine == nil {
		t.Fatal("作成したチャネルが一覧にありません")
	}
	if !notify.IsMasked(fmt.Sprint(mine.Config["webhook"])) || !notify.IsMasked(fmt.Sprint(mine.Config["secret"])) {
		t.Fatalf("認証情報フィールドはマスク値であるべきです: %v", mine.Config)
	}
	if len(mine.SecretKeys) == 0 {
		t.Fatal("API は、どのフィールドが認証情報かをフロントに伝えるべきです")
	}

	// PATCH は名前だけ変え、マスクした認証情報を送り返す。本物の認証情報はそのまま残る。
	body, _ := json.Marshal(map[string]any{
		"name":   "改名後",
		"config": map[string]any{"webhook": fmt.Sprint(mine.Config["webhook"]), "secret": fmt.Sprint(mine.Config["secret"])},
	})
	if r := f.request("PATCH", fmt.Sprintf("/api/notify/channels/%d", chID), string(body)); r.Code != 200 {
		t.Fatalf("更新に失敗しました %d: %s", r.Code, r.Body)
	}
	cfg := f.channelConfig(t, chID)
	if cfg["webhook"] != "https://oapi.dingtalk.com/robot/send?access_token=abc123456" {
		t.Fatalf("マスクの送り返しで本物の認証情報が上書きされました: %v", cfg["webhook"])
	}
	if cfg["secret"] != "SECabcdef123456" {
		t.Fatalf("マスクの送り返しで secret が上書きされました: %v", cfg["secret"])
	}
	if f.channel(t, chID).Name != "改名後" {
		t.Fatal("名前が更新されていません")
	}

	// secret を明示的に空にしたら効く（「マスクを送り返すと変えない」とは違う）。
	body, _ = json.Marshal(map[string]any{"config": map[string]any{"secret": ""}})
	if r := f.request("PATCH", fmt.Sprintf("/api/notify/channels/%d", chID), string(body)); r.Code != 200 {
		t.Fatalf("secret のクリアに失敗しました %d: %s", r.Code, r.Body)
	}
	if _, still := f.channelConfig(t, chID)["secret"]; still {
		t.Fatal("空文字は secret を消すべきです")
	}
}

func (f *notifyFixture) channelConfig(t *testing.T, id int64) map[string]any {
	t.Helper()
	var cfg map[string]any
	if err := json.Unmarshal(f.channel(t, id).Config, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestNotifyChannelAPICreateValidation(t *testing.T) {
	f := newNotifyFixture(t)
	cases := []struct {
		name    string
		payload map[string]any
		wantSub string
	}{
		{"種別が不正", map[string]any{"name": "x", "kind": "nope", "config": map[string]any{}}, "チャネル種別が無効"},
		{"名前がない", map[string]any{"kind": notify.KindDingTalk, "config": map[string]any{"webhook": "https://e.com/h"}}, "チャネル名がありません"},
		{"webhook がない", map[string]any{"name": "x", "kind": notify.KindDingTalk, "config": map[string]any{}}, "Webhook"},
		{"webhook のスキームが不正", map[string]any{"name": "x", "kind": notify.KindDingTalk, "config": map[string]any{"webhook": "file:///etc/passwd"}}, "Webhook アドレスが無効"},
		{"モードが不正", map[string]any{"name": "x", "kind": notify.KindDingTalk, "mode": "sometimes", "config": map[string]any{"webhook": "https://e.com/h"}}, "通知モードが無効"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(tc.payload)
			r := f.request("POST", "/api/notify/channels", string(raw))
			if r.Code != 400 {
				t.Fatalf("400 が返るべきです。得た値 %d: %s", r.Code, r.Body)
			}
			if !strings.Contains(r.Body.String(), tc.wantSub) {
				t.Fatalf("エラーは %q に触れるべきです。得た値 %s", tc.wantSub, r.Body)
			}
		})
	}
	if r := f.request("DELETE", "/api/notify/channels/99999999", ""); r.Code != 404 {
		t.Fatalf("存在しないチャネルの削除は 404 であるべきです。得た値 %d", r.Code)
	}
}

func TestNotifyFilterBlocksBelowThreshold(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "重大のみ",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
		"filter": map[string]any{"min_severity": "critical"},
	})
	f.record(t, "危険度の低い問題", "low")
	if _, _, err := f.pg.FanOutPendingEvents(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("しきい値未満の脆弱性は配信を作らないべきです。得た値 %d 件", n)
	}
	f.n.stepRealtime(context.Background(), f.channel(t, chID), 50, "")
	if hook.count() != 0 {
		t.Fatal("除外した脆弱性のメッセージを送ってはいけません")
	}
}

func TestNotifyDigestBatchesMultipleFindingsIntoOneMessage(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "まとめ配信",
		"kind":   notify.KindDingTalk,
		"mode":   db.NotifyModeDigest,
		"config": map[string]any{"webhook": hook.URL},
	})
	for i := 0; i < 3; i++ {
		f.record(t, fmt.Sprintf("まとめ脆弱性%d", i+1), "high")
	}
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatal(err)
	}
	ch := f.channel(t, chID)

	// 期限前は送らない。
	f.n.stepDigest(ctx, ch, 50, "")
	if hook.count() != 0 {
		t.Fatal("まとめバッチの期限前に送信しました")
	}

	// バッチを古くしたあと。3 件が 1 メッセージになる。
	f.agePendingBatch(t, chID)
	f.n.stepDigest(ctx, ch, 50, "")
	if got := hook.count(); got != 1 {
		t.Fatalf("3 件は 1 メッセージにまとまるべきです。実際は %d 件", got)
	}
	text := markdownText(t, hook.last(t))
	if !strings.Contains(text, "直近") || !strings.Contains(text, "3 件") {
		t.Fatalf("まとめメッセージに件数か時間幅の文がありません:\n%s", text)
	}
	for i := 1; i <= 3; i++ {
		if !strings.Contains(text, fmt.Sprintf("まとめ脆弱性%d", i)) {
			t.Fatalf("まとめメッセージに %d 件目がありません:\n%s", i, text)
		}
	}
	// 同じバッチは batch_id を共有する。
	var distinct, total int
	if err := f.pg.QueryRow(`SELECT count(DISTINCT batch_id), count(*) FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&distinct, &total); err != nil {
		t.Fatal(err)
	}
	if total != 3 || distinct != 1 {
		t.Fatalf("3 件の配信は同じ batch_id を共有すべきです。distinct=%d total=%d", distinct, total)
	}
}

func TestNotifyDisabledChannelDoesNotSend(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":    "停止したチャネル",
		"kind":    notify.KindDingTalk,
		"enabled": false,
		"config":  map[string]any{"webhook": hook.URL},
	})
	f.record(t, "停止中の脆弱性", "critical")
	if _, _, err := f.pg.FanOutPendingEvents(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("停止したチャネルは配信を作らないべきです。得た値 %d 件", n)
	}
}

func TestNotifyStatusChangeDelivery(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "状態変更の購読",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
		"filter": map[string]any{"on_status_change": true},
	})
	finding := f.record(t, "状態変更の例", "high")
	r := f.request("PATCH", fmt.Sprintf("/api/exploration/findings/%d", finding), `{"status":"fixed"}`)
	if r.Code != 200 {
		t.Fatalf("状態の変更に失敗しました %d: %s", r.Code, r.Body)
	}
	f.deliver(t, chID, "")

	// 2 件あるべきである。fixed の 1 件は状態変更。finding_created も同じラウンドで出ることがある。
	// 状態変更の方が実際には後から作られる。順序には依存せず、全部から探す。
	found := false
	for i := 0; i < hook.count(); i++ {
		text := markdownText(t, hook.body(t, i))
		if strings.Contains(text, "状態の変更") && strings.Contains(text, "修正済み") {
			found = true
		}
	}
	if !found {
		t.Fatalf("「状態の変更」と「修正済み」を含むメッセージがありません（計 %d 件）", hook.count())
	}
}

func TestNotifyStatusChangeSuppressedByDefault(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "状態変更を購読しない",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	finding := f.record(t, "変更を購読しない", "high")
	if r := f.request("PATCH", fmt.Sprintf("/api/exploration/findings/%d", finding), `{"status":"false_positive"}`); r.Code != 200 {
		t.Fatalf("状態の変更に失敗しました %d: %s", r.Code, r.Body)
	}
	if _, _, err := f.pg.FanOutPendingEvents(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries d
JOIN notification_events e ON e.id = d.event_id
WHERE d.channel_id=$1 AND e.kind=$2`, chID, notify.EventFindingStatusChanged).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("状態変更を購読していないチャネルは、その配信を受け取らないべきです。得た値 %d 件", n)
	}
}

func TestNotifyTestMessageEndpoint(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "テスト送信",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	if r := f.request("POST", fmt.Sprintf("/api/notify/channels/%d/test", chID), ""); r.Code != 200 {
		t.Fatalf("テスト送信に失敗しました %d: %s", r.Code, r.Body)
	}
	if hook.count() != 1 {
		t.Fatalf("仮の受信側はテストメッセージを 1 件受け取るべきです。得た値 %d", hook.count())
	}
	// テストメッセージは、テストだと一目で分かり、本物の脆弱性と取り違えてはならない。
	if text := markdownText(t, hook.last(t)); !strings.Contains(text, "テスト") {
		t.Fatalf("テストメッセージにはテストだと分かる表示があるべきです: %s", text)
	}
	// 設定が壊れているときは、チャネルの生のエラーをそのままユーザーへ返す。
	badID := f.createChannel(t, map[string]any{
		"name":   "壊れたアドレス",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": "http://127.0.0.1:1/hook"},
	})
	if r := f.request("POST", fmt.Sprintf("/api/notify/channels/%d/test", badID), ""); r.Code != 502 {
		t.Fatalf("配信失敗は 502 であるべきです。得た値 %d: %s", r.Code, r.Body)
	}
}

func TestNotifyDeliveriesHistoryAndRetry(t *testing.T) {
	f := newNotifyFixture(t)
	// 必ず失敗するアドレスを指し、failed の配信を作る。
	chID := f.createChannel(t, map[string]any{
		"name":   "失敗時の再試行",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": "http://127.0.0.1:1/hook"},
	})
	f.record(t, "失敗する配信", "high")
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatal(err)
	}
	ch := f.channel(t, chID)
	// 再試行の予算を使い切るまで送り続ける。
	for i := 0; i < db.MaxNotifyAttempts; i++ {
		f.n.stepRealtime(ctx, ch, 50, "")
		if _, err := f.pg.Exec(`UPDATE notification_deliveries SET next_attempt_at = now() - interval '1 minute' WHERE channel_id=$1`, chID); err != nil {
			t.Fatal(err)
		}
	}
	var state string
	if err := f.pg.QueryRow(`SELECT state FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != db.NotifyStateFailed {
		t.Fatalf("再試行を使い切ったら failed であるべきです。得た値 %s", state)
	}

	r := f.request("GET", fmt.Sprintf("/api/notify/deliveries?channel_id=%d&state=failed", chID), "")
	if r.Code != 200 {
		t.Fatalf("履歴の取得に失敗しました %d: %s", r.Code, r.Body)
	}
	var hist struct {
		Deliveries []struct {
			ID        int64  `json:"id"`
			State     string `json:"state"`
			LastError string `json:"last_error"`
			Attempts  int    `json:"attempts"`
			Title     string `json:"title"`
		} `json:"deliveries"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &hist); err != nil {
		t.Fatal(err)
	}
	if hist.Total != 1 || len(hist.Deliveries) != 1 {
		t.Fatalf("失敗した配信が 1 件見つかるべきです。total=%d len=%d", hist.Total, len(hist.Deliveries))
	}
	if hist.Deliveries[0].LastError == "" {
		t.Fatal("履歴には失敗理由があるべきです。無いとユーザーは原因を追えません")
	}
	if hist.Deliveries[0].Attempts < db.MaxNotifyAttempts {
		t.Fatalf("試行回数は記録されるべきです。得た値 %d", hist.Deliveries[0].Attempts)
	}
	if hist.Deliveries[0].Title != "失敗する配信" {
		t.Fatalf("履歴には脆弱性のタイトルがあるべきです。得た値 %q", hist.Deliveries[0].Title)
	}

	// 手動の再送。pending に戻り、カウントはゼロになる。
	if r := f.request("POST", fmt.Sprintf("/api/notify/deliveries/%d/retry", hist.Deliveries[0].ID), ""); r.Code != 200 {
		t.Fatalf("再送に失敗しました %d: %s", r.Code, r.Body)
	}
	var attempts int
	if err := f.pg.QueryRow(`SELECT state, attempts FROM notification_deliveries WHERE id=$1`, hist.Deliveries[0].ID).Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != db.NotifyStatePending || attempts != 0 {
		t.Fatalf("再送後は pending かつ attempts=0 であるべきです。得た値 %s/%d", state, attempts)
	}
}

func TestNotifyMetaAndSettingsRoundTrip(t *testing.T) {
	f := newNotifyFixture(t)
	r := f.request("GET", "/api/notify/meta", "")
	if r.Code != 200 {
		t.Fatalf("meta に失敗しました: %s", r.Body)
	}
	var meta struct {
		Kinds []struct {
			Kind       string   `json:"kind"`
			SecretKeys []string `json:"secret_keys"`
		} `json:"kinds"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &meta); err != nil {
		t.Fatal(err)
	}
	if len(meta.Kinds) != len(notify.Kinds()) {
		t.Fatalf("meta はチャネルを %d 件すべて出すべきです。得た値 %d", len(notify.Kinds()), len(meta.Kinds))
	}
	for _, k := range meta.Kinds {
		if len(k.SecretKeys) == 0 {
			t.Errorf("チャネル %s は認証情報フィールドを報告していません", k.Kind)
		}
	}

	// 全体設定 3 項目の往復。末尾のスラッシュは正規化して除く。残すと戻りリンクが "//function/..." になる。
	if r := f.request("PUT", "/api/settings", `{"notify_public_base_url":"https://artex.example.com/","notify_digest_interval_min":15,"notify_enabled":true}`); r.Code != 200 {
		t.Fatalf("設定の書き込みに失敗しました %d: %s", r.Code, r.Body)
	}
	t.Cleanup(func() {
		f.pg.Exec(`DELETE FROM settings WHERE key IN ($1,$2)`, settingNotifyPublicBaseURL, settingNotifyDigestMinutes)
	})
	payload := f.s.settingsPayload()
	if payload["notify_public_base_url"] != "https://artex.example.com" {
		t.Fatalf("戻りリンクのアドレスが正規化されていません: %v", payload["notify_public_base_url"])
	}
	if payload["notify_digest_interval_min"] != 15 {
		t.Fatalf("まとめの周期が反映されていません: %v", payload["notify_digest_interval_min"])
	}

	// 不正な値は拒む。
	for _, body := range []string{
		`{"notify_public_base_url":"ftp://x"}`,
		`{"notify_digest_interval_min":0}`,
		`{"notify_digest_interval_min":99999}`,
	} {
		if r := f.request("PUT", "/api/settings", body); r.Code != 400 {
			t.Errorf("%s は 400 であるべきです。得た値 %d", body, r.Code)
		}
	}
}

// TestNotifyDeepLinkUsesPublicBaseURL は戻りリンクの連結を覆う。public_base_url があるとき
// 1 件のメッセージはボタン付きの ActionCard を使い、リンクは脆弱性の詳細ページを指す。
func TestNotifyDeepLinkUsesPublicBaseURL(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "戻りリンク",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	finding := f.record(t, "戻りリンク付きの脆弱性", "high")
	f.deliver(t, chID, "https://artex.example.com")

	body := hook.last(t)
	card, _ := body["actionCard"].(map[string]any)
	if card == nil {
		t.Fatalf("戻りリンクがあるときは ActionCard であるべきです。msgtype=%v", body["msgtype"])
	}
	want := fmt.Sprintf("https://artex.example.com/function/findings/detail?id=%d", finding)
	if card["singleURL"] != want {
		t.Fatalf("戻りリンクが違います\n期待 %s\n得た値 %v", want, card["singleURL"])
	}
}

// TestNotifyNoDeepLinkWithoutBaseURL は逆を覆う。外部アドレスがないとき、壊れたリンクを作らない
// （localhost や相対パスを指さない）。プレーンな markdown に戻る。
func TestNotifyNoDeepLinkWithoutBaseURL(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "戻りリンクなし",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	f.record(t, "戻りリンクのない脆弱性", "high")
	f.deliver(t, chID, "")

	body := hook.last(t)
	if body["msgtype"] != "markdown" {
		t.Fatalf("外部アドレスが無いときは markdown を送るべきです。得た値 %v", body["msgtype"])
	}
	if text := markdownText(t, body); strings.Contains(text, "詳細を見る") {
		t.Fatalf("外部アドレスが無いときは詳細リンクを出してはいけません:\n%s", text)
	}
}

// TestNotifyDigestSegmentsAndDefersRemainder は「静かに失われる」修正のエンドツーエンドの証拠である。
//
// まとめメッセージはチャネルの長さ上限に縛られる（WeCom は 4096 バイト）。1 バッチに入りきらないときは項目単位で切る。
// このメッセージに入ったものは到達済みにし、残りはキューへ戻して次のメッセージを待つ。以前の実装はバッチ全体を
// 成功にしていた。切り捨てられたものはメッセージにも失敗リストにもなく、配信履歴は成功と出して、
// 脆弱性はそのまま消えた。
//
// 4 つを確かめる。① 実際に入った件数だけをマークした ② 残りは未送信のまま ③ 遅らせた項目は
// 再試行回数を消費していない ④ もう 1 ラウンドで残りを送れる（止まらない）。
func TestNotifyDigestSegmentsAndDefersRemainder(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	// WeCom を使う。markdown の上限は 4096 バイトで、6 チャネルの中で最もきつい。
	chID := f.createChannel(t, map[string]any{
		"name":   "分割したまとめ",
		"kind":   notify.KindWeCom,
		"mode":   db.NotifyModeDigest,
		"config": map[string]any{"webhook": hook.URL},
	})
	const total = 60
	// タイトルを少し長くし、60 件が 4096 バイトを大きく超えて、必ず分割されるようにする。
	longName := strings.Repeat("超长漏洞名称", 6)
	for i := 0; i < total; i++ {
		f.record(t, longName+strconv.Itoa(i+1), "high")
	}
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatal(err)
	}
	f.agePendingBatch(t, chID)
	ch := f.channel(t, chID)

	f.n.stepDigest(ctx, ch, 50, "")
	if hook.count() != 1 {
		t.Fatalf("メッセージは 1 件だけであるべきです。得た値 %d", hook.count())
	}

	var sent, pending int
	if err := f.pg.QueryRow(`SELECT
    count(*) FILTER (WHERE state=$2),
    count(*) FILTER (WHERE state=$3)
  FROM notification_deliveries WHERE channel_id=$1`, chID, db.NotifyStateSent, db.NotifyStatePending).
		Scan(&sent, &pending); err != nil {
		t.Fatal(err)
	}
	if sent == 0 {
		t.Fatal("到達済みと記録された項目があるべきです")
	}
	if pending == 0 {
		t.Fatalf("%d 件を 4096 バイトに全部は入りません。残りは未送信であるべきです。sent=%d", total, sent)
	}
	if sent+pending != total {
		t.Fatalf("件数が合いません。sent=%d pending=%d total=%d（到達も未送信も無いものは欠落）", sent, pending, total)
	}
	// メッセージ本文は、このメッセージに入っていない件数がいくつ残っているかをそのまま伝える。
	if text := markdownText(t, hook.last(t)); !strings.Contains(text, "残り") {
		t.Fatalf("このメッセージに入りきらない項目があることを書くべきです:\n%.400s", text)
	}

	// 遅らせた項目は再試行の予算を消費しない。受け取り時に attempts は楽観的に +1 されており、遅らせるときに戻す。
	var maxAttempts int
	if err := f.pg.QueryRow(`SELECT COALESCE(max(attempts),0) FROM notification_deliveries
WHERE channel_id=$1 AND state=$2`, chID, db.NotifyStatePending).Scan(&maxAttempts); err != nil {
		t.Fatal(err)
	}
	if maxAttempts > 0 {
		t.Fatalf("先送りした項目は再試行回数を消費しないべきです（消費すると数件で失敗になります）。attempts=%d", maxAttempts)
	}

	// 収束するまで繰り返す。確かめるのは最終的に全部届き、途中で複数ラウンドに分かれたことである。
	// 「2 ラウンド目で送り終わる」より強い。分割が止まらず、残りの項目も捨てないことを示す。
	rounds := 0
	for {
		var undelivered int
		if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries
WHERE channel_id=$1 AND state <> $2 AND state <> $3`, chID, db.NotifyStateSent, db.NotifyStateFailed).
			Scan(&undelivered); err != nil {
			t.Fatal(err)
		}
		if undelivered == 0 {
			break
		}
		rounds++
		if rounds > total+5 {
			t.Fatalf("分割配信が収束しません。%d ラウンド回しても %d 件が未決着です", rounds, undelivered)
		}
		before := hook.count()
		f.n.stepDigest(ctx, ch, 50, "")
		if hook.count() == before {
			t.Fatalf("%d ラウンド目で進捗が無く、残り %d 件が止まったままになります", rounds, undelivered)
		}
	}
	if rounds < 2 {
		t.Fatalf("4096 バイトのメッセージに長いタイトルの脆弱性 %d 件は入りません。複数ラウンドになるべきです。実際は %d ラウンド", total, rounds)
	}
	// 最初のラウンドのあとの各ラウンドは、続きの送信だけである。チャネルに拒まれた項目はない。
	var failed int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1 AND state=$2`,
		chID, db.NotifyStateFailed).Scan(&failed); err != nil {
		t.Fatal(err)
	}
	if failed != 0 {
		t.Fatalf("仮の受信側は常に成功を返すので、失敗項目は無いはずです。得た値 %d", failed)
	}
}

// TestNotifyBackoffTableMatchesAttemptBudget は、ずれを防ぐアサーションである。
//
// 再試行の予算（db.MaxNotifyAttempts）とバックオフの列（notifyBackoff）は別のパッケージにある。
// 前者は状態機械の方針、後者はエンジンの実行の間隔である。片方だけ変えると、たとえば予算を 5
// 回に上げてバックオフの段を足し忘れると、コードはエラーを出さない。4 回目と 5 回目の再試行が最後の段の間隔のままになり、
// 「再試行の間隔が理由もなく遅くなる」と見え、調べるときにここだと結びつきにくい。
// 両方の長さが一致することを確かめ、このずれを CI で出す。
func TestNotifyBackoffTableMatchesAttemptBudget(t *testing.T) {
	if len(notifyBackoff) != db.MaxNotifyAttempts {
		t.Fatalf("バックオフの段数(%d)と最大試行回数(%d)が一致しません。片方を変えたらもう片方も変えてください",
			len(notifyBackoff), db.MaxNotifyAttempts)
	}
	// バックオフの間隔は単調に減らない。減ると再試行のたびに急になり、レート制限を強める。
	for i := 1; i < len(notifyBackoff); i++ {
		if notifyBackoff[i] < notifyBackoff[i-1] {
			t.Fatalf("バックオフ間隔は単調非減少であるべきです。%d 段目 %v < %d 段目 %v",
				i, notifyBackoff[i], i-1, notifyBackoff[i-1])
		}
	}
}

// TestNotifyRateLimitDoesNotConsumeRetryBudget は「先にトークンを取ってから受け取る」順序を固定する。
// 逆だと（先に受け取ってから捨てる）、レート制限で止まった配信が attempts を 1 回すでに数え、
// 待っているだけで予算を使い切り、最後は failed になる。
func TestNotifyRateLimitDoesNotConsumeRetryBudget(t *testing.T) {
	// トークンバケット自体だけを試す。Server は要らない（そのために作るべきでもない）。
	n := &Notifier{buckets: map[int64]*notifyBucket{}}
	now := time.Now()
	// 毎分 1 件。満杯のときは最大 1 件。
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now); got != 1 {
		t.Fatalf("満杯のとき、毎分 1 件ならトークンを 1 つ取るべきです。得た値 %d", got)
	}
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(time.Millisecond)); got != 0 {
		t.Fatalf("トークンを使い切ったらすぐ 0 を返すべきです。得た値 %d", got)
	}
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(30*time.Second)); got != 0 {
		t.Fatalf("半分の時間ではトークンが 1 つ戻らないべきです。得た値 %d", got)
	}
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(time.Minute)); got != 1 {
		t.Fatalf("1 周期たてばトークンが 1 つ戻るべきです。得た値 %d", got)
	}
	// レート制限のないチャネルは有限の上限を通る。1 ラウンドが無限の滞留に引きずられないようにする。
	if got := n.takeTokens(2, 0, notifyUnlimitedBurstPerTick+10, now); got != notifyUnlimitedBurstPerTick {
		t.Fatalf("無制限ならラウンド上限 %d を返すべきです。得た値 %d", notifyUnlimitedBurstPerTick, got)
	}
	// チャネル同士のトークンバケットは互いに独立している。
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(time.Millisecond)); got != 0 {
		t.Fatalf("チャネル 1 のバケットは空のままであるべきです。得た値 %d", got)
	}
}

// TestNotifyTakeTokensKeepsUnusedTokens は「want 個だけ取る」意味を固定する。
//
// 以前の実装はバケットを全部空にしてから呼び出し側が切り詰めていた。rate=100/min のチャネルが満杯のとき、
// 1 ラウンドで 5 件しか使わず、残りの 95 トークンをそのまま捨てる。そのラウンドに未送信の配信がなくても同様に引く。
// 結果、コメントが言う「滞留時に rate_per_min 件を一度に送れる」は、どの場合もできない。
func TestNotifyTakeTokensKeepsUnusedTokens(t *testing.T) {
	n := &Notifier{buckets: map[int64]*notifyBucket{}}
	now := time.Now()
	// バケットの初期値は満杯（100）。このラウンドは 5 個だけ欲しい。
	if got := n.takeTokens(1, 100, 5, now); got != 5 {
		t.Fatalf("want=5 ならトークンをちょうど 5 つ取るべきです。得た値 %d", got)
	}
	// 重要な確認。残りの 95 個はバケットに残る。空にして捨てない。
	// 時間は進めない。取れたものは残量だけから来て、補充からではない。
	if got := n.takeTokens(1, 100, 95, now); got != 95 {
		t.Fatalf("残りのトークンはまだ取れるべきです（期待 95）。得た値 %d。バケットがラウンドで空になりました", got)
	}
	if got := n.takeTokens(1, 100, 1, now); got != 0 {
		t.Fatalf("バケットは空なので 0 を返すべきです。得た値 %d", got)
	}
	// want<=0 はトークンを減らさない（空のラウンドは課金しない）。
	n2 := &Notifier{buckets: map[int64]*notifyBucket{}}
	if got := n2.takeTokens(1, 20, 0, now); got != 0 {
		t.Fatalf("want=0 は 0 を返すべきです。得た値 %d", got)
	}
	if got := n2.takeTokens(1, 20, 20, now); got != 20 {
		t.Fatalf("want=0 はトークンを消費しないので、まだ 20 取れるべきです。得た値 %d", got)
	}
}

// TestDigestTickPlanDecouplesBatchSizeFromSendBudget は、まとめモードの 2 つの次元を固定する。
//
// まとめバッチの大きさを各ラウンドのリクエスト予算に結び付けると、rate_per_min=20 のチャネルは各
// 3 秒の tick でトークンが 1 つしか補充されず、まとめメッセージ 1 件に脆弱性が 1 件しか入らない。機能としては
// まとめがないのと同じで、メッセージの見出しは「直近 30 分の新規脆弱性 1 件」と書く。この退化はエラーを出さず、
// 既存のエンドツーエンドの用例からも見えない（stepDigest に十分大きい limit を手で渡し、
// step の枠計算を迂回している）。ここでは判断そのものを直接確かめる。
func TestDigestTickPlanDecouplesBatchSizeFromSendBudget(t *testing.T) {
	tokens, claimLimit := digestTickPlan()
	// 1 バッチ = メッセージ 1 件 = リクエスト 1 回 = トークン 1 個。トークンの単位はメッセージであり、脆弱性ではない。
	if tokens != 1 {
		t.Fatalf("まとめ 1 バッチはメッセージ 1 件なので、トークンはちょうど 1 つ消費するべきです。得た値 %d", tokens)
	}
	if claimLimit != db.MaxDigestBatchSize {
		t.Fatalf("まとめバッチの大きさはメモリ上限 db.MaxDigestBatchSize=%d であるべきです。得た値 %d",
			db.MaxDigestBatchSize, claimLimit)
	}
	// 重要な関係。バッチの大きさは、各ラウンドのリクエスト予算よりずっと大きくなければならない。同じ桁になると、
	// 「メッセージを何件送るか」と「1 バッチに脆弱性を何件入れるか」を、また 1 つの数に混ぜたことになる。
	if claimLimit <= notifyMaxSendsPerChannelPerTick {
		t.Fatalf("まとめバッチの大きさ %d は、ラウンドごとのリクエスト予算 %d に縛られないべきです。"+
			"リクエスト予算はリースから逆算した「何回リクエストするか」であり、「1 バッチに脆弱性を何件入れるか」とは別の単位です",
			claimLimit, notifyMaxSendsPerChannelPerTick)
	}
}

// TestNotifyTickBudgetFitsWithinLease も、ずれを防ぐアサーションである。
//
// 1 チャネルの各ラウンドの配信件数の上限（notifyMaxSendsPerChannelPerTick）は、リース時間から逆算する。
// 1 ラウンドで直列に配信する最悪時間はリースより短くなければならない。超えると、後ろの数件は送り終わる前にリースが切れる。
// 複数インスタンスでは、相手がそれらを取り直して重複送信する。この 3 つの定数は別の場所にあり、
// どれか 1 つを変えると関係が壊れてもエラーは出ない。ここで固定する。
func TestNotifyTickBudgetFitsWithinLease(t *testing.T) {
	worst := time.Duration(notifyMaxSendsPerChannelPerTick) * notifySendTimeout
	if worst >= notifyLease {
		t.Fatalf("チャネル 1 つ、1 ラウンドの最悪時間 %v がリース %v 以上になってはいけません"+
			"（notifyMaxSendsPerChannelPerTick=%d × notifySendTimeout=%v）——"+
			"この 3 つの定数は、どれか 1 つを変えたら残り 2 つも確認してください",
			worst, notifyLease, notifyMaxSendsPerChannelPerTick, notifySendTimeout)
	}
}
