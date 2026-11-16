package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"
)

// dingTalkChannel は DingTalk のカスタムロボットを実装します。
//
// プラットフォームの特性（ここでの実装の取捨を決めています）:
//   - ロボット 1 台の制限は 20 件/分です。超過分は黙って捨てられます（HTTP は 200 のままのことがあります）。
//     そのため制限はクライアント側で行います。DefaultRatePerMin を参照してください。
//   - セキュリティ設定は 3 つのうち 1 つです。署名の追加、カスタムキーワード、IP 許可リストです。署名の追加だけが
//     メッセージ内容に依存しません。そのため署名の追加だけをサポートします（3 つともオフの素の webhook も使えます）。
//   - 成功も失敗も HTTP 200 を返し、body の errcode で区別します。errcode を見ないと、
//     配信失敗を成功として記録します。
type dingTalkChannel struct{}

func (dingTalkChannel) Kind() string { return KindDingTalk }

func (dingTalkChannel) DefaultRatePerMin() int { return 20 }

// DingTalk の Webhook アドレスには access_token が含まれ、それ自体が認証情報なので、全体をマスクします。
func (dingTalkChannel) SecretKeys() []string { return []string{"webhook", "secret"} }

// 宛先は DingTalk の Webhook アドレスそのものです。アドレスを変えるときは、新しいアドレスの署名秘密も改めて指定します。
func (dingTalkChannel) DestinationKeys() []string { return []string{"webhook"} }

func (dingTalkChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("Webhook アドレスがありません")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("Webhook アドレスが無効です: %w", err)
	}
	return nil
}

// Send はメッセージを 1 回配信します。戻りリンクがあり単発のときは ActionCard（ボタン付き）を使い、それ以外は markdown です。
func (c dingTalkChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	hook := cfgString(cfg, "webhook")
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	endpoint, err := dingTalkSignedURL(hook, cfgString(cfg, "secret"), time.Now())
	if err != nil {
		return 0, Permanent(err)
	}

	title := markdownTitle(m)
	// DingTalk の markdown 本文に明示的なバイト上限はありません。証拠フィールドが異常に膨らまないよう、上限で守ります。
	text, kept := markdownBody(m, 20000)

	var payload any
	if !m.Batch && len(m.Items) == 1 && m.Items[0].DetailURL != "" {
		payload = map[string]any{
			"msgtype": "actionCard",
			"actionCard": map[string]any{
				"title":          title,
				"text":           text,
				"btnOrientation": "0",
				"singleTitle":    "詳細を見る",
				"singleURL":      m.Items[0].DetailURL,
			},
		}
	} else {
		payload = map[string]any{
			"msgtype":  "markdown",
			"markdown": map[string]any{"title": title, "text": text},
		}
	}

	raw, err := doJSON(ctx, "POST", endpoint, nil, payload)
	if err != nil {
		return 0, err
	}
	// DingTalk は業務エラーを 200 応答の中に隠します。
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("DingTalk の応答を解析できませんでした: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 301000 は署名検証の失敗、310000 はキーワード不一致です。どちらも設定エラーで、
		// 再試行では直りません。
		return 0, Permanent(fmt.Errorf("DingTalk がエラー %d を返しました: %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}

// dingTalkSignedURL は公式の署名規則で、webhook に timestamp と sign を足します。
//
// 規則: 署名対象は timestamp + "\n" + secret です。HMAC-SHA256 の鍵も secret で、
// 結果を base64 してから URL エンコードします。timestamp はミリ秒です。secret が空のときはそのまま返し、
// 署名を有効にしていないロボットに対応します。
func dingTalkSignedURL(hook, secret string, now time.Time) (string, error) {
	if secret == "" {
		return hook, nil
	}
	ts := strconv.FormatInt(now.UnixMilli(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "\n" + secret))
	sign := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	u, err := url.Parse(hook)
	if err != nil {
		// err はそのまま渡しません。url.Parse のエラー文には完全なアドレス（access_token を含む）が入ります。
		return "", fmt.Errorf("Webhook アドレスを解析できませんでした: %s", redactRequestTarget(hook))
	}
	q := u.Query()
	q.Set("timestamp", ts)
	q.Set("sign", sign)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// validateHTTPURL はアドレスが使え、スキームが対応していることを確かめ、リテラル IP の宛先が内部ネットかを判定します。
//
// 2 点に注意します。
//
//  1. エラーメッセージはマスクします。url.Parse が返すのは *url.Error で、Error() に
//     完全な元のアドレスが入ります。この機能のアドレスには認証情報が埋まっています（DingTalk の access_token、
//     WeCom の key、Telegram の bot token、Feishu の hook id）。以前はここで `return err` していたため、
//     「アドレス形式が不正」というエラーが認証情報を外へ運び、テスト API の 400、
//     配信のたびに保存する last_error、サーバーログ、配信履歴 API へ流れていました。
//
//  2. リテラル IP はここで内部ネットと判定し、ドメインはダイヤル時に判定します（最終的に効くのは blockInternalDial で、
//     DNS リバインディングも覆います）。ここでも一度見るのは、設定の保存時に案内を出し、
//     最初の配信失敗まで待たないためです。
//
// スキームを制限するのは防御です。file:///gopher:// のような指定は http.Client に想定外の
// 動作をさせます（scheme の検査で既に止まっていますが、この面を開く理由はありません）。
func validateHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("アドレスを解析できません（%s）", redactRequestTarget(raw))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("対応しているのは http/https だけです。受け取ったスキームは %q です", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("ホスト名がありません")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && isBlockedDialIP(ip) && !allowLocalTargets() {
		return fmt.Errorf("このマシンまたはリンクローカルのアドレス %s への配信は拒否します（このマシン上のサービスへ配信する必要がある場合は、%s=1 を設定してください）", ip, AllowLocalTargetsEnv)
	}
	return nil
}
