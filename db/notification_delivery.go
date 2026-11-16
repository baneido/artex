package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// このファイルは配信タスクの取得と状態遷移である。
//
// 取得は長いトランザクションではなく「リース」を使う。行を sending にし、next_attempt_at を未来へ動かして
// リース期限にする。トランザクションをコミットしてからネットワーク配信を行う。配信中はデータベースのロックを持たない。
// ネットワーク要求は数秒かかることがある（クライアントのタイムアウトは 15 秒）。行ロックを持ったままだと、同じ DB のほかの書き込みが遅くなる。
//
// 代償は、配信の途中でプロセスが落ちると行が sending のまま残ることである。これは**自分で回復できる**。リース期限後は
// next_attempt_at が過去になり、次の取得が同じ行を再び拾う（取得条件の
// state IN ('pending','sending') を参照）。再試行回数は取得の時点で +1 済みなので、クラッシュが
// 無限の再試行にはならない。MaxNotifyAttempts 回を使い切ると failed になり、人手の対応を待つ。

// MaxNotifyAttempts は配信 1 件の最大試行回数である（初回を含む）。
// 配信エンジンではなくここに置く。これは状態機械自身の方針で、エンジンは実行するだけである。
const MaxNotifyAttempts = 3

// MaxDigestBatchSize は、1 つのまとめバッチが一度に束ねる配信の上限である。
//
// 理由は資源である。1 つのまとめ周期で数万件の脆弱性を拾うことがある（あり得る。全量スキャン 1 回で
// そうなる）。上限が無いと、取得が全行をメモリへ読み、1 通の長すぎるメッセージに描画し、
// チャネルの長さ上限で大半を切る。メモリを無駄にし、切れた脆弱性を**黙って失う**。
// 上限を置くと、超えた分は DB に残って次のバッチになり、次の周期で自然に送られる。失われない。
//
// 500 にした根拠は、メッセージへ描画したあとも企業 WeChat の 4096 バイト上限の中で「まだ内容が読める」規模だからである。
// これより大きくしても、切断がより後ろで起きるだけである。
const MaxDigestBatchSize = 500

// NotificationDelivery は配信タスク 1 件で、描画に必要なチャネル設定とイベントのスナップショットを含む。
type NotificationDelivery struct {
	ID            int64           `json:"id"`
	EventID       int64           `json:"event_id"`
	ChannelID     int64           `json:"channel_id"`
	State         string          `json:"state"`
	Attempts      int             `json:"attempts"`
	NextAttemptAt time.Time       `json:"next_attempt_at"`
	LastError     string          `json:"last_error"`
	BatchID       *int64          `json:"batch_id,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	SentAt        *time.Time      `json:"sent_at,omitempty"`
	Snapshot      json.RawMessage `json:"snapshot,omitempty"`
	// 一緒に読む描画コンテキスト。JSON には入れない（DTO は server 層が組み立てる）。
	Channel *NotificationChannel `json:"-"`
	// FindingID と EventKind はイベントから渡す。履歴一覧から脆弱性の詳細へ直接移るためである。
	FindingID int64  `json:"finding_id,string"`
	EventKind string `json:"event_kind"`
	// ChannelName と ChannelKind は一覧表示用の重複フィールドで、画面の 2 度目の照会を省く。
	ChannelName string `json:"channel_name"`
	ChannelKind string `json:"channel_kind"`
}

const notificationDeliveryCols = `d.id, d.event_id, d.channel_id, d.state, d.attempts, d.next_attempt_at,
       d.last_error, d.batch_id, d.created_at, d.sent_at`

// joinedDeliveryQuery は配信行の統一した読み取り形である。配信、イベントのスナップショット、チャネル設定。
// メッセージを 1 通描画するには 3 つとも要る。分けて読むと往復が 3 回になる。
const joinedDeliveryQuery = `SELECT ` + notificationDeliveryCols + `,
       e.snapshot, e.kind, e.finding_id,
       c.id, c.name, c.kind, c.enabled, c.config, c.mode, c.filter, c.rate_per_min
FROM notification_deliveries d
JOIN notification_events e ON e.id = d.event_id
JOIN notification_channels c ON c.id = d.channel_id`

func scanNotificationDelivery(sc interface{ Scan(...any) error }) (*NotificationDelivery, error) {
	var (
		dl        NotificationDelivery
		lastErr   sql.NullString
		batchID   sql.NullInt64
		sentAt    sql.NullTime
		snapshot  []byte
		eventKind string
		channel   NotificationChannel
		chEnabled bool
	)
	if err := sc.Scan(&dl.ID, &dl.EventID, &dl.ChannelID, &dl.State, &dl.Attempts, &dl.NextAttemptAt,
		&lastErr, &batchID, &dl.CreatedAt, &sentAt,
		&snapshot, &eventKind, &dl.FindingID,
		&channel.ID, &channel.Name, &channel.Kind, &chEnabled, &channel.Config, &channel.Mode, &channel.Filter, &channel.RatePerMin); err != nil {
		return nil, err
	}
	dl.LastError = lastErr.String
	if batchID.Valid {
		dl.BatchID = &batchID.Int64
	}
	if sentAt.Valid {
		dl.SentAt = &sentAt.Time
	}
	dl.Snapshot = json.RawMessage(snapshot)
	dl.EventKind = eventKind
	dl.ChannelName = channel.Name
	dl.ChannelKind = channel.Kind
	channel.Enabled = &chEnabled
	dl.Channel = &channel
	return &dl, nil
}

// claimQuery は取得 1 回を表す。先に sel で候補を選んでロックし、sending にして
// リースを延ばす。sel の中の lease の位置は、呼び出し側が $n のプレースホルダを置き、自分で引数を渡す。
type claimQuery struct {
	sql  string
	args []any
}

// ClaimRealtimeDeliveries は、あるチャネルの期限が来たリアルタイム配信を、最大 limit 件取得する。
//
// 意図的に**チャネル 1 つずつ**取得する。「全体から一括で取って、送るものだけ選ぶ」にはしない。レート制限の門は配信エンジンがチャネルごとに
// 持つ。先にこのチャネルが今ラウンドであと何件送れるかを知り、同じ数だけ取る。そうしないとレート制限が
// 再試行回数を消費する。逆に先に取って捨てると、レート制限で止めた行はすでに attempts を 1 回数えており、
// 3 回の予算が待ちだけで尽き、最後は failed になる。
//
// 条件には「リース期限切れの sending」を含む。それがクラッシュ後の自己回復の着地点である。lease は 1 回の
// 配信の最悪所要時間（チャネルの HTTP クライアントのタイムアウトは 15 秒）より明らかに大きくなければならない。そうでないと、同じ行を 2 つの dispatcher が
// 同時に配信する。無効化したチャネルもここで止める。無効化は既存の配信を skipped にしているが、
// ここでもう一度止めて、無効化と取得が同時に走ったときの取りこぼしを避ける。
func (d *DB) ClaimRealtimeDeliveries(ctx context.Context, channelID int64, limit int, lease time.Duration) ([]*NotificationDelivery, error) {
	if limit <= 0 {
		return nil, nil
	}
	return d.claimDeliveries(ctx, lease, claimQuery{
		sql: `SELECT dd.id FROM notification_deliveries dd
JOIN notification_channels c ON c.id = dd.channel_id
WHERE dd.channel_id = $1 AND dd.state IN ($2,$3) AND dd.next_attempt_at <= now()
  AND c.enabled AND c.mode = $4
ORDER BY dd.next_attempt_at, dd.id
FOR UPDATE OF dd SKIP LOCKED
LIMIT $5`,
		args: []any{channelID, NotifyStatePending, NotifyStateSending, NotifyModeRealtime, limit},
	}, nil)
}

// DigestBatchDue は、そのチャネルが期限の来たバッチを溜めているかを報告する。未配信があり、かつ**最も古い 1 件**の
// 経過時間がまとめ周期に達している。
//
// 判定は壁時計ではなく、最も古い配信の経過時間による。作りたてのチャネルが、正時への揃えだけで
// 1 件だけの「まとめ」をすぐ出さず、長く滞留したバッチがもう 1 周むだに待たない。
//
// ClaimDigestBatch と分けるのは意味が違うからである。この関数は「送るべきか」だけに答える。
// 取得はそのチャネルの未配信を**すべて**持っていく（経過時間が足りないものも含む）。そうしないと 1 周期が
// 複数のメッセージに分かれ、まとめの意味が無くなる。
func (d *DB) DigestBatchDue(ctx context.Context, channelID int64, minAge time.Duration) (bool, error) {
	var due bool
	err := d.QueryRowContext(ctx, `SELECT EXISTS (
  SELECT 1 FROM notification_deliveries d
  JOIN notification_channels c ON c.id = d.channel_id
  WHERE d.channel_id = $1 AND d.state IN ($2,$3) AND c.enabled
  GROUP BY d.channel_id
  HAVING min(d.created_at) <= now() - make_interval(secs => $4)
)`, channelID, NotifyStatePending, NotifyStateSending, int64(minAge.Seconds())).Scan(&due)
	return due, err
}

// ClaimDigestBatch は、あるチャネルの今期限が来た未配信を、1 つのまとめバッチとして取得する。
// 1 バッチは最大 MaxDigestBatchSize 件である。
//
// 同じバッチの配信は batch_id を共有し、集合の最小 id をバッチ番号にする（安定し、読め、
// 追加のシーケンスは要らない）。再試行では COALESCE で元のバッチ番号を残し、「この N 件は一緒に送った」が
// 何度再試行しても成り立つようにする。
//
// ランダムではなく id の昇順で先頭 N 件を取る。最も早くできた配信を先に送り、滞留しても
// 「新しい脆弱性が先に出て、古い脆弱性が永遠に後ろに残る」飢餓を起こさない。
func (d *DB) ClaimDigestBatch(ctx context.Context, channelID int64, limit int, lease time.Duration) ([]*NotificationDelivery, error) {
	if limit <= 0 {
		return nil, nil
	}
	// limit は**メモリ上の上限**で、呼び出し側は MaxDigestBatchSize を渡す。ここでさらに挟み、
	// より大きい値が入るのを防ぐ。
	//
	// 「レート制限の枠」をバッチサイズとして受けないのは意図的である。レート制限の単位はメッセージ件数である。1 バッチは
	// 1 通だけ送り、トークンを 1 つ消費する。差し引くのは server 層の takeTokens である。これは「1 バッチに何件の
	// 脆弱性を入れるか」とは単位が違う。以前は rate_per_min を digest に効かせるため、毎ラウンドの
	// 要求予算をバッチサイズとして渡していた。その結果、rate=20/min のチャネルは 1 バッチに脆弱性を 1 件しか入れず、
	// digest はまとめの文面が付いたリアルタイム通知へ退化した。レート制限を変えるなら takeTokens の want を変える。
	// ここは動かさない。
	if limit > MaxDigestBatchSize {
		limit = MaxDigestBatchSize
	}
	out, err := d.claimDeliveries(ctx, lease, claimQuery{
		sql: `SELECT dd.id FROM notification_deliveries dd
JOIN notification_channels c ON c.id = dd.channel_id
WHERE dd.channel_id = $1 AND dd.state IN ($2,$3) AND dd.next_attempt_at <= now() AND c.enabled
ORDER BY dd.id
FOR UPDATE OF dd SKIP LOCKED
LIMIT $4`,
		args: []any{channelID, NotifyStatePending, NotifyStateSending, limit},
	}, func(tx *sql.Tx, ids []int64) error {
		batchID := ids[0]
		for _, id := range ids {
			if id < batchID {
				batchID = id
			}
		}
		ph, idArgs := placeholders(2, ids)
		_, err := tx.ExecContext(ctx, `UPDATE notification_deliveries SET batch_id = COALESCE(batch_id, $1)
WHERE id IN (`+ph+`)`, append([]any{batchID}, idArgs...)...)
		return err
	})
	return out, err
}

// claimDeliveries は「選択、sending にしてリースを延ばす、完全な行を読む」を、すべて 1 つのトランザクションで行う。
// postClaim は任意の追加手順である（まとめバッチはここで batch_id を書く）。
func (d *DB) claimDeliveries(ctx context.Context, lease time.Duration, cq claimQuery, postClaim func(*sql.Tx, []int64) error) ([]*NotificationDelivery, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // コミット成功後は no-op

	ids, err := selectForClaim(ctx, tx, cq.sql, cq.args...)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, tx.Commit()
	}
	// sending にし、next_attempt_at を未来へ動かす。この未来の時刻がリース期限である。
	// 「リース期限内」と「再試行時刻前」は同じ条件で表せる。列は増やさない。
	ph, idArgs := placeholders(3, ids)
	if _, err := tx.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, attempts=attempts+1, next_attempt_at=now()+make_interval(secs => $2)
WHERE id IN (`+ph+`)`,
		append([]any{NotifyStateSending, lease.Seconds()}, idArgs...)...); err != nil {
		return nil, err
	}
	if postClaim != nil {
		if err := postClaim(tx, ids); err != nil {
			return nil, err
		}
	}
	out, err := loadDeliveriesTx(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

func selectForClaim(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func loadDeliveriesTx(ctx context.Context, tx *sql.Tx, ids []int64) ([]*NotificationDelivery, error) {
	ph, args := placeholders(1, ids)
	rows, err := tx.QueryContext(ctx, joinedDeliveryQuery+` WHERE d.id IN (`+ph+`) ORDER BY d.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationDelivery{}
	for rows.Next() {
		dl, err := scanNotificationDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, dl)
	}
	return out, rows.Err()
}

// MarkDeliveriesSent は配信の一群を配信済みにする。
func (d *DB) MarkDeliveriesSent(ctx context.Context, ids []int64) error {
	ph, args := placeholders(2, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, sent_at=now(), last_error='' WHERE id IN (`+ph+`)`, append([]any{NotifyStateSent}, args...)...)
	return err
}

// RescheduleDeliveries は配信の一群を pending に戻し、再試行時刻を後ろへずらす。
//
// 新しい中間状態を足さず pending に戻すのは、「残り何回か」を 1 か所だけで
// 表すためである（MaxNotifyAttempts）。状態機械の分岐が再試行方針とともに膨らまない。
func (d *DB) RescheduleDeliveries(ctx context.Context, ids []int64, delay time.Duration, errMsg string) error {
	ph, args := placeholders(4, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, next_attempt_at=now()+make_interval(secs => $2), last_error=$3
WHERE id IN (`+ph+`)`,
		append([]any{NotifyStatePending, delay.Seconds(), truncateNotifyError(errMsg)}, args...)...)
	return err
}

// DeferDeliveries は配信の一群を pending に戻してすぐ再取得できるようにし、**取得時に数えた試行を取り消す**。
//
// 用途は 1 つだけである。まとめメッセージをチャネルの長さ上限で分割して送るとき、この通に入らなかった項目は次のバッチへ残す。
// それは失敗ではないので、再試行予算を消費してはならない。取得時に attempts は楽観的に +1 済みなので、
// ここで戻す。戻さないと、500 件の滞留が 1 区切り 20 件で 25 区切りになり、
// 末尾の項目は 3 区切り目で MaxNotifyAttempts により failed になる。それらは一度もエラーを出していない。
//
// GREATEST(...,0) は、「手動再送で attempts を 0 にしたあとにここへ来る」場合を受ける。
// カウントを負にしない。
func (d *DB) DeferDeliveries(ctx context.Context, ids []int64, reason string) error {
	ph, args := placeholders(3, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, attempts=GREATEST(attempts-1, 0), next_attempt_at=now(), last_error=$2
WHERE id IN (`+ph+`)`,
		append([]any{NotifyStatePending, truncateNotifyError(reason)}, args...)...)
	return err
}

// FailDeliveries は配信の一群を最終失敗にし、配信履歴での手動再送を待つ。
func (d *DB) FailDeliveries(ctx context.Context, ids []int64, errMsg string) error {
	// プレースホルダは $3 から始まる。$1 は state、$2 は last_error である。
	ph, args := placeholders(3, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries SET state=$1, last_error=$2 WHERE id IN (`+ph+`)`,
		append([]any{NotifyStateFailed, truncateNotifyError(errMsg)}, args...)...)
	return err
}

// RetryNotificationDelivery は配信を 1 件手動で再送する。pending に戻し、再試行回数を 0 にし、
// すぐに期限切れにする。回数を消すのは意図的である。人が「再送」を押すのは、前の失敗原因が処理済みという意味で、
// 古い回数で制限する理由は無い。
func (d *DB) RetryNotificationDelivery(ctx context.Context, id int64) error {
	res, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$2, attempts=0, next_attempt_at=now(), last_error=''
WHERE id=$1 AND state IN ($3,$4)`, id, NotifyStatePending, NotifyStateFailed, NotifyStateSkipped)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("配信 %d は存在しないか、現在の状態では再送できません", id)
	}
	return nil
}

// NotificationDeliveryFilter は配信履歴の照会条件である。
type NotificationDeliveryFilter struct {
	ChannelID int64
	State     string
	EventKind string
}

func (f NotificationDeliveryFilter) where() (string, []any) {
	var conds []string
	var args []any
	if f.ChannelID > 0 {
		args = append(args, f.ChannelID)
		conds = append(conds, fmt.Sprintf("d.channel_id=$%d", len(args)))
	}
	if f.State != "" {
		args = append(args, f.State)
		conds = append(conds, fmt.Sprintf("d.state=$%d", len(args)))
	}
	if f.EventKind != "" {
		args = append(args, f.EventKind)
		conds = append(conds, fmt.Sprintf("e.kind=$%d", len(args)))
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// ListNotificationDeliveries は配信履歴をページで返す。新しいものが先である。
func (d *DB) ListNotificationDeliveries(ctx context.Context, f NotificationDeliveryFilter, page, pageSize int) ([]*NotificationDelivery, int, error) {
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 || pageSize > 200 {
		pageSize = 50
	}
	where, args := f.where()

	var total int
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM notification_deliveries d
JOIN notification_events e ON e.id = d.event_id`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	q := fmt.Sprintf("%s%s ORDER BY d.id DESC LIMIT $%d OFFSET $%d",
		joinedDeliveryQuery, where, len(args)+1, len(args)+2)
	rows, err := d.QueryContext(ctx, q, append(args, pageSize, (page-1)*pageSize)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []*NotificationDelivery{}
	for rows.Next() {
		dl, err := scanNotificationDelivery(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, dl)
	}
	return out, total, rows.Err()
}

// truncateNotifyError はエラー情報を、列が受け付ける長さまで切る。チャネルの応答本文は長いことがある
// （汎用 Webhook が自前のサービスに当たったときが特に長い）。切らないと履歴一覧のペイロードが膨らむ。
func truncateNotifyError(msg string) string {
	const max = 500
	if len(msg) <= max {
		return msg
	}
	// 文字境界まで戻し、UTF-8 の途中で切って画面が文字化けするのを避ける。
	cut := max
	for cut > 0 && !isUTF8Start(msg[cut]) {
		cut--
	}
	return msg[:cut] + "…"
}

func isUTF8Start(b byte) bool { return b&0xC0 != 0x80 }

// placeholders は start から始まる $n のプレースホルダ列と対応する引数を作り、IN (...) に使う。
// 例は start=3, ids=[7,8] で "$3,$4" と [7,8] になる。
func placeholders(start int, ids []int64) (string, []any) {
	ph := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		ph = append(ph, fmt.Sprintf("$%d", start+i))
		args = append(args, id)
	}
	return strings.Join(ph, ","), args
}
