package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// telegramTextLimit は Telegram sendMessage の text フィールドの上限です（文字数）。
const telegramTextLimit = 4096

// telegramChannel は Telegram Bot API を実装します。
//
// プラットフォームの特性:
//   - 認証はすべて URL path にあります（/bot<token>/sendMessage）。署名の追加は要りません。
//   - HTML 解析モードを使い、MarkdownV2 は使いません。MarkdownV2 は `_*[]()~`>#+-=|{}.!`
//     の 18 文字をエスケープする必要があり、1 つ漏らすとメッセージ全体が拒否されます。HTML は & < > の 3 つだけで済みます。
//   - 業務エラーも HTTP 200 の中に隠れ、ok フィールドで判定します。
type telegramChannel struct{}

func (telegramChannel) Kind() string { return KindTelegram }

// Telegram の個人チャットは約 1 件/秒、グループは 20 件/分です。保守的な値を取ります。
func (telegramChannel) DefaultRatePerMin() int { return 20 }

// Bot Token は認証情報の全体です。chat_id は宛先だけで、秘密ではありません（Token が無ければメッセージは送れません）。
func (telegramChannel) SecretKeys() []string { return []string{"bot_token"} }

// base_url は Token をどの API エンドポイントへ送るかを決めます（自前のリバースプロキシなど）。変えるときは Token を改めて指定します。
func (telegramChannel) DestinationKeys() []string { return []string{"base_url"} }

func (telegramChannel) Validate(cfg map[string]any) error {
	if cfgString(cfg, "bot_token") == "" {
		return errors.New("Bot Token がありません")
	}
	if cfgString(cfg, "chat_id") == "" {
		return errors.New("Chat ID がありません")
	}
	if base := cfgString(cfg, "base_url"); base != "" {
		if err := validateHTTPURL(base); err != nil {
			return fmt.Errorf("API アドレスが無効です: %w", err)
		}
	}
	return nil
}

func (c telegramChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	endpoint, err := telegramEndpoint(cfg)
	if err != nil {
		return 0, Permanent(err)
	}
	text, kept := telegramHTML(m)
	payload := map[string]any{
		"chat_id":                  cfgString(cfg, "chat_id"),
		"text":                     text,
		"parse_mode":               "HTML",
		"disable_web_page_preview": false,
	}
	raw, err := doJSON(ctx, "POST", endpoint, nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		OK          bool   `json:"ok"`
		ErrorCode   int    `json:"error_code"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("Telegram の応答を解析できませんでした: %w (%s)", err, snippet(raw))
	}
	if res.OK {
		return kept, nil
	}
	// 429 は制限です。待って再試行すれば届きます。それ以外（400 のパラメータ誤り、401 の token 誤り、403 のブロック、
	// 404 の chat 不存在）は設定の問題で、再試行では直りません。
	if res.ErrorCode == 429 {
		return 0, fmt.Errorf("Telegram が制限しています: %s", res.Description)
	}
	return 0, Permanent(fmt.Errorf("Telegram がエラー %d を返しました: %s", res.ErrorCode, res.Description))
}

// telegramEndpoint は sendMessage のアドレスを組み立てます。base_url が空のときは公式 API を使い、
// 空でないときは自前の Bot API リバースプロキシに使います（中国のネットワークではよくある要件です）。
func telegramEndpoint(cfg map[string]any) (string, error) {
	base := cfgString(cfg, "base_url")
	if base == "" {
		base = "https://api.telegram.org"
	}
	base = strings.TrimSuffix(base, "/")
	token := cfgString(cfg, "bot_token")
	raw := base + "/bot" + token + "/sendMessage"
	u, err := url.Parse(raw)
	if err != nil {
		// err はそのまま渡しません。アドレスに Bot Token が含まれ、この時点では addr も表示してはいけません。
		return "", fmt.Errorf("API アドレスを組み立てられませんでした（API アドレス: %s）", redactRequestTarget(base))
	}
	return u.String(), nil
}

// telegramHTML は HTML 本文を描画し、本文と実際に書いた件数を返します（Channel.Send を参照）。
func telegramHTML(m Message) (string, int) {
	var b strings.Builder
	b.WriteString("<b>" + telegramEscape(markdownTitle(m)) + "</b>\n")
	if m.Batch {
		// Telegram の上限は文字数なので、詰め方も文字数で測ります（runeSize）。
		footer := ""
		if m.HomeURL != "" {
			footer = fmt.Sprintf("\n\n<a href=\"%s\">プラットフォームですべて見る</a>", telegramEscapeAttr(m.HomeURL))
		}
		kept := packItemCount(m.Items, telegramTextLimit, telegramReservedRunes, footer, runeSize, func(it Item, idx int) string {
			return telegramBatchLine(it, idx+1)
		})
		items := m.Items[:kept]
		b.Reset()
		b.WriteString("<b>" + telegramEscape(telegramBatchTitle(m, items, len(m.Items))) + "</b>")
		for i, it := range items {
			b.WriteString("\n" + telegramEscape(telegramBatchLine(it, i+1)))
		}
		b.WriteString(footer)
		return TruncateHTML(b.String(), telegramTextLimit), kept
	}
	if len(m.Items) == 0 {
		return b.String(), 0
	}
	it := m.Items[0]
	if it.IsStatusChange() {
		b.WriteString(fmt.Sprintf("\n<b>状態の変更</b>: %s から %s",
			telegramEscape(StatusLabel(it.FromStatus)), telegramEscape(StatusLabel(it.ToStatus))))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		b.WriteString("\n<b>種別</b>: " + telegramEscape(it.VulnClass))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		b.WriteString("\n<b>資産</b>: " + telegramEscape(a))
	}
	if s := OneLine(it.Summary, maxSummaryRunes); s != "" {
		b.WriteString("\n<b>要約</b>: " + telegramEscape(s))
	}
	if it.DetailURL != "" {
		b.WriteString(fmt.Sprintf("\n\n<a href=\"%s\">詳細を見る</a>", telegramEscapeAttr(it.DetailURL)))
	}
	return TruncateHTML(b.String(), telegramTextLimit), 1
}

// telegramReservedRunes は、メッセージタイトルと出るかもしれない切り詰め案内のために残す文字数です。
const telegramReservedRunes = 160

// telegramBatchLine はまとめの 1 件を描画します（未エスケープ。呼び出し側が一括でエスケープします）。
func telegramBatchLine(it Item, idx int) string {
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		return fmt.Sprintf("%d. %s %s / %s", idx, SeverityLabel(it.Severity), it.Title(), a)
	}
	return fmt.Sprintf("%d. %s %s", idx, SeverityLabel(it.Severity), it.Title())
}

// telegramBatchTitle はまとめメッセージのタイトル行を描画します。件数は、このメッセージに実際に含まれる件数を使い、
// このバッチの総数は使いません。使わないと、読者は見出しの数字が全部だと思うからです。
func telegramBatchTitle(m Message, items []Item, total int) string {
	title := fmt.Sprintf("検出事項のまとめ 全 %d 件", total)
	if extra := total - len(items); extra > 0 {
		title += fmt.Sprintf("（このメッセージは先頭 %d 件です。残り %d 件は次のメッセージで続きます）", len(items), extra)
	}
	if m.WindowMinutes > 0 {
		title = fmt.Sprintf("直近 %d 分 %s", m.WindowMinutes, title)
	}
	return title
}

// telegramEscape は HTML のテキストをエスケープします。
// Telegram が認識するのはこの 3 つのエンティティだけです。エスケープ後、既存の &amp; のようなエンティティは二重にエスケープされます。それが
// 期待する動作です。出すのは元の文字であり、ユーザーに HTML を注入させません。
func telegramEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// telegramEscapeAttr は HTML 属性値をエスケープします。テキストのエスケープに加え、引用符も処理します。
// URL 内の引用符は href 属性を早く閉じ、その後の内容を注入点にします。
func telegramEscapeAttr(s string) string {
	s = telegramEscape(s)
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return s
}
