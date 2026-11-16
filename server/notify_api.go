package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/notify"
)

// 本ファイルは通知機能の HTTP インターフェースである。ルートはすべて requireAuth の後にぶら下がる（Handler() を参照）。
// 他の管理インターフェースと同じである。

// notifyChannelDTO はチャネルの対外表現である。
//
// Config はマスク後の設定である。認証情報フィールドは notify.MaskedPrefix で始まる値に置き換わる。
// フロントがマスク値をそのまま送り返したときは「このフィールドは変更していない」の意味で、サーバーは DB の元の値を残す
// （notify.MergeConfig を参照）。
type notifyChannelDTO struct {
	ID         int64          `json:"id"`
	Name       string         `json:"name"`
	Kind       string         `json:"kind"`
	Enabled    bool           `json:"enabled"`
	Mode       string         `json:"mode"`
	Config     map[string]any `json:"config"`
	Filter     notify.Filter  `json:"filter"`
	RatePerMin int            `json:"rate_per_min"`
	CreatedAt  time.Time      `json:"created_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
	// SecretKeys は、どのフィールドが認証情報かをフロントに伝える。パスワード欄と「空欄なら変更しない」の表示に使う。
	// チャネル自身が宣言する（notify.Channel.SecretKeys）。フロントはチャネルの知識をハードコードしない。
	SecretKeys []string `json:"secret_keys"`
}

// notifyDeliveryDTO は配信履歴の対外表現である。
type notifyDeliveryDTO struct {
	ID          int64      `json:"id"`
	FindingID   int64      `json:"finding_id,string"`
	EventKind   string     `json:"event_kind"`
	ChannelID   int64      `json:"channel_id"`
	ChannelName string     `json:"channel_name"`
	ChannelKind string     `json:"channel_kind"`
	State       string     `json:"state"`
	Attempts    int        `json:"attempts"`
	LastError   string     `json:"last_error"`
	BatchID     *int64     `json:"batch_id,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	SentAt      *time.Time `json:"sent_at,omitempty"`
	NextAttempt time.Time  `json:"next_attempt_at"`
	// メッセージタイトルの要約。履歴一覧を展開しなくても、何を送ったか分かる。
	Title    string `json:"title"`
	Severity string `json:"severity"`
}

func toNotifyChannelDTO(ch *db.NotificationChannel) notifyChannelDTO {
	var cfg map[string]any
	if len(ch.Config) > 0 {
		_ = json.Unmarshal(ch.Config, &cfg)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	secrets := []string{}
	if c, ok := notify.Get(ch.Kind); ok {
		secrets = c.SecretKeys()
	}
	return notifyChannelDTO{
		ID:         ch.ID,
		Name:       ch.Name,
		Kind:       ch.Kind,
		Enabled:    ch.IsEnabled(),
		Mode:       ch.Mode,
		Config:     notify.MaskConfig(ch.Kind, cfg),
		Filter:     notify.ParseFilter(ch.Filter),
		RatePerMin: ch.RatePerMin,
		CreatedAt:  ch.CreatedAt,
		UpdatedAt:  ch.UpdatedAt,
		SecretKeys: secrets,
	}
}

func toNotifyDeliveryDTO(dl *db.NotificationDelivery) notifyDeliveryDTO {
	snap, _ := parseSnapshot(dl)
	dto := notifyDeliveryDTO{
		ID:          dl.ID,
		FindingID:   dl.FindingID,
		EventKind:   dl.EventKind,
		ChannelID:   dl.ChannelID,
		ChannelName: dl.ChannelName,
		ChannelKind: dl.ChannelKind,
		State:       dl.State,
		Attempts:    dl.Attempts,
		LastError:   dl.LastError,
		BatchID:     dl.BatchID,
		CreatedAt:   dl.CreatedAt,
		SentAt:      dl.SentAt,
		NextAttempt: dl.NextAttemptAt,
		Severity:    snap.Severity,
	}
	if snap.Name != "" {
		dto.Title = snap.Name
	} else {
		dto.Title = snap.VulnClass
	}
	return dto
}

// notifyMeta は通知ページに必要な静的メタデータと全体設定を返す。1 回のリクエストで揃える。
// ドロップダウン 1 つのためにフロントが 3 回リクエストするのを避ける。
func (s *Server) notifyMeta(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	kinds := make([]map[string]any, 0, len(notify.Kinds()))
	for _, k := range notify.Kinds() {
		ch, _ := notify.Get(k)
		kinds = append(kinds, map[string]any{
			"kind":                 k,
			"default_rate_per_min": ch.DefaultRatePerMin(),
			"secret_keys":          ch.SecretKeys(),
		})
	}
	baseURL, _, _ := pg.GetSetting(settingNotifyPublicBaseURL)
	digest, _, _ := pg.GetSetting(settingNotifyDigestMinutes)
	stats, err := pg.NotificationStatsSnapshot(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"kinds":               kinds,
		"enabled":             pg.GetBool(settingNotifyEnabled, true),
		"public_base_url":     baseURL,
		"digest_interval_min": digest,
		"defaults": map[string]any{
			"digest_interval_min": notifyDefaultDigestMinutes,
		},
		"stats": stats,
	})
}

func (s *Server) notifyListChannels(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	channels, err := pg.ListNotificationChannels(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out := make([]notifyChannelDTO, 0, len(channels))
	for _, ch := range channels {
		out = append(out, toNotifyChannelDTO(ch))
	}
	writeJSON(w, 200, map[string]any{"channels": out})
}

// notifyChannelRequest はチャネルの作成と更新のリクエストボディである。
//
// 業務フィールドはすべてポインタにし、「未送信」と「ゼロ値を送った」を区別する。PATCH では
// 未送信のフィールドは DB の元の値を残す。
type notifyChannelRequest struct {
	Name       *string        `json:"name"`
	Kind       *string        `json:"kind"`
	Enabled    *bool          `json:"enabled"`
	Mode       *string        `json:"mode"`
	Config     map[string]any `json:"config"`
	Filter     *notify.Filter `json:"filter"`
	RatePerMin *int           `json:"rate_per_min"`
}

func (s *Server) notifyCreateChannel(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var req notifyChannelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "リクエストボディが正しい JSON ではありません: "+err.Error())
		return
	}
	if req.Kind == nil || !notify.ValidKind(*req.Kind) {
		writeErr(w, 400, fmt.Sprintf("チャネル種別が無効です。指定できる値：%s", strings.Join(notify.Kinds(), " / ")))
		return
	}
	name := ""
	if req.Name != nil {
		name = strings.TrimSpace(*req.Name)
	}
	if name == "" {
		writeErr(w, 400, "チャネル名がありません")
		return
	}
	channel, _ := notify.Get(*req.Kind)
	if err := channel.Validate(req.Config); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	ch := &db.NotificationChannel{
		Name:       name,
		Kind:       *req.Kind,
		Enabled:    req.Enabled,
		Mode:       db.NotifyModeRealtime,
		RatePerMin: channel.DefaultRatePerMin(),
	}
	if req.Mode != nil {
		if !db.ValidNotifyMode(*req.Mode) {
			writeErr(w, 400, "通知モードが無効です。指定できる値：realtime / digest")
			return
		}
		ch.Mode = *req.Mode
	}
	if req.RatePerMin != nil {
		// 明示した値はそのまま使う。0 を含む。0 は「レート制限なし」で、正当な設定である。
		if *req.RatePerMin < 0 {
			writeErr(w, 400, "レート制限値は負の値にできません")
			return
		}
		ch.RatePerMin = *req.RatePerMin
	}
	// 「フィールド省略」のときだけチャネルのデフォルトを使う。デフォルトは db 層ではなくここで決める。
	// 「このフィールドを送っていない」と「明示的に 0 を送った」を区別できるのはリクエストボディだけであり、意味は全く違う
	// （前者はデフォルト、後者はレート制限なし）。db 層が 0 も未指定として扱うと、レート制限なしの設定に到達できない。
	if req.RatePerMin == nil {
		ch.RatePerMin = channel.DefaultRatePerMin()
	}
	if req.Filter != nil {
		// 書き込み時に、取り得る値が限られるフィルタフィールド（min_severity など）を検証する。notify.Filter.Validate を参照。
		// しきい値の誤字はフィルタを静かに無効にして全件送信になる。入口で止める。
		if err := req.Filter.Validate(); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		raw, _ := json.Marshal(req.Filter)
		ch.Filter = raw
	}
	rawCfg, _ := json.Marshal(req.Config)
	ch.Config = rawCfg

	id, err := pg.SaveNotificationChannel(r.Context(), ch)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": id})
}

func (s *Server) notifyUpdateChannel(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "チャネル id が無効です")
		return
	}
	current, err := pg.NotificationChannelByID(r.Context(), id)
	if err != nil {
		notifyChannelLookupErr(w, err)
		return
	}
	var req notifyChannelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "リクエストボディが正しい JSON ではありません: "+err.Error())
		return
	}

	// kind は変更できる。種別を変えると認証情報フィールドは一式置き換わり、古い設定とはマージしない。
	kind := current.Kind
	if req.Kind != nil {
		if !notify.ValidKind(*req.Kind) {
			writeErr(w, 400, fmt.Sprintf("チャネル種別が無効です。指定できる値：%s", strings.Join(notify.Kinds(), " / ")))
			return
		}
		kind = *req.Kind
	}
	channel, _ := notify.Get(kind)

	var stored map[string]any
	if kind == current.Kind {
		if len(current.Config) > 0 {
			_ = json.Unmarshal(current.Config, &stored)
		}
	}
	if stored == nil {
		stored = map[string]any{}
	}
	// 素の MergeConfig ではなく PrepareConfigUpdate を使う。宛先アドレスが変わるときは、操作者に
	// 認証情報フィールドを改めて示させる。「アドレスだけ変えて認証情報は流用」だと、DB の本物の認証情報を新しいアドレスへ送ることになる。
	merged, err := notify.PrepareConfigUpdate(kind, stored, req.Config)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := channel.Validate(merged); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	rawCfg, _ := json.Marshal(merged)

	ch := &db.NotificationChannel{
		ID:         id,
		Name:       current.Name,
		Kind:       kind,
		Enabled:    current.Enabled,
		Mode:       current.Mode,
		Config:     rawCfg,
		Filter:     current.Filter,
		RatePerMin: current.RatePerMin,
	}
	if req.Name != nil {
		if ch.Name = strings.TrimSpace(*req.Name); ch.Name == "" {
			writeErr(w, 400, "チャネル名は空にできません")
			return
		}
	}
	if req.Enabled != nil {
		ch.Enabled = req.Enabled
	}
	if req.Mode != nil {
		if !db.ValidNotifyMode(*req.Mode) {
			writeErr(w, 400, "通知モードが無効です。指定できる値：realtime / digest")
			return
		}
		ch.Mode = *req.Mode
	}
	if req.RatePerMin != nil {
		if *req.RatePerMin < 0 {
			writeErr(w, 400, "レート制限値は負の値にできません")
			return
		}
		ch.RatePerMin = *req.RatePerMin
	}
	if req.Filter != nil {
		if err := req.Filter.Validate(); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		raw, _ := json.Marshal(req.Filter)
		ch.Filter = raw
	}

	// SetNotificationChannelEnabled を通す。SaveNotificationChannel の経路は通さない。
	// 「停止」と同時に、残っている未送信の配信を skipped にする。再度有効にしたときに
	// 期限の過ぎた滞留メッセージを受け取らないようにする。
	enabledChanged := ch.Enabled != nil && current.Enabled != nil && *ch.Enabled != *current.Enabled
	if enabledChanged {
		// 先に設定の更新を DB へ書く（この時点の enabled は旧値。スキップ処理が先に走らないようにする）。
		// その後でスイッチだけ切り替える。2 ステップの間に並行の窓はない。この 2 フィールドを変える入口はこの API だけである。
		prev := ch.Enabled
		ch.Enabled = current.Enabled
		if _, err := pg.SaveNotificationChannel(r.Context(), ch); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if err := pg.SetNotificationChannelEnabled(r.Context(), id, *prev); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"id": id})
		return
	}
	if _, err := pg.SaveNotificationChannel(r.Context(), ch); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": id})
}

func (s *Server) notifyDeleteChannel(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "チャネル id が無効です")
		return
	}
	if err := pg.DeleteNotificationChannel(r.Context(), id); err != nil {
		notifyChannelLookupErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// notifyTestChannel は、現在保存されている設定でテストメッセージを 1 件送る。
//
// 配信キューを通さず、チャネルの Send を直接呼ぶ。テストの目的は、ユーザーに「この設定で
// 送れるか」をすぐ伝えることである。キューに入れると結果が配信履歴に隠れ、成否を知るにはもう一度開く必要がある。
// そのためこの API は同期である。タイムアウト上限は notify パッケージの HTTP クライアントが決める（15 秒）。
func (s *Server) notifyTestChannel(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "チャネル id が無効です")
		return
	}
	ch, err := pg.NotificationChannelByID(r.Context(), id)
	if err != nil {
		notifyChannelLookupErr(w, err)
		return
	}
	channel, ok := notify.Get(ch.Kind)
	if !ok {
		writeErr(w, 400, fmt.Sprintf("チャネル種別 %q は登録されていません", ch.Kind))
		return
	}
	var cfg map[string]any
	if len(ch.Config) > 0 {
		_ = json.Unmarshal(ch.Config, &cfg)
	}
	if err := channel.Validate(cfg); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	msg := notifyTestMessage(s.notifierBaseURL(pg))
	start := time.Now()
	// テストメッセージは 1 件だけなので、到達件数はここでは不要である（チャネルの長さ上限は 1 件のメッセージについては
	// 切り詰めで足り、分割はしない）。
	if _, err := channel.Send(r.Context(), cfg, msg); err != nil {
		// チャネルが返した生のエラーを、そのままユーザーへ返す。設定を調べるときの唯一の手がかりである。
		writeErr(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok":         true,
		"latency_ms": time.Since(start).Milliseconds(),
	})
}

// notifyTestMessage はテストメッセージを作る。テストだと一目で分かる内容にする。
// 受け取った人が、本物の脆弱性と取り違えないようにする。
func notifyTestMessage(baseURL string) notify.Message {
	return notify.Message{
		Items: []notify.Item{{
			FindingID: 0,
			Name:      "テストメッセージ · チャネル設定は正常です",
			VulnClass: "接続テスト",
			Severity:  "low",
			Summary:   "これは ARTEX の通知チャネルのテストメッセージです。受信できていれば、このチャネルの設定は有効です。",
			Assets:    []string{"artex.example.com"},
			DetailURL: baseURL,
		}},
		HomeURL: baseURL,
	}
}

// notifierBaseURL は、リンクを戻すときに使う外部アドレスを読む。
func (s *Server) notifierBaseURL(pg *db.DB) string {
	v, _, _ := pg.GetSetting(settingNotifyPublicBaseURL)
	return trimTrailingSlash(v)
}

func (s *Server) notifyListDeliveries(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	f := db.NotificationDeliveryFilter{
		State:     r.URL.Query().Get("state"),
		EventKind: r.URL.Query().Get("event_kind"),
	}
	if v := r.URL.Query().Get("channel_id"); v != "" {
		f.ChannelID = int64(atoiDefault(v, 0))
	}
	page := queryInt(r, "page", 1)
	pageSize := queryInt(r, "page_size", 50)
	items, total, err := pg.ListNotificationDeliveries(r.Context(), f, page, pageSize)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out := make([]notifyDeliveryDTO, 0, len(items))
	for _, dl := range items {
		out = append(out, toNotifyDeliveryDTO(dl))
	}
	writeJSON(w, 200, map[string]any{"deliveries": out, "total": total, "page": page, "page_size": pageSize})
}

func (s *Server) notifyRetryDelivery(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, "配信 id が無効です")
		return
	}
	if err := pg.RetryNotificationDelivery(r.Context(), id); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// notifyChannelLookupErr は「チャネルが存在しない」を 404 にし、それ以外のエラーは 500 にする。
func notifyChannelLookupErr(w http.ResponseWriter, err error) {
	if errors.Is(err, db.ErrNotificationChannelNotFound) {
		writeErr(w, 404, "通知チャネルが存在しません")
		return
	}
	writeErr(w, 500, err.Error())
}
