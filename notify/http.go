package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

// allowLocalTargets は、メッセージをループバックまたはリンクローカルアドレスへ配信してよいかを決めます。
//
// 既定は拒否です。これらのアドレス帯は IM ロボットや公開メールサーバーが現れる場所ではなく、到達先は
// 敏感です。同じマシンの別サービスの管理ポートや、クラウド環境のメタデータエンドポイント
// （169.254.169.254。インスタンスの認証情報を読めます）です。配信先は管理者が設定しますが、XSS や CSRF で
// 借りた管理セッションや、同じ JWT を共有する別の人が、設定を変えて応答内容を読み戻せます。
// doJSON は 4xx/5xx の応答本文の先頭 200 バイトを last_error に書き、配信履歴 API が
// それを表示します。これが半盲読みのプリミティブです。
//
// ただし「このマシン上の SMTP リレー」（127.0.0.1:25 の postfix）は自前メールでよくある設定で、
// 一律に拒否すると詰まります。ハードコードで許可せず、明示的な逃げ道を残します。
// ARTEX_NOTIFY_ALLOW_LOCAL=1 を設定すると許可します。
//
// AllowLocalTargetsEnv として公開しているのは、テストが明示的に開けられるようにするためです。このパッケージと server パッケージの
// テストは 127.0.0.1 上の httptest の偽受信側を大量に使い、開けないと防御がすべて止めます。
const AllowLocalTargetsEnv = "ARTEX_NOTIFY_ALLOW_LOCAL"

func allowLocalTargets() bool {
	v := strings.TrimSpace(os.Getenv(AllowLocalTargetsEnv))
	return v == "1" || strings.EqualFold(v, "true")
}

// isBlockedDialIP は、対象 IP が「既定では配信を許可しない」アドレス帯に属するかを報告します。
//
// 拒否するのはループバック、リンクローカル（クラウドメタデータ 169.254.169.254 を含む）、未指定、マルチキャストです。
// RFC1918 の私設網は拒否しません。社内の自前 Mattermost や SMTP リレーは正当な使い方としてよくあり、
// それらまで止めると、実際の環境で機能が使えなくなります。この取捨は意図したものです。
// 防御は本当に敏感な対象を止め、通常の配置まで無効にはしません。
func isBlockedDialIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	// IPv4-mapped IPv6（::ffff:127.0.0.1）は IPv4 に戻してから判定します。戻さないと検査を回避できます。
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

// blockInternalDial は http.Transport のダイヤラの Control フックで、接続を確立するときに
// 対象アドレスを検査します。
//
// ダイヤル段階に置く理由: ここが最終的に効く点だからです。設定保存時の検証だけでは足りません。
// 設定検証を回避する 2 つの場合を同時に覆います。DNS リバインディング（検証時は公開 IP、
// 実際の接続時は内部ネット）と、リダイレクトです（ホストをまたぐ移動は拒否済みですが、同じホストの移動は
// パスを別の場所へ向けられます）。
func blockInternalDial(_, address string, _ syscall.RawConn) error {
	if allowLocalTargets() {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("目標アドレス %q を解決できません", host)
	}
	if isBlockedDialIP(ip) {
		return fmt.Errorf("このマシンまたはリンクローカルのアドレス %s への配信は拒否します（このマシン上のサービスへ配信する必要がある場合は、%s=1 を設定してください）", ip, AllowLocalTargetsEnv)
	}
	return nil
}

// notifyTransport は、既定の Transport にダイヤルの防御だけを足したものです。
// Clone で既定の調整（接続プール、HTTP/2、タイムアウト、proxy など）を残し、
// 検査を 1 つ足すために他の動作を変えないようにします。
var notifyTransport = func() *http.Transport {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{}
	}
	clone := t.Clone()
	clone.DialContext = (&net.Dialer{Timeout: 10 * time.Second, Control: blockInternalDial}).DialContext
	return clone
}()

// httpClient は、すべてのチャネル配信が共用するクライアントです。
//
// プロジェクトのグローバルな出口プロキシ（server 側の GlobalProxy）は意図して使い回しません。あのプロキシはペネトレーション
// 対象のトラフィック用で、不安定なトンネルであることが多く、通知の可用性を対象ネットワークの揺れに縛るべきではありません。
// IM の通知は直接つながれば足ります。タイムアウトは 15 秒です。これより遅い相手は、実質すでに障害です。
//
// ホストをまたぐリダイレクトは拒否します。この機能の配信先は「固定の endpoint」の形で、正常時に
// 別のホストへリダイレクトしません。そして各社の認証情報（DingTalk の access_token、WeCom の key、Telegram の
// bot token）は URL の中にあります。ホストをまたぐ移動に付いていくと、認証情報をリダイレクト先へ渡すことになります。同じホスト
// の移動（末尾のスラッシュ補完など）は許可します。
var httpClient = &http.Client{
	Timeout:   15 * time.Second,
	Transport: notifyTransport,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("リダイレクトが多すぎます")
		}
		if len(via) > 0 && req.URL.Host != via[0].URL.Host {
			return fmt.Errorf("別のホストへのリダイレクトは拒否します（%s → %s）", via[0].URL.Host, req.URL.Host)
		}
		return nil
	},
}

// respBodyLimit は、読む応答本文の大きさを制限します。相手が異常なとき巨大な内容を返すことがあり、必要なのは
// エラーコードと、配信履歴に出す短いエラー説明だけです。
const respBodyLimit = 8 << 10

// doJSON はリクエストを 1 回送り、応答本文（長さ制限済み）を返します。
//
// payload が nil のときは空の body を送ります（GET や、プラットフォームが body を要求しない場合）。
// headers のキーと値はそのまま付け、汎用 Webhook のカスタムヘッダーに使います。
//
// エラー分類がこの関数の中心です。ネットワーク層の失敗と 5xx/408/429 は「再試行できる」に、
// それ以外の 4xx は「永続的な失敗」にします。403 を再試行しても、同じエラーをログに 3 回書くだけです。
func doJSON(ctx context.Context, method, url string, headers map[string]string, payload any) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			// シリアライズ失敗はローカルの不具合（設定フィールドの型が違う）で、再試行しても良くなりません。
			return nil, Permanent(fmt.Errorf("リクエスト本文を組み立てられませんでした: %w", err))
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		// URL が不正です。多くはユーザーのアドレス記入ミスで、永続的な失敗です。
		// ここでも err はそのまま渡しません。url.Parse のエラー文には完全なアドレスが入ります。
		return nil, Permanent(fmt.Errorf("リクエスト先のアドレスが不正です: %s", redactRequestTarget(url)))
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		// 接続拒否、DNS 失敗、タイムアウトは、多くが一時的な障害です。退避して再試行します。
		//
		// エラー文はマスクしてから外へ出します。理由: http.Client.Do が返すのは *url.Error で、
		// Error() は `Op "完全なURL": 下位のエラー` です。この機能の各社の認証情報は URL の中にあります
		// （DingTalk の access_token、WeCom の key、Feishu の hook id、Telegram の /bot<token>/）。
		// マスクしないと、認証情報がこのエラー文字列を通って 4 か所へ流れます。notification_deliveries
		// の last_error（平文で保存）、配信履歴 API の応答（チャネル設定のマスクを回避）、
		// サーバーログ、テスト送信 API がフロントへ返す 502 の文面です。
		return nil, fmt.Errorf("リクエストに失敗しました: %s", redactTransportError(err))
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, respBodyLimit))
	if readErr != nil {
		return nil, fmt.Errorf("応答を読み取れませんでした: %w", readErr)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return raw, nil
	}
	// 429（制限）と 408（タイムアウト）は再試行する価値があります。それ以外の 4xx は設定か権限の問題で、再試行に意味はありません。
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout {
		return nil, fmt.Errorf("相手が制限しているか、タイムアウトしました (HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("相手のサービスが異常です (HTTP %d): %s", resp.StatusCode, snippet(raw))
	}
	return nil, Permanent(fmt.Errorf("相手がリクエストを拒否しました (HTTP %d): %s", resp.StatusCode, snippet(raw)))
}

// snippet は応答本文を 1 行の短いテキストにします。エラー情報に使います。応答には改行と大量の空白があり、
// そのまま last_error に入れると配信履歴のページのレイアウトが壊れます。
func snippet(raw []byte) string {
	return OneLine(string(raw), 200)
}

// redactRequestTarget は配信先を「scheme://host/…」に圧縮し、エラー情報に使います。
//
// これがこのパッケージでアドレスをマスクする唯一の口径で、意図して粗くしています。scheme と host 以外は
// すべて捨てます。URL のどの部分が認証情報かを、汎用かつ安全に判断する方法が無いからです。
//
//	DingTalk   認証情報は query      /robot/send?access_token=xxx
//	WeCom      認証情報は query      /cgi-bin/webhook/send?key=xxx
//	Feishu     認証情報はパスの末尾  /open-apis/bot/v2/hook/<hook_id>
//	Telegram   認証情報はパスの途中  /bot<token>/sendMessage
//
// 「有用な部分だけ残す」にはチャネルごとのパッチが要り、1 社でも漏らすと認証情報の漏えいです。
// host を残すだけでも調査には足ります（DNS が解決できない、つながらない、証明書が違う、は特定できます）。
// どのロボットかは、チャネル設定のマスク末尾が示します。
//
// 解析に失敗したときは固定のプレースホルダを返します。元の文字列は絶対に表示しません。
func redactRequestTarget(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(アドレスを解析できません)"
	}
	return u.Scheme + "://" + u.Host + "/…"
}

// redactTransportError は転送層のエラーからアドレスを外し、下位の原因だけを残します。
//
// *url.Error の構造は {Op, URL, Err} で、Error() は URL も一緒に出します。
// ここでは Err フィールドを明示的に取り、Error() を避けます。後から文字列置換するより確実です。
// 置換は URL エンコードやエスケープ後の変種に正しく対応する必要があり、漏れやすいです。
func redactTransportError(err error) string {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		host := ""
		if u, parseErr := url.Parse(uerr.URL); parseErr == nil {
			host = u.Host
		}
		if uerr.Err != nil {
			return fmt.Sprintf("%s %s: %s", uerr.Op, host, uerr.Err)
		}
		return fmt.Sprintf("%s %s: 不明なエラー", uerr.Op, host)
	}
	// *url.Error でないもの（リダイレクト方針が返すエラーなど）もアドレスを含むことがあるので、同じマスクを通します。
	return redactURLsInText(err.Error())
}

// redactURLsInText は、テキスト中の http(s) アドレスをマスクした形に置き換えます。
//
// 構造化フィールドが取れないエラー（リダイレクト方針のエラー、サードパーティの独自エラー）の受けです。
// http/https の接頭辞だけを認識し、空白と引用符で切ります。アドレスにこの 2 種の文字は含まれません。
func redactURLsInText(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		rest := s[i:]
		if strings.HasPrefix(rest, "http://") || strings.HasPrefix(rest, "https://") {
			end := len(rest)
			if j := strings.IndexAny(rest, " \t\n\"'"); j >= 0 {
				end = j
			}
			b.WriteString(redactRequestTarget(rest[:end]))
			i += end
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
