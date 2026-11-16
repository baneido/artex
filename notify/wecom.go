package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// weComMarkdownLimit は企業 WeChat（WeCom）グループロボットの markdown content の上限です（バイト。文字数ではない）。
// 6 チャネルのうち最も厳しい上限で、TruncateBytes が必要な主な理由です。
const weComMarkdownLimit = 4096

// weComChannel は企業 WeChat のグループロボットを実装します。
//
// プラットフォームの特性:
//   - 認証は URL 上の key だけで、署名の追加には対応していません。Webhook アドレスそのものが認証情報のすべてです。
//   - markdown content の上限は 4096 バイトです。超えるとメッセージ全体が拒否されます（途中では切られません）。中国語は 1 字 3 バイトなので、
//     本文に書けるのは千字あまりになり、クライアント側で切る必要があります。
//   - 制限は 20 件/分です。これもクライアント側の制限で受けます。
type weComChannel struct{}

func (weComChannel) Kind() string { return KindWeCom }

func (weComChannel) DefaultRatePerMin() int { return 20 }

// 企業 WeChat の認証情報は Webhook の 1 か所（URL 上の key）だけで、署名の追加には対応していません。
// アドレス全体が認証情報であり、他にマスクするフィールドはありません。
func (weComChannel) SecretKeys() []string { return []string{"webhook"} }

// WeCom のフィールドは Webhook の 1 つだけで、宛先でもあり認証情報でもあります。アドレスを変えたあとに残る認証情報はありません。
func (weComChannel) DestinationKeys() []string { return []string{"webhook"} }

func (weComChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("Webhook アドレスがありません")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("Webhook アドレスが無効です: %w", err)
	}
	return nil
}

func (c weComChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	// まとめのバッチは長くなることがあります（50 件 × 1 行 + 接頭辞）。4096 バイトはすぐ超えます。
	// 切り詰めはここで行い、プラットフォームのエラーには頼りません。拒否されるとこのバッチは全部失われますが、切り詰めれば先頭の数件は届きます。
	content, kept := markdownBody(m, weComMarkdownLimit)
	payload := map[string]any{
		"msgtype":  "markdown",
		"markdown": map[string]any{"content": content},
	}
	raw, err := doJSON(ctx, "POST", cfgString(cfg, "webhook"), nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("企業 WeChat の応答を解析できませんでした: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 45009 は API 呼び出しが上限を超えたことを示します。プラットフォームの制限ウィンドウは回転するので、待って再試行すれば届きます。
		// そのため再試行可能にします。ここへ来るのはクライアントの rate_per_min が厳しすぎるときで、
		// 再試行は保険です。直すにはこのチャネルの制限値を下げます。
		if res.ErrCode == 45009 {
			return 0, fmt.Errorf("企業 WeChat が制限しています %d: %s", res.ErrCode, res.ErrMsg)
		}
		// 93000 は webhook key が無効であることを示します。永続的な失敗で、再試行では直りません。
		return 0, Permanent(fmt.Errorf("企業 WeChat がエラー %d を返しました: %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}
