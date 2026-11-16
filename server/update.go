package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/Autumn-27/artex/selfupdate"
)

// 画面からワンクリックでアップデートする HTTP 面。ダウンロード、検証、入れ替えの実処理はすべて selfupdate パッケージにあり、
// ここは認証の境界、同時実行の排他、進捗の配信、そして「終了すべきだ」を main に伝えることだけを担う。
//
// 再起動はこのプロセスでは行わない。新しいバージョンを一時保存したあと、プロセスは selfupdate.ExitRestart で終了し、
// 起動スクリプト（start.sh / start.bat。Docker では ENTRYPOINT）が再び起動する。

// restartCh は、アップデートの準備またはロールバックが終わると閉じる。main はそれを受けて ExitRestart で終了する。
var (
	restartOnce sync.Once
	restartCh   = make(chan struct{})
)

// RestartRequested は、「終了して、起動プロセスに自分を引き上げさせてください」のときに閉じる channel を返す。
func RestartRequested() <-chan struct{} { return restartCh }

func requestRestart() { restartOnce.Do(func() { close(restartCh) }) }

// bootState は今回の起動時の selfupdate.Bootstrap の結論（アップデート成功、直前にロールバック、
// 一時保存を破棄）である。main が入れ、/api/update/check が前回のアップデートの結果をフロントへそのまま伝える。
var (
	bootStateMu sync.Mutex
	bootState   selfupdate.State
)

// SetBootUpdateState は、main が起動時に 1 回呼ぶ。
func SetBootUpdateState(st selfupdate.State) {
	bootStateMu.Lock()
	defer bootStateMu.Unlock()
	bootState = st
}

func bootUpdateState() selfupdate.State {
	bootStateMu.Lock()
	defer bootStateMu.Unlock()
	return bootState
}

// releaseCache は GitHub の最新バージョンの問い合わせ結果をキャッシュする。
//
// 上部の「新しいバージョンがある」表示は、ページを読み込むたびに 1 回調べる。未認証の GitHub API は
// IP あたり毎時 60 回である。キャッシュが無いと、タブをいくつか開くかページを数回開くだけで枠を使い切り、
// そのあと本当にアップデートしたくても調べられなくなる。ユーザーが「アップデートを確認」を押したときは force でキャッシュを避けられる。
type releaseCache struct {
	mu  sync.Mutex
	rel *selfupdate.Release
	err error
	at  time.Time
	// fetch は取得関数で、テスト用の注入点である。nil のときは本当の GitHub 問い合わせに進む。
	fetch func(context.Context, *http.Client) (*selfupdate.Release, error)
}

const (
	releaseTTL = 30 * time.Minute
	// 失敗の結果も少しの間キャッシュする。そうでないと、GitHub に届かないとき、ページを開くたびにタイムアウトを待つ。
	// ただし TTL は短くし、ネットワークが戻ったらすぐ自分で直る。
	releaseErrTTL = 2 * time.Minute
	// 問い合わせのタイムアウト。NewClient の 30 分はパッケージ全体のダウンロード用で、バージョン確認はそこまで待てない。
	releaseTimeout = 20 * time.Second
)

var relCache = &releaseCache{}

// get は最新の Release を返す。キャッシュが当たればネットワークへ行かない。
//
// 取得の間はロックを持ち続ける。同時のリクエストは、それぞれ GitHub へ行かず、同じ 1 回の結果を待つ。
// （ページを開いた直後に複数のタブが同時に調べるのが、制限に当たりやすい。）
func (c *releaseCache) get(ctx context.Context, client *http.Client, force bool) (*selfupdate.Release, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !force {
		ttl := releaseTTL
		if c.err != nil {
			ttl = releaseErrTTL
		}
		if !c.at.IsZero() && time.Since(c.at) < ttl {
			return c.rel, c.err
		}
	}

	fetch := c.fetch
	if fetch == nil {
		fetch = selfupdate.FetchLatest
	}
	ctx, cancel := context.WithTimeout(ctx, releaseTimeout)
	defer cancel()
	rel, err := fetch(ctx, client)
	// リクエストのキャンセル（ユーザーがタブを閉じた）は GitHub の障害ではない。キャッシュへ書かない。
	// 書くと、次の訪問者が理由の分からない「キャンセル済み」エラーを受け取る。
	if err != nil && ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
		return c.rel, err
	}
	c.rel, c.err, c.at = rel, err, time.Now()
	return rel, err
}

// updateProgress はフロントへ送る進捗 1 件である。
type updateProgress struct {
	Phase   selfupdate.Phase `json:"phase"`
	Percent int              `json:"percent"` // ダウンロード段階だけで意味がある。それ以外は -1
	Message string           `json:"message"`
	Version string           `json:"version,omitempty"`
	Error   string           `json:"error,omitempty"`
}

// updateHub は 1 回のアップデートの進捗を持ち、SSE の購読者へ配信する。
//
// running は同時に排他も兼ねる。アップデート中に再度 POST /api/update/apply すると 409 を返す。
// 2 つの goroutine が同じ artex.new へ同時に書くのを避ける。
type updateHub struct {
	mu      sync.Mutex
	running bool
	cur     updateProgress
	subs    map[chan updateProgress]struct{}
}

var updHub = &updateHub{
	cur:  updateProgress{Phase: selfupdate.PhaseIdle, Percent: -1},
	subs: map[chan updateProgress]struct{}{},
}

// begin はアップデートの権利を取る。すでに進行中なら false を返す。
func (h *updateHub) begin(version string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.running {
		return false
	}
	h.running = true
	h.cur = updateProgress{Phase: selfupdate.PhaseDownload, Percent: 0, Message: "準備中…", Version: version}
	h.fanout(h.cur)
	return true
}

// finish は 1 回のアップデートを終える。err が nil なら一時保存に成功し、再起動を待つ。
func (h *updateHub) finish(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.running = false
	if err != nil {
		h.cur = updateProgress{Phase: selfupdate.PhaseFailed, Percent: -1, Message: "更新に失敗しました", Error: err.Error(), Version: h.cur.Version}
	} else {
		h.cur = updateProgress{Phase: selfupdate.PhaseStaged, Percent: 100, Message: "新しいバージョンの準備ができました。再起動中…", Version: h.cur.Version}
	}
	h.fanout(h.cur)
}

func (h *updateHub) publish(ph selfupdate.Phase, pct int, msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cur = updateProgress{Phase: ph, Percent: pct, Message: msg, Version: h.cur.Version}
	h.fanout(h.cur)
}

// fanout は h.mu を持っているときに呼ぶ。購読者の channel にはバッファがあり、満杯なら捨てる。
// 進捗は捨ててよい瞬間の情報である。止まった SSE 接続がアップデート自体を止めてはいけない。
func (h *updateHub) fanout(p updateProgress) {
	for ch := range h.subs {
		select {
		case ch <- p:
		default:
		}
	}
}

func (h *updateHub) snapshot() (updateProgress, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cur, h.running
}

func (h *updateHub) subscribe() (<-chan updateProgress, func()) {
	ch := make(chan updateProgress, 64)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, ch)
			h.mu.Unlock()
			close(ch)
		})
	}
}

// updateCheck は GitHub 上の最新の正式版を調べ、現在のバージョンと比べる。
//
// フロントも api.github.com へ直接つなぐ（GitHub の CORS は *）。基準はこの API である。
// ダウンロードはバックエンドが行う。バックエンドが GitHub へ届いて、はじめてアップデートできる。ブラウザは届き、サーバーは届かない
// ことはよくある（サーバーが社内網にある、またはプロキシがブラウザにしか無い）。そのときアップデートは失敗する。
// 確認の段階で、そのままエラーを返す。
func (s *Server) updateCheck(w http.ResponseWriter, r *http.Request) {
	current := BuildVersion
	mode := "binary"
	if selfupdate.InDocker() {
		mode = "docker"
	}
	boot := bootUpdateState()
	out := map[string]any{
		"current":     current,
		"mode":        mode,
		"os":          runtime.GOOS,
		"arch":        runtime.GOARCH,
		"has_backup":  selfupdate.HasBackup(),
		"repo":        selfupdate.Repo,
		"boot_notice": boot.Detail,
		"rolled_back": boot.RolledBack,
	}

	// Online self-update is compiled out in this fork (selfupdate.Enabled).
	// Report "no update, disabled" and return before building a client or
	// querying any release feed, so this endpoint makes no outbound request.
	if !selfupdate.Enabled {
		out["disabled"] = true
		out["has_update"] = false
		out["comparable"] = false
		out["reason"] = "このビルドではオンライン更新が無効です（リモートのバージョンの確認もダウンロードも行いません）"
		writeJSON(w, 200, out)
		return
	}

	// 上部の表示はキャッシュを使う（デフォルト）。ユーザーが「アップデートを確認」を押したときは force=1 で取得し直す。
	force := r.URL.Query().Get("force") != ""
	client := selfupdate.NewClient(s.m.GlobalProxy())
	rel, err := relCache.get(r.Context(), client, force)
	if err != nil {
		out["error"] = err.Error()
		writeJSON(w, 200, out)
		return
	}

	latest := rel.TagName
	out["latest"] = latest
	out["notes"] = rel.Body
	out["html_url"] = rel.HTMLURL
	if !rel.PublishedAt.IsZero() {
		out["published_at"] = rel.PublishedAt.Format(time.RFC3339)
	}

	asset := selfupdate.AssetName(latest, runtime.GOOS, runtime.GOARCH)
	out["asset"] = asset
	if a, ok := rel.FindAsset(asset); ok {
		out["asset_available"] = true
		out["size"] = a.Size
	} else {
		out["asset_available"] = false
	}

	cmp, comparable := selfupdate.CompareVersions(current, latest)
	out["comparable"] = comparable
	out["has_update"] = comparable && cmp < 0
	if !comparable {
		// 開発ビルド（dev、または git describe に接尾辞がある）には比べられるバージョン番号が無い。通すと
		// 正式版が、ローカルでデバッグ中のバイナリを上書きする。そのためアップデートは出さない。
		out["reason"] = fmt.Sprintf("現在のバージョン %q は正式なリリース版ではないため、ワンクリック更新は無効です", current)
	}
	writeJSON(w, 200, out)
}

// updateApply は新しいバージョンをダウンロードして一時保存し、終わったらプロセスを終了して起動スクリプトに再起動させる。
//
// すぐ 202 を返す。実作業はバックグラウンドの goroutine で行う。パッケージ全体のダウンロードは数分かかることがあり、
// リクエストにぶら下げるとリバースプロキシのタイムアウトで切れる。進捗は /api/update/stream で送る。
func (s *Server) updateApply(w http.ResponseWriter, r *http.Request) {
	// Online self-update is compiled out in this fork (selfupdate.Enabled):
	// refuse before any download so no remote binary is ever staged.
	if !selfupdate.Enabled {
		writeErr(w, 403, "このビルドではオンライン更新が無効です")
		return
	}
	current := BuildVersion

	// キャッシュを使う。画面で見て確認したバージョンを入れる。
	client := selfupdate.NewClient(s.m.GlobalProxy())
	rel, err := relCache.get(r.Context(), client, false)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	cmp, comparable := selfupdate.CompareVersions(current, rel.TagName)
	if !comparable {
		writeErr(w, 400, fmt.Sprintf("現在のバージョン %q は正式なリリース版ではないため、ワンクリック更新は無効です", current))
		return
	}
	if cmp >= 0 {
		writeErr(w, 400, fmt.Sprintf("すでに最新バージョン %s です", current))
		return
	}
	if !updHub.begin(rel.TagName) {
		writeErr(w, 409, "更新はすでに進行中です")
		return
	}

	go func() {
		// リクエストの ctx ではなく s.ctx を使う。HTTP 応答を返すとリクエストは終わり、
		// それにぶら下げたダウンロードはすぐキャンセルされる。
		err := selfupdate.Stage(s.ctx, client, rel, current, func(ph selfupdate.Phase, pct int, msg string) {
			updHub.publish(ph, pct, msg)
		})
		updHub.finish(err)
		if err != nil {
			log.Printf("[update] アップデートに失敗しました：%v", err)
			return
		}
		log.Printf("[update] %s → %s を一時保存しました。入れ替えのため、まもなく終了します", current, rel.TagName)
		// 最後の進捗をフロントへ送る時間を少し残してから、終了を起こす。
		time.Sleep(1500 * time.Millisecond)
		requestRestart()
	}()

	writeJSON(w, 202, map[string]any{"ok": true, "target": rel.TagName})
}

// updateRollback は前のバージョンへ戻す（入れ替え前にバックアップした artex.old）。
func (s *Server) updateRollback(w http.ResponseWriter, r *http.Request) {
	// Part of the self-update machinery, which is compiled out in this fork
	// (selfupdate.Enabled). There is never a staged backup to roll back to,
	// so refuse rather than appear to offer a working control.
	if !selfupdate.Enabled {
		writeErr(w, 403, "このビルドではオンライン更新が無効です")
		return
	}
	if _, running := updHub.snapshot(); running {
		writeErr(w, 409, "更新の進行中はロールバックできません")
		return
	}
	if err := selfupdate.Rollback(); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	log.Printf("[update] 前のバージョンへ手動でロールバックしました。切り替えのため、まもなく終了します")
	writeJSON(w, 202, map[string]any{"ok": true})
	go func() {
		time.Sleep(500 * time.Millisecond)
		requestRestart()
	}()
}

// updateStream は SSE でアップデートの進捗を送る。
func (s *Server) updateStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch, unsub := updHub.subscribe()
	defer unsub()

	send := func(p updateProgress) {
		b, _ := json.Marshal(p)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}
	// 先に現在の状態を 1 件足す。ページを再読み込みした直後から、進行中のアップデートが見える。
	cur, _ := updHub.snapshot()
	send(cur)

	ctx := r.Context()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case p, ok := <-ch:
			if !ok {
				return
			}
			send(p)
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}
