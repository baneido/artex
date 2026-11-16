package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/notify"
)

// 全体設定のキー（settings のキー値テーブルにある。テーブルは作らない）。
const (
	// settingNotifyEnabled は通知の全体スイッチ。デフォルトはオン。メンテナンス中に一括で止めるためのもので、
	// 機能を有効にする条件ではない。本当の有効条件は「チャネルを設定したか」である。
	settingNotifyEnabled = "notify_enabled"
	// settingNotifyPublicBaseURL は、脆弱性詳細への戻りリンクを作る外部のアクセスアドレスである
	// （例 https://artex.example.com）。空ならメッセージに戻りリンクのボタンを付けない。
	// プロジェクトに再利用できる外部アドレスの設定がないので、ここで 1 項目足す。
	settingNotifyPublicBaseURL = "notify_public_base_url"
	// settingNotifyDigestMinutes はまとめモードの周期（分）。
	settingNotifyDigestMinutes = "notify_digest_interval_min"
)

const (
	// notifyTick は配信エンジンの確認間隔。3 秒はこのエンジンの即時性の上限であり、
	// 「脆弱性の DB 登録」から「メッセージが IM に着く」までの主な遅延でもある。
	notifyTick = 3 * time.Second
	// notifyLease は配信を受け取るときのリース時間。1 回の配信の最悪時間より十分長くする
	// （notify パッケージの HTTP クライアントのタイムアウトは 15 秒）。そうしないと、同じ行を 2 つの
	// dispatcher が同時に配信する。
	notifyLease = 3 * time.Minute
	// notifyFanOutPerTick は 1 ラウンドで分配するイベント数の上限。チャネルを初めて有効にしたとき、
	// 過去の滞留を一度に全部配信タスクへ展開しないようにする。
	notifyFanOutPerTick = 200
	// notifyDefaultDigestMinutes はまとめ周期のデフォルト。
	notifyDefaultDigestMinutes = 30
	// notifyUnlimitedBurstPerTick は、チャネルにレート制限がないときの 1 ラウンドの配信上限。
	// 「1 チャネルを無制限にし、1 回のスキャンで脆弱性が千件超出る」場合に、
	// 1 ラウンドのループが長いブロックになるのを防ぐ。
	notifyUnlimitedBurstPerTick = 50
	// notifyMaxSendsPerChannelPerTick は、1 チャネルが 1 ラウンドで配信する最大件数。
	//
	// この上限はリース時間から逆算する。受け取り時に行へ付けるのはリース（notifyLease = 3 分）である。
	// 1 ラウンドで直列に配信する件数が多く、最悪時間がリースを超えると、後ろの数件は送り終わる前にリースが切れる。
	// 単一プロセスなら問題にならない（Run は 1 つの goroutine が直列に走り、tick は再入しない）。2 つの
	// プロセスが同じ DB につながると、相手はリース切れの行を取り直して重複送信し、さらに
	// attempts を二重に増やし、元のプロセスがまだ配信中なのに失敗と判定する。
	//
	// 値。3 分のリース / 30 秒の 1 回タイムアウト = 6 はリースをちょうど使い切る、余裕ゼロで、
	// 取れない。5 にすると最悪時間 150 秒で 30 秒の余裕が残る。この関係は
	// TestNotifyTickBudgetFitsWithinLease が固定する。notifyLease、
	// notifySendTimeout、またはこの値のどれを変えても、そのアサーションは失敗する。
	notifyMaxSendsPerChannelPerTick = 5
	// notifySendTimeout は 1 回の配信のタイムアウト。上の定数の値もこれで決まる。
	// 2 つの積は notifyLease を超えてはならない。TestNotifyTickBudgetFitsWithinLease を参照。
	notifySendTimeout = 30 * time.Second
)

// notifyBackoff は失敗の再試行のバックオフ列。添字は試した回数。
// 3 回（初回を含む）は db.MaxNotifyAttempts に対応する。両方一緒に変える。
var notifyBackoff = []time.Duration{
	time.Second,
	5 * time.Second,
	30 * time.Second,
}

// Notifier は脆弱性通知の配信エンジンである。
//
// Scheduler と並んで、独立した goroutine として動く（server.New を参照）。Scheduler の tick は使わない。
// 通知の即時性（3 秒）とトリガーの業務の間隔は違う。
// 失敗も互いに引きずらない。通知が止まっても agent のトリガーには影響しない。
type Notifier struct {
	s  *Server
	pg *db.DB

	// mu は buckets を守る。チャネル数は少なく競合も低い。ミューテックス 1 つで足り、
	// これ以上細かい構造は要らない。
	mu      sync.Mutex
	buckets map[int64]*notifyBucket
}

// notifyBucket は 1 チャネルのトークンバケットである。
//
// 「毎分カウントしてゼロに戻す」スライディングウィンドウではなくトークンバケットを使う。前者の境界の影響が悪い。
// ウィンドウの終わりに 20 件送り、次の瞬間にまた 20 件送ると、プラットフォームからは 1 秒で 40 件になり、
// レート制限される。トークンバケットは一定速度で補充するので、この突発を自然に避ける。
type notifyBucket struct {
	tokens   float64
	lastFill time.Time
}

func newNotifier(s *Server) *Notifier {
	return &Notifier{s: s, pg: s.m.pg, buckets: map[int64]*notifyBucket{}}
}

// Run は ctx が終わるまでループする。server.New が 1 回起動する。
func (n *Notifier) Run(ctx context.Context) {
	if n.pg == nil {
		return
	}
	t := time.NewTicker(notifyTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n.step(ctx)
		}
	}
}

// step は 1 ラウンド走る。先に新しいイベントを分配し、次に期限の来たタスクを配信する。
//
// どのステップが失敗してもログを残すだけで、ループは止めない。通知システムの故障をプロセスレベルの問題にしてはならない。
// tick はそれぞれ独立している。次のラウンドが自然に再試行する。
func (n *Notifier) step(ctx context.Context) {
	if !n.enabled() {
		return
	}
	if _, _, err := n.pg.FanOutPendingEvents(ctx, notifyFanOutPerTick); err != nil {
		log.Printf("[notify] イベントの振り分けに失敗しました: %v", err)
		return
	}
	channels, err := n.pg.ListNotificationChannels(ctx)
	if err != nil {
		log.Printf("[notify] チャネルの読み取りに失敗しました: %v", err)
		return
	}
	baseURL := n.publicBaseURL()
	for _, ch := range channels {
		if !ch.IsEnabled() {
			continue
		}
		// トークンバケットの単位はメッセージ件数（HTTP リクエスト数と同じ）であり、脆弱性の件数ではない。
		// 即時モードでは両者が同じである（脆弱性 1 件がメッセージ 1 件）。まとめモードでは 1 バッチの脆弱性が
		// メッセージ 1 件になるので、トークンは 1 つだけ消費する。
		//
		// どちらのモードも、先にトークンバケットを聞き、枠の分だけ受け取る。順序は逆にできない。逆にすると、レート制限で止まった
		// 配信がすでに再試行回数を消費している。
		now := time.Now()
		if ch.Mode == db.NotifyModeDigest {
			tokens, claimLimit := digestTickPlan()
			if n.takeTokens(ch.ID, ch.RatePerMin, tokens, now) <= 0 {
				continue
			}
			n.stepDigest(ctx, ch, claimLimit, baseURL)
			continue
		}
		allow := n.takeTokens(ch.ID, ch.RatePerMin, notifyMaxSendsPerChannelPerTick, now)
		if allow <= 0 {
			continue
		}
		n.stepRealtime(ctx, ch, allow, baseURL)
	}
}

// digestTickPlan は、まとめチャネルのこのラウンドのトークン消費とバッチサイズの上限を返す。
//
// 2 つの戻り値は別の次元である。関数を分ける理由はここにある。
//
//   - tokens はメッセージ件数。1 バッチの脆弱性はメッセージ 1 件、HTTP リクエスト 1 回なので、常に 1。
//     rate_per_min はそのため digest でも効く（1 分あたりのまとめメッセージの上限）。
//   - claimLimit はこのバッチに入る脆弱性の最大件数。メモリの上限だけで決まり、リクエスト予算とは無関係。
//
// 以前は rate_per_min を digest でも効かせるため、各ラウンドのリクエスト予算
// （notifyMaxSendsPerChannelPerTick。リースから逆算する）をそのままバッチサイズとして渡していた。
// 結果、rate_per_min=20 のチャネルは 3 秒の tick でトークンが 1 つしか補充されず、まとめメッセージ 1 件に
// 脆弱性が 1 件しか入らない。digest が「まとめの文面が付いた即時通知」に退化し、読み手は
// 「直近 30 分の新規脆弱性 1 件」を連続で受け取る。db.MaxDigestBatchSize には届かない。
//
// この症状はエンドツーエンドのテストでは見つかりにくい（既存の用例は十分大きい limit を
// stepDigest に手で渡しており、step の枠計算を迂回している）。判断をここに集め、
// TestDigestTickPlanDecouplesBatchSizeFromSendBudget が直接固定する。
func digestTickPlan() (tokens, claimLimit int) {
	return 1, db.MaxDigestBatchSize
}

// stepRealtime は、あるチャネルの即時タスクを受け取って配信する。脆弱性 1 件がメッセージ 1 件。
func (n *Notifier) stepRealtime(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	deliveries, err := n.pg.ClaimRealtimeDeliveries(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] 即時配信の取得に失敗しました channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf("チャネル種別 %q は登録されていません", ch.Kind))
		return
	}
	for _, dl := range deliveries {
		msg, err := n.renderSingle(ctx, dl, baseURL)
		if err != nil {
			// 描画の失敗はローカルデータの問題で、再試行しても良くならない。
			_ = n.pg.FailDeliveries(ctx, []int64{dl.ID}, err.Error())
			continue
		}
		n.send(ctx, channel, cfg, msg, []*db.NotificationDelivery{dl})
	}
}

// stepDigest は、バッチの期限が来たら、あるチャネルの未送信配信を 1 件のメッセージにまとめて送る。
func (n *Notifier) stepDigest(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	window := n.digestInterval()
	due, err := n.pg.DigestBatchDue(ctx, ch.ID, window)
	if err != nil {
		log.Printf("[notify] まとめバッチの判定に失敗しました channel=%d: %v", ch.ID, err)
		return
	}
	if !due {
		return
	}
	deliveries, err := n.pg.ClaimDigestBatch(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] まとめバッチの取得に失敗しました channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf("チャネル種別 %q は登録されていません", ch.Kind))
		return
	}
	msg, included, err := n.renderBatch(ctx, deliveries, baseURL, int(window.Minutes()))
	if err != nil {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), err.Error())
		return
	}
	// スナップショットが壊れてメッセージに入らなかった配信は、明示的に失敗とする。そうしないと included の
	// 外に残り、メッセージにも失敗リストにも入らない。送信成功時のその後の一括マークから漏れ、
	// sending のままリース切れまで繰り返し受け取られる。
	if skipped := excludeDeliveries(deliveries, included); len(skipped) > 0 {
		reason := "イベントのスナップショットを解析できないため、この脆弱性をメッセージに変換できません"
		if fErr := n.pg.FailDeliveries(ctx, deliveryIDs(skipped), reason); fErr != nil {
			log.Printf("[notify] 壊れたスナップショットの配信の記録に失敗しました channel=%s ids=%v: %v", ch.Kind, deliveryIDs(skipped), fErr)
		}
		log.Printf("[notify] スナップショットを解釈できない配信を %d 件スキップしました channel=%d", len(skipped), ch.ID)
	}
	// メッセージに入ったものだけを send に渡す。included[i] と msg.Items[i] は厳密に対応する。
	// send はこの対応で、「チャネルが前の K 件を載せたと報告した」結果を正しい配信行へ落とす。
	n.send(ctx, channel, cfg, msg, included)
}

// send は配信し、結果に応じて状態を進める。
//
// 同じバッチの配信（まとめモードでは数十件になり得る）は 1 つの送信結果を共有する。届くか、バッチ全体を再試行するか。
// 1 件ずつの再試行はしない。まとめメッセージは 1 件なので、一部だけ再送するとバッチの意味が崩れる。
//
// 唯一の例外は、チャネルの長さ上限による分割である。チャネルが実際に載せたのは前の K 件だけと報告したら、
// K+1 件目以降は次のバッチへ残す。一緒に成功とマークしない。切り捨てられた
// 脆弱性はメッセージにも失敗リストにもなく、消える。
func (n *Notifier) send(ctx context.Context, channel notify.Channel, cfg map[string]any, msg notify.Message, deliveries []*db.NotificationDelivery) {
	// 1 回の配信に上限を付ける。あるチャネルが止まって、このラウンドの残りのチャネルを全部引きずらないようにする。
	sendCtx, cancel := context.WithTimeout(ctx, notifySendTimeout)
	defer cancel()
	delivered, err := channel.Send(sendCtx, cfg, msg)
	if err == nil && delivered > 0 {
		if delivered > len(deliveries) {
			// チャネルが報告する件数は配信数を超えられない。超えたら描画層の計算が間違っている。
			// 全部届いたものとして扱い、問題は記録する。記録を壊すよりよい。
			log.Printf("[notify] チャネルが報告した到達件数 %d が配信数 %d を超えています channel=%s。すべて到達したものとして扱います",
				delivered, len(deliveries), channel.Kind())
			delivered = len(deliveries)
		}
		sent, rest := deliveries[:delivered], deliveries[delivered:]
		if err := n.pg.MarkDeliveriesSent(ctx, deliveryIDs(sent)); err != nil {
			log.Printf("[notify] 到達済みの記録に失敗しました channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(sent), err)
		}
		if len(rest) > 0 {
			// このメッセージはチャネルの長さ上限に達した。残りはすぐキューへ戻し、次の tick で続ける。
			// RescheduleDeliveries ではなく DeferDeliveries を使う。これは失敗ではない。
			// 再試行の予算を消費しない（受け取り時に楽観的に +1 しており、そこで戻す）。
			if err := n.pg.DeferDeliveries(ctx, deliveryIDs(rest),
				fmt.Sprintf("このメッセージはチャネルの長さの上限に達したため、先頭 %d 件だけを配信し、残りは次のバッチに回します", delivered)); err != nil {
				log.Printf("[notify] 分割した続きのキュー投入に失敗しました channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(rest), err)
			}
		}
		return
	}
	if err == nil {
		// チャネルはエラーも、何件届いたかも報告していない。失敗として扱う（バックオフへ進む）。
		// この配信が繰り返し受け取られ、永久にマークが付かないのを避ける。
		err = fmt.Errorf("チャネルが配信件数を報告しませんでした（delivered=%d）", delivered)
	}

	// 失敗の扱いは 1 件ずつ決める。バッチ全体の最大試行回数では判断しない。
	//
	// 以前は `if maxAttempts(deliveries) >= MaxNotifyAttempts` でバッチ全体を失敗にしていた。バッチ内の
	// 各件の試行回数は同じではない。すでに 2 回再試行した古い配信（attempts=2）が、同じバッチの
	// 新しい配信（attempts=1）を一緒に failed へ引き込む。新しい脆弱性は再試行を 1 回も使わず永久に消え、
	// 「古い行が新しい行を巻き添えにしない」という当初の意図と逆になる。
	permanent := notify.IsPermanent(err)
	var failIDs, exhaustedIDs []int64
	byDelay := map[time.Duration][]int64{}
	for _, dl := range deliveries {
		switch {
		case permanent:
			failIDs = append(failIDs, dl.ID)
		case dl.Attempts >= db.MaxNotifyAttempts:
			exhaustedIDs = append(exhaustedIDs, dl.ID)
		default:
			delay := notifyBackoff[min(dl.Attempts, len(notifyBackoff)-1)]
			byDelay[delay] = append(byDelay[delay], dl.ID)
		}
	}

	if len(failIDs) > 0 {
		if fErr := n.pg.FailDeliveries(ctx, failIDs, err.Error()); fErr != nil {
			log.Printf("[notify] 失敗状態の記録でエラー channel=%s ids=%v: %v", channel.Kind(), failIDs, fErr)
		}
	}
	if len(exhaustedIDs) > 0 {
		reason := fmt.Sprintf("%d 回リトライしても失敗しました: %s", db.MaxNotifyAttempts, err)
		if fErr := n.pg.FailDeliveries(ctx, exhaustedIDs, reason); fErr != nil {
			log.Printf("[notify] 失敗状態の記録でエラー channel=%s ids=%v: %v", channel.Kind(), exhaustedIDs, fErr)
		}
	}
	// 遅延でグループ化して並べ直す。バックオフは 3 段だけなので、グループ数はもともと小さい。1 件ずつ
	// UPDATE を出さない（500 件のバッチが 500 回の往復になる）。
	for delay, group := range byDelay {
		if rErr := n.pg.RescheduleDeliveries(ctx, group, delay, err.Error()); rErr != nil {
			log.Printf("[notify] 配信の再スケジュールに失敗しました channel=%s ids=%v: %v", channel.Kind(), group, rErr)
		}
	}
	if len(failIDs)+len(exhaustedIDs) > 0 {
		log.Printf("[notify] 配信に失敗しました channel=%d kind=%s 恒久失敗=%d 再試行尽きた=%d 再試行待ち=%d: %s",
			deliveries[0].ChannelID, channel.Kind(), len(failIDs), len(exhaustedIDs), len(byDelay), err)
	}
}

// excludeDeliveries は、all のうち keep にないものを返す（ポインタの同一性で比べる）。
// 「メッセージに入らなかった」配信を見つける。明示的に扱う必要があり、グレーのまま残さない。
func excludeDeliveries(all, keep []*db.NotificationDelivery) []*db.NotificationDelivery {
	inKeep := make(map[*db.NotificationDelivery]bool, len(keep))
	for _, dl := range keep {
		inKeep[dl] = true
	}
	var out []*db.NotificationDelivery
	for _, dl := range all {
		if !inKeep[dl] {
			out = append(out, dl)
		}
	}
	return out
}

// adapt はチャネル実装を取り、設定を解釈する。
// ok=false は種別が未登録という意味で、配信は無限に再試行せず、すぐ失敗とする。
func (n *Notifier) adapt(ch *db.NotificationChannel) (notify.Channel, map[string]any, bool) {
	channel, ok := notify.Get(ch.Kind)
	if !ok {
		return nil, nil, false
	}
	var cfg map[string]any
	if len(ch.Config) > 0 {
		// 設定の解釈に失敗したら空の map を渡す。チャネル自身の Validate が「どのフィールドが欠けているか」を出す。
		// そのエラーの方が、JSON の解釈エラーよりユーザーの修正の手がかりになる。
		_ = json.Unmarshal(ch.Config, &cfg)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	return channel, cfg, true
}

// renderSingle は脆弱性 1 件のメッセージを描画する。
func (n *Notifier) renderSingle(ctx context.Context, dl *db.NotificationDelivery, baseURL string) (notify.Message, error) {
	snap, err := parseSnapshot(dl)
	if err != nil {
		return notify.Message{}, err
	}
	item, err := n.itemFor(ctx, snap, baseURL)
	if err != nil {
		return notify.Message{}, err
	}
	return notify.Message{Items: []notify.Item{item}, HomeURL: baseURL}, nil
}

// renderBatch はまとめメッセージを描画する。スナップショットは 1 件ずつ解釈する。1 件壊れてもその 1 件を飛ばすだけで、
// バッチ全体のまとめを消さない。
//
// included の戻り値と msg.Items は厳密に 1 対 1 である（i 番目の配信 ↔ i 番目の項目）。
// この対応は必須である。呼び出し側は「チャネルが前の K 件を載せたと報告した」に従い、前の K 件の配信を
// 到達済みにする。ここで壊れたスナップショットを飛ばしながら included から外さないと、
// 添字がずれる。失敗すべき壊れた項目が到達済みになり、良い項目が未到達と誤判定される。
// 壊れたものは呼び出し側が明示的に失敗にする。stepDigest を参照。
func (n *Notifier) renderBatch(ctx context.Context, deliveries []*db.NotificationDelivery, baseURL string, windowMinutes int) (notify.Message, []*db.NotificationDelivery, error) {
	items := make([]notify.Item, 0, len(deliveries))
	included := make([]*db.NotificationDelivery, 0, len(deliveries))
	for _, dl := range deliveries {
		snap, err := parseSnapshot(dl)
		if err != nil {
			// 壊れたスナップショットはメッセージにも included にも入れない。扱いは呼び出し側が行う
			// （明示的に失敗にする。「到達済み」に混ぜて通さない）。
			log.Printf("[notify] まとめバッチで解釈できないスナップショットをスキップしました delivery=%d: %v", dl.ID, err)
			continue
		}
		item, err := n.itemFor(ctx, snap, baseURL)
		if err != nil {
			return notify.Message{}, nil, err
		}
		items = append(items, item)
		included = append(included, dl)
	}
	if len(items) == 0 {
		return notify.Message{}, nil, fmt.Errorf("ダイジェストのバッチ %d 件の配信がすべて解析できません", len(deliveries))
	}
	return notify.Message{
		Items:         items,
		Batch:         true,
		WindowMinutes: windowMinutes,
		HomeURL:       baseURL,
	}, included, nil
}

// itemFor はイベントのスナップショットを送信待ちの項目に描画し、ついでに資産名と詳細への戻りリンクを解釈する。
func (n *Notifier) itemFor(ctx context.Context, snap notify.Snapshot, baseURL string) (notify.Item, error) {
	assets, err := n.pg.NotificationAssetNames(ctx, snap.AssetIDs)
	if err != nil {
		// 資産名の解釈失敗で通知を止めてはならない。名前が読めない方が、通知が届かないより軽い。
		// メッセージの資産が 1 行少ないだけである。
		log.Printf("[notify] 資産名の解決に失敗しました finding=%d: %v", snap.FindingID, err)
	}
	item := notify.Item{
		FindingID:  snap.FindingID,
		Name:       snap.Name,
		VulnClass:  snap.VulnClass,
		Severity:   snap.Severity,
		Summary:    snap.Summary,
		Assets:     assets,
		FromStatus: snap.FromStatus,
		ToStatus:   snap.ToStatus,
	}
	if baseURL != "" {
		// 詳細ページのルートは web/src/app/(main)/function/findings/detail/page.tsx。
		// query パラメータ id から脆弱性 id を読む。
		item.DetailURL = fmt.Sprintf("%s/function/findings/detail?id=%d", baseURL, snap.FindingID)
	}
	return item, nil
}

// takeTokens はチャネルのトークンバケットから最大 want 個のトークンを取り、実際に取れた数を返す。
//
// トークン 1 つ = メッセージ 1 件（HTTP リクエスト 1 回）。即時モードでは呼び出し側が必要な件数を渡す。
// まとめモードでは 1 バッチの脆弱性はメッセージ 1 件なので、1 を渡す。
//
// バケット容量はそのチャネルの毎分上限で、一定速度で補充する。ratePerMin<=0 はレート制限なし。
// 有限だが十分大きい値を返し、1 ラウンドのループが無限の滞留に引きずられないようにする。
//
// want という上限は必要である。これがないとバケットを全部空にするしかない。呼び出し側には各ラウンドの上限があり、
// 余分に取ったトークンは使えず、次の補充まで消える。溜めた突発容量に届かず、
// 「このラウンドに未送信の配信がない」ときも 1 回分引かれる。
func (n *Notifier) takeTokens(channelID int64, ratePerMin, want int, now time.Time) int {
	if want <= 0 {
		return 0
	}
	if ratePerMin <= 0 {
		return min(want, notifyUnlimitedBurstPerTick)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	b := n.buckets[channelID]
	if b == nil {
		b = &notifyBucket{tokens: float64(ratePerMin), lastFill: now}
		n.buckets[channelID] = b
	}
	// 経過した実時間で補充する。速度は毎秒 ratePerMin/60。
	if elapsed := now.Sub(b.lastFill).Seconds(); elapsed > 0 {
		b.tokens = minF(float64(ratePerMin), b.tokens+elapsed*float64(ratePerMin)/60)
		b.lastFill = now
	}
	// ごく小さい epsilon を足してから整数にする。トークン数は浮動小数の累積で、2 回に分けて満タンにすると
	// 0.5 + 0.5 が 0.9999999999 になり、そのまま int() すると 0 に切り捨てられる。
	// 数学上は満タンのバケットからトークンが取れない。1e-9 はトークン 1 つよりずっと小さく、本当の不足を見逃さない。
	take := min(int(b.tokens+1e-9), want)
	if take <= 0 {
		return 0
	}
	b.tokens -= float64(take)
	return take
}

// enabled は全体スイッチを読む。
func (n *Notifier) enabled() bool {
	return n.pg.GetBool(settingNotifyEnabled, true)
}

// publicBaseURL は戻りリンク用の外部アドレスを返し、末尾のスラッシュを除く。
func (n *Notifier) publicBaseURL() string {
	v, ok, err := n.pg.GetSetting(settingNotifyPublicBaseURL)
	if err != nil || !ok {
		return ""
	}
	return trimTrailingSlash(v)
}

// digestInterval はまとめ周期を返す。不正または未設定のときはデフォルトに戻る。
func (n *Notifier) digestInterval() time.Duration {
	v, ok, err := n.pg.GetSetting(settingNotifyDigestMinutes)
	if err != nil || !ok {
		return time.Duration(notifyDefaultDigestMinutes) * time.Minute
	}
	m := 0
	if _, err := fmt.Sscanf(v, "%d", &m); err != nil || m <= 0 {
		return time.Duration(notifyDefaultDigestMinutes) * time.Minute
	}
	return time.Duration(m) * time.Minute
}

// parseSnapshot は配信に対応するイベントのスナップショットを解釈する。
func parseSnapshot(dl *db.NotificationDelivery) (notify.Snapshot, error) {
	var snap notify.Snapshot
	if len(dl.Snapshot) == 0 {
		return snap, fmt.Errorf("配信 %d のイベントスナップショットが空です", dl.ID)
	}
	if err := json.Unmarshal(dl.Snapshot, &snap); err != nil {
		return snap, fmt.Errorf("配信 %d のイベントスナップショットの解析に失敗しました: %w", dl.ID, err)
	}
	if snap.Kind == "" {
		// イベント種別はイベント行を正とする。スナップショット側は古いバージョンが書いた可能性がある。
		snap.Kind = dl.EventKind
	}
	return snap, nil
}

func deliveryIDs(deliveries []*db.NotificationDelivery) []int64 {
	out := make([]int64, 0, len(deliveries))
	for _, dl := range deliveries {
		out = append(out, dl.ID)
	}
	return out
}

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
