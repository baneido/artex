package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// feishuChannel は Feishu（Lark を含む）のカスタムロボットを実装し、対話型カードを使います。
//
// プラットフォームの特性:
//   - 署名アルゴリズムは DingTalk と違います。書き間違いやすいので、feishuSign のコメントを参照してください。
//   - DingTalk と同じく、業務エラーを HTTP 200 の body に入れます（code != 0）。
//   - カードの header は色テンプレートに対応しています。深刻度を色に対応させ、メッセージ一覧で深刻度がすぐ分かるようにします。
type feishuChannel struct{}

func (feishuChannel) Kind() string { return KindFeishu }

// Feishu のカスタムロボットは約 5 回/秒で、1 分あたり 100 回に相当します。
func (feishuChannel) DefaultRatePerMin() int { return 100 }

// Webhook アドレスの末尾がロボットの一意な識別子で、認証情報です。
func (feishuChannel) SecretKeys() []string { return []string{"webhook", "secret"} }

// 同じ理由です。Webhook アドレスを変えるときは、新しいアドレスの署名秘密を改めて指定します。
func (feishuChannel) DestinationKeys() []string { return []string{"webhook"} }

func (feishuChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("Webhook アドレスがありません")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("Webhook アドレスが無効です: %w", err)
	}
	return nil
}

func (c feishuChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	card, kept := feishuCard(m)
	payload := map[string]any{
		"msg_type": "interactive",
		"card":     card,
	}
	// 署名パラメータはメッセージと同じ階層で、secret を設定したときだけ現れます。
	if secret := cfgString(cfg, "secret"); secret != "" {
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		payload["timestamp"] = ts
		payload["sign"] = feishuSign(ts, secret)
	}
	raw, err := doJSON(ctx, "POST", cfgString(cfg, "webhook"), nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		// 一部バージョンの Feishu hook はこのフィールド名を使うので、合わせて受けます。
		StatusCode    int    `json:"StatusCode"`
		StatusMessage string `json:"StatusMessage"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("Feishu の応答を解析できませんでした: %w (%s)", err, snippet(raw))
	}
	if res.Code != 0 {
		return 0, Permanent(fmt.Errorf("Feishu がエラー %d を返しました: %s", res.Code, res.Msg))
	}
	if res.StatusCode != 0 {
		return 0, Permanent(fmt.Errorf("Feishu がエラー %d を返しました: %s", res.StatusCode, res.StatusMessage))
	}
	return kept, nil
}

// feishuSign は Feishu 公式の規則で署名を計算します。
//
// ここは特に間違えやすいです。公式サンプルは次です。
//
//	hmac.new(string_to_sign.encode(), digestmod=sha256)
//
// つまり key = timestamp + "\n" + secret で、message は空です。「key=secret, message=stringToSign」ではありません。
// 後者は DingTalk のアルゴリズムです。両者はちょうど逆で、
// もう一方の実装を写すと署名検証が必ず失敗します（19021 を返します）。
func feishuSign(timestamp, secret string) string {
	stringToSign := timestamp + "\n" + secret
	mac := hmac.New(sha256.New, []byte(stringToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// feishuSeverityTemplate は脆弱性の深刻度をカード header の色テンプレートへ対応させます。
// 未知の深刻度は grey です。blue は使いません。low と混ざるためです。
func feishuSeverityTemplate(severity string) string {
	switch severity {
	case "critical":
		return "red"
	case "high":
		return "orange"
	case "medium":
		return "yellow"
	case "low":
		return "blue"
	default:
		return "grey"
	}
}

// feishuMaxCardBytes はカード内容の保守的な上限です。Feishu はカードにサイズ制限があり、超えるとメッセージ全体が拒否されます。
// 公式上限より明らかに低い値を取り、JSON の包みのオーバーヘッドも含めます。
const feishuMaxCardBytes = 24000

// feishuCard は対話型カードを組み立て、カードと実際に書いた件数を返します。
// kept の用途は markdownBody と同じです。カードに本当に入った項目だけを、届いたものとして印を付けます。
func feishuCard(m Message) (map[string]any, int) {
	elements := []any{}
	kept := 0
	if m.Batch {
		// 先に件単位で詰めてから先頭を組み立てます。先頭には「残り N 件は次のメッセージで続きます」と書くので、
		// N は実際に入った件数から取ります。
		kept = packItemCount(m.Items, feishuMaxCardBytes, markdownReservedBytes, "", byteSize, func(it Item, idx int) string {
			return feishuBatchLine(it, idx+1)
		})
		items := m.Items[:kept]
		elements = append(elements, feishuMarkdownDiv(markdownBatchIntro(m, items, len(m.Items))))
		for i, it := range items {
			elements = append(elements, feishuMarkdownDiv(feishuBatchLine(it, i+1)))
		}
		if m.HomeURL != "" {
			elements = append(elements, feishuButton("プラットフォームですべて見る", m.HomeURL))
		}
	} else if len(m.Items) > 0 {
		kept = 1
		it := m.Items[0]
		elements = append(elements, feishuMarkdownDiv(feishuItemLines(it)))
		if it.DetailURL != "" {
			elements = append(elements, feishuButton("詳細を見る", it.DetailURL))
		}
	}

	card := map[string]any{
		"config":   map[string]any{"wide_screen_mode": true},
		"header":   map[string]any{"title": map[string]any{"tag": "plain_text", "content": markdownTitle(m)}},
		"elements": elements,
	}
	if len(m.Items) > 0 {
		card["header"].(map[string]any)["template"] = feishuSeverityTemplate(m.Items[0].Severity)
	}
	return card, kept
}

func feishuMarkdownDiv(content string) map[string]any {
	return map[string]any{"tag": "div", "text": map[string]any{"tag": "lark_md", "content": content}}
}

func feishuButton(label, url string) map[string]any {
	return map[string]any{
		"tag": "action",
		"actions": []any{map[string]any{
			"tag":  "button",
			"text": map[string]any{"tag": "lark_md", "content": label},
			"url":  url,
			"type": "primary",
		}},
	}
}

// feishuItemLines は脆弱性 1 件の lark_md 本文を描画します。
//
// lark_md と markdown は同族のテキスト形式で、同じくリンクと強調を解釈します。外部から来た
// フィールドはすべて markdownText（1 行化とエスケープ）を通します。通さないと、脆弱性タイトル 1 件が
// Feishu 上でクリックできる外部リンクになります。
func feishuItemLines(it Item) string {
	out := fmt.Sprintf("**%s %s**", SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if it.IsStatusChange() {
		out += fmt.Sprintf("\n**状態の変更**: %s から %s",
			markdownText(StatusLabel(it.FromStatus), 0), markdownText(StatusLabel(it.ToStatus), 0))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		out += fmt.Sprintf("\n**種別**: %s", markdownText(it.VulnClass, 0))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		out += fmt.Sprintf("\n**資産**: %s", markdownText(a, 0))
	}
	if it.Summary != "" {
		if s := markdownText(it.Summary, maxSummaryRunes); s != "" {
			out += fmt.Sprintf("\n**要約**: %s", s)
		}
	}
	return out
}

// feishuBatchLine はまとめカードの 1 件を描画します。
func feishuBatchLine(it Item, index int) string {
	line := fmt.Sprintf("**%d. %s %s**", index, SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		line += " / " + markdownText(a, 0)
	}
	return line
}
