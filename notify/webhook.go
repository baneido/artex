package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"text/template"
	"time"
)

// webhookChannel は汎用 Webhook アダプタです。URL、メソッド、ヘッダー、JSON テンプレートをユーザーが指定します。
// これがあるので、Slack、Mattermost、Discord、自前システムのために実装を分けずに済みます。
// それらのプラットフォームは、設定できるテンプレート 1 つで覆えます。
type webhookChannel struct{}

func (webhookChannel) Kind() string { return KindWebhook }

// 汎用 Webhook に公式の制限はありません。0 は既定で制限しないことを示し、相手の能力に合わせて利用者が決めます。
func (webhookChannel) DefaultRatePerMin() int { return 0 }

// url と headers をマスクします。宛先アドレス自体に token が付くことが多く、カスタムヘッダーには通常認証情報が入ります。
// どちらも API の表示に出るので、両方止めます。
// 代償として、編集時にヘッダーの 1 つを変えたいときはヘッダー一式を入れ直します（マスク値は「元の値を保つ」と解釈されます）。
// この取捨は意図したものです。ブラウザへ認証情報を出さず、1 回多く記入します。
func (webhookChannel) SecretKeys() []string { return []string{"url", "headers"} }

// 宛先は url です。url を変えるときは headers を改めて指定します。指定しないと、元の Authorization ヘッダーが
// そのまま新しいアドレスへ送られます。これがマスク回避の主な経路です。
func (webhookChannel) DestinationKeys() []string { return []string{"url"} }

// webhookDefaultTemplate は、テンプレート未記入のときのリクエスト本文です。素直な JSON で、
// 「JSON を 1 件受け取って保存する」自前の受信側のほとんどを覆います。
const webhookDefaultTemplate = `{
  "title": {{json .Title}},
  "batch": {{.Batch}},
  "count": {{.Count}},
  "items": [
{{- range $i, $it := .Items}}
{{- if $i}},{{end}}
    {
      "finding_id": {{$it.FindingID}},
      "name": {{json $it.Name}},
      "vulnclass": {{json $it.VulnClass}},
      "severity": {{json $it.Severity}},
      "summary": {{json $it.Summary}},
      "assets": {{json $it.Assets}},
      "detail_url": {{json $it.DetailURL}}
    }
{{- end}}
  ]
}`

// webhookTemplateData は、ユーザーのテンプレートに渡すコンテキストです。
type webhookTemplateData struct {
	Title   string
	Batch   bool
	Count   int
	Items   []webhookItem
	HomeURL string
	// SentAt は今回の配信時刻（RFC3339）で、受信側の記録に使います。
	SentAt string
}

type webhookItem struct {
	FindingID     int64
	Name          string
	VulnClass     string
	Severity      string
	SeverityLabel string
	Summary       string
	Assets        []string
	DetailURL     string
	FromStatus    string
	ToStatus      string
	// StatusLabel は状態変更の読みやすい説明です。例: 「未対応 から 修正済み」。状態変更でないときは空です。
	StatusLabel string
}

func (webhookChannel) Validate(cfg map[string]any) error {
	raw := cfgString(cfg, "url")
	if raw == "" {
		return errors.New("目標 URL がありません")
	}
	if err := validateHTTPURL(raw); err != nil {
		return fmt.Errorf("目標 URL が無効です: %w", err)
	}
	if m := strings.ToUpper(cfgString(cfg, "method")); m != "" && m != http.MethodGet && m != http.MethodPost && m != http.MethodPut && m != http.MethodPatch {
		return fmt.Errorf("メソッド %s には対応していません（使えるのは GET/POST/PUT/PATCH です）", m)
	}
	if tpl := cfgString(cfg, "body_template"); tpl != "" {
		if _, err := parseWebhookTemplate(tpl); err != nil {
			return fmt.Errorf("リクエスト本文テンプレートの構文が正しくありません: %w", err)
		}
	}
	return nil
}

func (c webhookChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	method := strings.ToUpper(cfgString(cfg, "method"))
	if method == "" {
		method = http.MethodPost
	}

	// GET にはリクエスト本文を付けません。内容を query に入れるのはテンプレートの範囲外で、GET の意味にも合いません。
	// GET は「当たったらフックを起動する」種類の受信側に向きます。
	var payload any
	if method != http.MethodGet {
		body, err := renderWebhookBody(cfgString(cfg, "body_template"), m)
		if err != nil {
			return 0, Permanent(err)
		}
		// テンプレートの描画結果は文字列の JSON です。ここでは json.RawMessage にしてそのまま送り、
		// 二重エスケープで、ユーザーが組んだ構造を JSON 文字列の中へ包まないようにします。
		if !json.Valid([]byte(body)) {
			return 0, Permanent(errors.New("リクエスト本文テンプレートの描画結果は正当な JSON ではありません"))
		}
		payload = json.RawMessage(body)
	}

	headers := cfgMap(cfg, "headers")
	if ct := cfgString(cfg, "content_type"); ct != "" {
		// 上書きは許可します。headers の後に適用し、明示した設定を優先します。
		if headers == nil {
			headers = map[string]string{}
		}
		headers["Content-Type"] = ct
	}
	if _, err := doJSON(ctx, method, cfgString(cfg, "url"), headers, payload); err != nil {
		return 0, err
	}
	// 汎用 Webhook は本文を切りません（受信側はユーザー自身のサービスで、大きさは body_template が決めます）。
	// そのためバッチ全体を届いたものとします。
	return len(m.Items), nil
}

// renderWebhookBody は、ユーザーのテンプレート（または既定テンプレート）でリクエスト本文を描画します。
func renderWebhookBody(tpl string, m Message) (string, error) {
	if strings.TrimSpace(tpl) == "" {
		tpl = webhookDefaultTemplate
	}
	t, err := parseWebhookTemplate(tpl)
	if err != nil {
		return "", fmt.Errorf("リクエスト本文テンプレートの構文が正しくありません: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, newWebhookTemplateData(m)); err != nil {
		return "", fmt.Errorf("リクエスト本文テンプレートを描画できませんでした: %w", err)
	}
	return buf.String(), nil
}

// parseWebhookTemplate はテンプレートを解析します。
//
// missingkey=zero は、無い map キーをエラーにせずゼロ値として描画します。このファイルのコンテキストは構造体なので、
// 主な効果は .Items が空のときに range がエラーにならないことです。本当に防ぐ必要があるのは .Items が nil のときです。
func parseWebhookTemplate(tpl string) (*template.Template, error) {
	return template.New("body").Funcs(webhookTemplateFuncs).Option("missingkey=zero").Parse(tpl)
}

// webhookTemplateFuncs は、テンプレートに出す補助関数です。
var webhookTemplateFuncs = template.FuncMap{
	// json は任意の値を JSON にシリアライズします。
	//
	// この関数は飾りではなく必須です。無いと、ユーザーは {{.Title}} で直接埋め込むしかなく、
	// 脆弱性タイトルに引用符や改行が 1 つあるだけで、リクエスト本文全体が正当な JSON でなくなります。受信側は
	// 拒否し、エラーは「JSON の解析に失敗」を指すので、タイトルの引用符が原因だと結び付きません。
	"json": func(v any) (string, error) {
		raw, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return string(raw), nil
	},
	// jsons は、JSON 断片を別の JSON 文字列値の中へ埋めます（文字列エスケープを 1 段かけます）。
	"jsons": func(v any) (string, error) {
		raw, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		quoted, err := json.Marshal(string(raw))
		if err != nil {
			return "", err
		}
		// 外側の引用符を外します。引用符を付けるかは呼び出し側が決めます。
		return string(quoted[1 : len(quoted)-1]), nil
	},
}

func newWebhookTemplateData(m Message) webhookTemplateData {
	d := webhookTemplateData{
		Title:   markdownTitle(m),
		Batch:   m.Batch,
		Count:   len(m.Items),
		HomeURL: m.HomeURL,
		SentAt:  time.Now().Format(time.RFC3339),
		Items:   make([]webhookItem, 0, len(m.Items)),
	}
	for _, it := range m.Items {
		wi := webhookItem{
			FindingID:     it.FindingID,
			Name:          it.Name,
			VulnClass:     it.VulnClass,
			Severity:      it.Severity,
			SeverityLabel: SeverityLabel(it.Severity),
			Summary:       it.Summary,
			Assets:        append([]string{}, it.Assets...),
			DetailURL:     it.DetailURL,
			FromStatus:    it.FromStatus,
			ToStatus:      it.ToStatus,
		}
		if it.IsStatusChange() {
			wi.StatusLabel = StatusLabel(it.FromStatus) + " から " + StatusLabel(it.ToStatus)
		}
		d.Items = append(d.Items, wi)
	}
	return d
}
