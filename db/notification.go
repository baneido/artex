package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/Autumn-27/artex/notify"
)

// このファイルは IM 通知のチャネル設定とイベント層である。配信タスクの取得と状態遷移は
// db/notification_delivery.go を参照。
//
// 変えるときに守る不変条件は 2 つ。
//
//  1. 脆弱性を書くトランザクション（RecordFindingTx）は InsertNotificationEventTx を 1 回の盲 INSERT で呼ぶだけにし、
//     通知関連の表は読まず、フィルタ照合もしない。ここで読み取りを足すと、
//     ユーザーが誤って入れたフィルタが脆弱性の書き込みトランザクションを汚染し、中止することさえある。
//  2. フィルタ照合は決してエラーにしない。壊れた設定は一律「当たった」と扱う（notify.Match を参照）。多く送る方を取り、
//     漏らさない。

// ErrNotificationChannelNotFound は、チャネルが存在しない。
var ErrNotificationChannelNotFound = errors.New("通知チャネルが存在しません")

// 配信状態。
const (
	NotifyStatePending = "pending" // 未配信
	NotifyStateSending = "sending" // いずれかの dispatcher が取得済みで、リース期限内
	NotifyStateSent    = "sent"    // 配信済み
	NotifyStateFailed  = "failed"  // 再試行を使い切ったか恒久失敗。手動再送できる
	NotifyStateSkipped = "skipped" // チャネルは無効で、もう送らない
)

// 通知モード。
const (
	NotifyModeRealtime = "realtime"
	NotifyModeDigest   = "digest"
)

// ValidNotifyMode は通知モードを許可リストで検証する（findings.status と同じ。DB CHECK は使わず、
// 後から広げやすくする）。
func ValidNotifyMode(m string) bool {
	return m == NotifyModeRealtime || m == NotifyModeDigest
}

// NotificationChannel はチャネルインスタンスの設定である。Config と Filter は生の JSON のままにし、
// 解釈は notify パッケージへ渡す。db 層はフィールドの意味を知らない。
type NotificationChannel struct {
	ID     int64           `json:"id"`
	Name   string          `json:"name"`
	Kind   string          `json:"kind"`
	Mode   string          `json:"mode"`
	Config json.RawMessage `json:"config"`
	Filter json.RawMessage `json:"filter"`
	// Enabled をポインタにするのは「このフィールドを送っていない」と「明示的に false」を分けるためである。
	// 画面のスイッチは、変わったフィールドだけを送る。
	Enabled    *bool     `json:"enabled,omitempty"`
	RatePerMin int       `json:"rate_per_min"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// IsEnabled はチャネルが有効かを返す。Enabled が nil（未ロード）のときは有効として扱う。
func (c *NotificationChannel) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

// NotificationEvent はイベントの事実 1 件である。
type NotificationEvent struct {
	ID        int64           `json:"id"`
	Kind      string          `json:"kind"`
	FindingID int64           `json:"finding_id"`
	Snapshot  json.RawMessage `json:"snapshot"`
	CreatedAt time.Time       `json:"created_at"`
}

const notificationChannelCols = `id, name, kind, enabled, config, mode, filter, rate_per_min, created_at, updated_at`

func scanNotificationChannel(sc interface{ Scan(...any) error }) (*NotificationChannel, error) {
	var c NotificationChannel
	var enabled bool
	if err := sc.Scan(&c.ID, &c.Name, &c.Kind, &enabled, &c.Config, &c.Mode, &c.Filter, &c.RatePerMin, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	c.Enabled = &enabled
	return &c, nil
}

// ListNotificationChannels はチャネルインスタンスをすべて返す。有効なものを先にし、同じ段は id 順である。
// 並びを SQL に置くのは、UI と dispatcher が同じ安定した順を見るためである。
func (d *DB) ListNotificationChannels(ctx context.Context) ([]*NotificationChannel, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+notificationChannelCols+` FROM notification_channels
ORDER BY enabled DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationChannel{}
	for rows.Next() {
		c, err := scanNotificationChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// NotificationChannelByID はチャネルを 1 件取る。
func (d *DB) NotificationChannelByID(ctx context.Context, id int64) (*NotificationChannel, error) {
	row := d.QueryRowContext(ctx, `SELECT `+notificationChannelCols+` FROM notification_channels WHERE id=$1`, id)
	c, err := scanNotificationChannel(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotificationChannelNotFound
	}
	return c, err
}

// SaveNotificationChannel はチャネルを新規作成または更新する。
//
// 更新時は、呼び出し側が明示したフィールド（nil でない、空でない）だけを上書きする。画面は部分的に
// 変えたドロワーのフォームを送れて、表示していない config のフィールドを送り返さなくて済む。送り返すと
// 「マスク値が本物の秘密を上書きする」事故になる。
func (d *DB) SaveNotificationChannel(ctx context.Context, c *NotificationChannel) (int64, error) {
	if c.Mode == "" {
		c.Mode = NotifyModeRealtime
	}
	// ここでは意図的に 0 を**加工しない**。0 は正当な設定で、意味は「レート制限なし」である。
	//
	// 以前は `if c.RatePerMin <= 0 { c.RatePerMin = 既定値 }` と書いていた。意図は「未指定のときは
	// 安全な既定を入れる」だったが、「明示的な 0」まで一緒に飲み込んでいた。文書、UI の説明、
	// takeTokens はいずれも 0 をレート制限なしと解釈するのに、ここだけが黙って 20（DingTalk、企業 WeChat、Telegram）
	// または 100（Feishu）へ変えていた。操作者は制限を外したつもりでも、実際は毎分 20 で止まっており、その表示も無かった。
	//
	// 「未指定」と「明示的な 0」の違いは呼び出し側だけが知っている（リクエスト本体でフィールドが欠けるか、明示的に 0 か）。
	// だから既定値は、フィールドが欠けるときに server 層が埋める。notifyCreateChannel を参照。
	if c.RatePerMin < 0 {
		return 0, errors.New("レート制限値は負の値にできません")
	}
	if c.Config == nil {
		c.Config = json.RawMessage(`{}`)
	}
	if c.Filter == nil {
		c.Filter = json.RawMessage(`{}`)
	}
	enabled := c.IsEnabled()

	if c.ID == 0 {
		var id int64
		err := d.QueryRowContext(ctx, `INSERT INTO notification_channels(name,kind,enabled,config,mode,filter,rate_per_min)
VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
			c.Name, c.Kind, enabled, string(c.Config), c.Mode, string(c.Filter), c.RatePerMin).Scan(&id)
		return id, err
	}
	res, err := d.ExecContext(ctx, `UPDATE notification_channels
SET name=$2, kind=$3, enabled=$4, config=$5, mode=$6, filter=$7, rate_per_min=$8
WHERE id=$1`,
		c.ID, c.Name, c.Kind, enabled, string(c.Config), c.Mode, string(c.Filter), c.RatePerMin)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrNotificationChannelNotFound
	}
	return c.ID, nil
}

// SetNotificationChannelEnabled は有効と無効を切り替える。
//
// チャネルを無効にするとき、まだ送っていない配信もまとめて skipped にする。そうしないと、再び有効にしたあと
// 「無効のあいだに滞留した」古い脆弱性がまとめて届き、鮮度は無く、新規と誤認しやすい。
func (d *DB) SetNotificationChannelEnabled(ctx context.Context, id int64, enabled bool) error {
	return d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE notification_channels SET enabled=$2 WHERE id=$1`, id, enabled)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotificationChannelNotFound
		}
		if !enabled {
			if _, err := tx.ExecContext(ctx, `UPDATE notification_deliveries SET state=$2, last_error=$3
WHERE channel_id=$1 AND state IN ($4,$5)`,
				id, NotifyStateSkipped, "チャネルが無効になっています", NotifyStatePending, NotifyStateSending); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteNotificationChannel はチャネルを削除する。配信履歴は外部キーでカスケード削除される
// （チャネル設定が無くなれば、履歴は解釈できない）。
func (d *DB) DeleteNotificationChannel(ctx context.Context, id int64) error {
	res, err := d.ExecContext(ctx, `DELETE FROM notification_channels WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotificationChannelNotFound
	}
	return nil
}

// RecordNotificationEventTx は、呼び出し側のトランザクションで通知イベントを 1 件**できる範囲で**書く。
//
// 脆弱性の書き込み経路で通知に触れるのはここだけである。INSERT は 1 回で、どの表も読まず、チャネルも知らず、
// フィルタも走らせない。トランザクションのコミットが「脆弱性の保存」と「通知タスクの存在」を原子的に揃える。
// コミットは成功したのにキューへ入らず、メッセージが永久に失われる窓は無い。
//
// 重要な設計は 2 つで、どちらもその場の思いつきではない。
//
//  1. **SAVEPOINT を使う理由**。PostgreSQL では、トランザクション内の文が 1 つでも失敗するとトランザクション全体が
//     aborted になり、その後の文（COMMIT を含む）はすべて失敗する。「この INSERT の
//     エラーを無視して、呼び出し側にコミットを続けさせる」は PG ではできない。セーブポイントでエラーを
//     この 1 文に隔離する場合を除く。セーブポイントが無いと、残る選択肢は「トランザクション全体のロールバック」だけである。
//
//  2. **全体のロールバックが誤りである理由**。通知は便宜の機能で、製品そのものは脆弱性レコードである。通知
//     テーブルの問題（古い DB が未移行、ディスクの瞬断）で、高深刻度の脆弱性を保存できなくしてはならない。だからここではエラーを隔離し、
//     ログを残し、false を返す。脆弱性の書き込みは通常どおりコミットする。代償は、この通知 1 件を捨てることである。
//     error ではなく bool を返すのは意図的である。呼び出し側は、これを書き込みの成否に影響するエラーとして扱ってはならない。
func RecordNotificationEventTx(ctx context.Context, tx *sql.Tx, kind string, findingID int64, snap notify.Snapshot) bool {
	raw, err := json.Marshal(snap)
	if err != nil {
		log.Printf("[notify] 通知イベントのシリアライズに失敗しました finding=%d: %v", findingID, err)
		return false
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT notify_event`); err != nil {
		log.Printf("[notify] セーブポイントの作成に失敗しました finding=%d: %v", findingID, err)
		return false
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO notification_events(kind,finding_id,snapshot) VALUES($1,$2,$3)`,
		kind, findingID, string(raw)); err != nil {
		log.Printf("[notify] 通知イベントの書き込みに失敗しました finding=%d（脆弱性レコードには影響しません）: %v", findingID, err)
		// セーブポイントまでロールバックし、トランザクションを aborted から戻す。
		if _, rbErr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT notify_event`); rbErr != nil {
			log.Printf("[notify] セーブポイントへのロールバックに失敗しました finding=%d: %v", findingID, rbErr)
		}
		return false
	}
	// セーブポイントを解放し、長いトランザクションに無用なセーブポイントを溜めない。
	_, _ = tx.ExecContext(ctx, `RELEASE SAVEPOINT notify_event`)
	return true
}

// AddNotificationEvent は InsertNotificationEventTx の独立トランザクション版である。既存の
// トランザクションの外の呼び出し（チャネルの「テストメッセージを送る」など。本物の finding は無い）に使う。
func (d *DB) AddNotificationEvent(ctx context.Context, kind string, findingID int64, snap notify.Snapshot) (int64, error) {
	raw, err := json.Marshal(snap)
	if err != nil {
		return 0, fmt.Errorf("通知イベントのスナップショットのシリアライズに失敗しました: %w", err)
	}
	var id int64
	err = d.QueryRowContext(ctx, `INSERT INTO notification_events(kind,finding_id,snapshot) VALUES($1,$2,$3) RETURNING id`,
		kind, findingID, string(raw)).Scan(&id)
	return id, err
}

// FanOutPendingEvents は、まだ振り分けていない脆弱性イベントを、現在有効なチャネルに展開して配信タスクにする。
// この周回で処理したイベント数と、新しく作った配信数を返す。
//
// 1 周回は 1 トランザクションである。イベントは FOR UPDATE SKIP LOCKED で取得し、複数プロセスが同時に走っても
// それぞれ別の行を取る（このプロジェクトのアーカイブキューの取得も同じやり方。
// db/task_archives.go の completeNextArchiveJob を参照）。
//
// フィルタ照合は意図的に SQL ではなく Go 側に置く。チャネルのフィルタは任意フィールドの JSONB で、
// 6 通りの組み合わせを SQL で書くとクエリの保守が難しくなる。チャネル数は「人が設定する数件」なので、
// 全部読んでメモリで 1 件ずつ比べる方が速く、テストもしやすい。
//
// どのチャネルにも当たらないイベントも fanned_out にする。そうしないと未振り分けの集合に残り続け、
// tick のたびに再走査される。
func (d *DB) FanOutPendingEvents(ctx context.Context, limit int) (eventCount, deliveryCount int, err error) {
	if limit <= 0 {
		limit = 200
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback() //nolint:errcheck // コミット成功後は no-op

	channels, err := listEnabledNotificationChannelsTx(ctx, tx)
	if err != nil {
		return 0, 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, kind, finding_id, snapshot FROM notification_events
WHERE NOT fanned_out ORDER BY id FOR UPDATE SKIP LOCKED LIMIT $1`, limit)
	if err != nil {
		return 0, 0, err
	}
	var (
		events      []NotificationEvent
		parsedSnaps []notify.Snapshot
	)
	for rows.Next() {
		var ev NotificationEvent
		if err := rows.Scan(&ev.ID, &ev.Kind, &ev.FindingID, &ev.Snapshot); err != nil {
			rows.Close()
			return 0, 0, err
		}
		var snap notify.Snapshot
		// スナップショットは自分たちで書いたもので、理屈の上では必ず解釈できる。解釈に失敗しても配信の流れは止めない。
		// ただしこのイベントはフィールドが全部空なので、フィルタ付きのチャネルはすべて飛ばす。1 件少なく送っても、
		// 壊れた 1 行でキュー全体を止めない。
		_ = json.Unmarshal(ev.Snapshot, &snap)
		// kind は行の値を正とする。スナップショット側は描画用のコピーで、古い版が書いた可能性がある。
		snap.Kind = ev.Kind
		events = append(events, ev)
		parsedSnaps = append(parsedSnaps, snap)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	if len(events) == 0 {
		return 0, 0, tx.Commit()
	}

	type pending struct {
		eventID   int64
		channelID int64
	}
	var toInsert []pending
	for i, snap := range parsedSnaps {
		for _, ch := range channels {
			if !notify.Match(notify.ParseFilter(ch.Filter), snap) {
				continue
			}
			toInsert = append(toInsert, pending{eventID: events[i].ID, channelID: ch.ID})
		}
	}
	if len(toInsert) > 0 {
		var (
			vals []string
			args []any
		)
		for _, p := range toInsert {
			vals = append(vals, fmt.Sprintf("($%d,$%d)", len(args)+1, len(args)+2))
			args = append(args, p.eventID, p.channelID)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO notification_deliveries(event_id,channel_id) VALUES `+strings.Join(vals, ","), args...); err != nil {
			return 0, 0, err
		}
	}

	// この周回のイベントを振り分け済みにする。どのチャネルにも当たらないイベントも一緒に印を付ける（関数コメントを参照）。
	ids := make([]string, 0, len(events))
	markArgs := make([]any, 0, len(events))
	for _, ev := range events {
		markArgs = append(markArgs, ev.ID)
		ids = append(ids, fmt.Sprintf("$%d", len(markArgs)))
	}
	if _, err := tx.ExecContext(ctx, `UPDATE notification_events SET fanned_out=true WHERE id IN (`+strings.Join(ids, ",")+`)`, markArgs...); err != nil {
		return 0, 0, err
	}
	return len(events), len(toInsert), tx.Commit()
}

// listEnabledNotificationChannelsTx はトランザクション内で有効なチャネルを取る。数は少ない。
// ページングもキャッシュも付けない。キャッシュは「設定を変えてからいつ効くか」という余分なタイミング問題を持ち込む。
func listEnabledNotificationChannelsTx(ctx context.Context, tx *sql.Tx) ([]*NotificationChannel, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, name, kind, config, mode, filter, rate_per_min
FROM notification_channels WHERE enabled ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationChannel{}
	for rows.Next() {
		var c NotificationChannel
		if err := rows.Scan(&c.ID, &c.Name, &c.Kind, &c.Config, &c.Mode, &c.Filter, &c.RatePerMin); err != nil {
			return nil, err
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// NotificationAssetNames は資産 id を短い表示名へ解き、通知メッセージに使う。
//
// 戻り値の順は引数と同じで、長さは引数より短いことがある（存在しない id は飛ばす）。引数の順を保つのは、
// 同じ脆弱性のメッセージで、複数回の配信でも資産の順を安定させるためである。そうしないと再試行後のメッセージで
// 資産の順が変わり、「資産が変わった」と誤読される。
func (d *DB) NotificationAssetNames(ctx context.Context, ids []int64) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ph, args := placeholders(1, ids)
	rows, err := d.QueryContext(ctx, `SELECT id, type, domain, ip, url, app_name, bundle_id FROM assets WHERE id IN (`+ph+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	labels := map[int64]string{}
	for rows.Next() {
		var (
			id                int64
			typ               string
			domain, ip, url   sql.NullString
			appName, bundleID sql.NullString
		)
		if err := rows.Scan(&id, &typ, &domain, &ip, &url, &appName, &bundleID); err != nil {
			return nil, err
		}
		labels[id] = assetDisplayName(typ, domain.String, ip.String, url.String, appName.String, bundleID.String)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ids))
	seen := map[int64]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if label, ok := labels[id]; ok && label != "" {
			out = append(out, label)
		}
	}
	return out, nil
}

// assetDisplayName は資産種別に応じて、最も見分けやすい識別子を選ぶ。
// 最後は空文字を返す。「名前が取れない資産」をどう見せるかは呼び出し側が決める。この関数はプレースホルダを作らない。
// 作ると「資産#42」のようなノイズが通知に混ざり、読み手は本物のドメインだと思ってしまう。
func assetDisplayName(typ, domain, ip, url, appName, bundleID string) string {
	pick := func(vals ...string) string {
		for _, v := range vals {
			if strings.TrimSpace(v) != "" {
				return v
			}
		}
		return ""
	}
	switch typ {
	case "root_domain", "subdomain":
		return domain
	case "ip":
		return ip
	case "app":
		return pick(appName, bundleID)
	case "service", "endpoint":
		return pick(url, domain, ip)
	default:
		return pick(domain, ip, url, appName)
	}
}

// SetFindingStatusWithNotify は脆弱性の対応状態を更新し、同じトランザクションで状態変更の
// 通知イベントを 1 件登録する。
//
// 戻り値は from が変更前の状態、found が脆弱性の有無、notified がイベント登録の成否である。
//
// 意図した挙動は 3 つ。
//   - 状態が実際には変わっていないときはイベントを登録しない。画面のドロワーが同じ値を再送したり、自動化スクリプトが
//     冪等に再実行したりしても、通知のノイズを出してはならない。
//   - 脆弱性が無いときは found=false を返し、何も書かない。呼び出し側が 404 に訳す。
//   - イベント登録の失敗は状態更新に影響しない（RecordNotificationEventTx のセーブポイントの説明を参照）。
//     だから notified=false でも状態の変更は成功している。呼び出し側はそれでエラーにしてはならない。
func (d *DB) SetFindingStatusWithNotify(ctx context.Context, id int64, status string) (from string, found bool, notified bool, err error) {
	err = d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		var txErr error
		from, found, _, notified, txErr = SetFindingStatusTx(ctx, tx, id, status)
		return txErr
	})
	return from, found, notified, err
}

// SetFindingStatusTx は**呼び出し側のトランザクション**の中で脆弱性の状態を更新し、状態変更の通知イベントを登録する。
//
// トランザクション単位の関数に分けたのは、状態を変える経路がすべて同じ意味を共有するためである。以前は
// patchFinding だけが通知付きで、**再テストの結論が「修正済み」のとき**（finding_retests
// の `UPDATE findings SET status=...`）は直接 DB へ書いていた。そのため
// `on_status_change` を設定したチャネルは、この状態遷移の通知を全く受け取れなかった。画面上の状態は静かに変わり、
// 運用者はプラットフォームを開くまで気付けなかった。
//
// 戻り値は from が変更前の状態、found が脆弱性の有無、changed が状態が本当に変わったか、
// notified がイベント登録の成否である（登録失敗は状態更新に影響しない。RecordNotificationEventTx を参照）。
func SetFindingStatusTx(ctx context.Context, tx *sql.Tx, id int64, status string) (from string, found bool, changed bool, notified bool, err error) {
	var (
		vulnclass, name, severity, summary string
		taskID                             sql.NullInt64
		assetIDs                           []byte
	)
	scanErr := tx.QueryRowContext(ctx, `SELECT vulnclass, name, severity, summary, task_id, asset_ids, status
FROM findings WHERE id=$1 FOR UPDATE`, id).
		Scan(&vulnclass, &name, &severity, &summary, &taskID, &assetIDs, &from)
	if scanErr == sql.ErrNoRows {
		return "", false, false, false, nil
	}
	if scanErr != nil {
		return "", false, false, false, scanErr
	}
	found = true
	if from == status {
		// 状態が本当に変わっていなければイベントは登録しない。同じ値の再送も、冪等な再実行も、
		// 通知のノイズを出してはならない。
		return from, true, false, false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE findings SET status=$2 WHERE id=$1`, id, status); err != nil {
		return from, true, false, false, err
	}
	var assets []int64
	_ = json.Unmarshal(assetIDs, &assets)
	notified = RecordNotificationEventTx(ctx, tx, notify.EventFindingStatusChanged, id, notify.Snapshot{
		Kind:       notify.EventFindingStatusChanged,
		FindingID:  id,
		TaskID:     taskID.Int64,
		VulnClass:  vulnclass,
		Name:       name,
		Severity:   severity,
		Summary:    summary,
		AssetIDs:   assets,
		FromStatus: from,
		ToStatus:   status,
	})
	return from, true, true, notified, nil
}

// NotificationStats は通知ページ上部の概況カウントである。
type NotificationStats struct {
	Channels     int   `json:"channels"`
	ChannelsOn   int   `json:"channels_on"`
	Pending      int   `json:"pending"`
	Failed       int   `json:"failed"`
	SentToday    int   `json:"sent_today"`
	BacklogAgeMS int64 `json:"backlog_age_ms"` // 最も古い未配信が今から何ミリ秒前か
}

// NotificationStatsSnapshot は通知システムの健全さをまとめる。
// BacklogAgeMS は「通知が止まっていないか」を最も直接に示す指標で、pending の件数より役に立つ。
// 滞留 3 件と滞留 3 件の差は、3 秒から 3 時間まであり得る。
func (d *DB) NotificationStatsSnapshot(ctx context.Context) (*NotificationStats, error) {
	var s NotificationStats
	if err := d.QueryRowContext(ctx, `SELECT
    (SELECT count(*) FROM notification_channels),
    (SELECT count(*) FROM notification_channels WHERE enabled),
    (SELECT count(*) FROM notification_deliveries WHERE state IN ($1,$2)),
    (SELECT count(*) FROM notification_deliveries WHERE state=$3),
    (SELECT count(*) FROM notification_deliveries WHERE state=$4 AND sent_at >= date_trunc('day', now())),
    COALESCE((SELECT EXTRACT(EPOCH FROM (now() - min(created_at))) * 1000 FROM notification_deliveries WHERE state=$1), 0)::bigint`,
		NotifyStatePending, NotifyStateSending, NotifyStateFailed, NotifyStateSent).
		Scan(&s.Channels, &s.ChannelsOn, &s.Pending, &s.Failed, &s.SentToday, &s.BacklogAgeMS); err != nil {
		return nil, err
	}
	return &s, nil
}
