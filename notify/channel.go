package notify

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
)

// Channel は通知チャネルのアダプタです。実装は状態を持ってはいけません。同じインスタンスを複数のチャネル
// 設定が並行して使い回し、認証情報は必ず cfg 引数から渡します。
type Channel interface {
	// Kind はチャネル種別の識別子を返します。登録表のキーと一致させます。
	Kind() string
	// Validate は設定の保存時に呼び、必須フィールドと形式を検証します。返したエラーはそのまま
	// 設定者に出るので、文面は「どのフィールドが無いか」を書き、漠然と「設定が無効」とは書きません。
	Validate(cfg map[string]any) error
	// Send はメッセージを 1 回配信し、実際に届いた件数とエラーを返します。
	//
	// 件数を返す理由: 各プラットフォームにはメッセージ長の上限があり、まとめが 1 バッチに入らないときは切れます。
	// 呼び出し側が無条件にバッチ全体を届いたことにすると、切れた項目は消えます。メッセージには無く、
	// 配信履歴は成功になり、脆弱性が一度も出ていないことを示す場所がありません。kept を返すと、
	// 呼び出し側は先頭の kept 件だけに印を付け、残りは次のバッチに残します。
	//
	// エラーは配信失敗です。*PermanentError は再試行してはいけない失敗です。
	// 失敗のとき kept に意味は無く、呼び出し側は無視します。
	Send(ctx context.Context, cfg map[string]any, m Message) (int, error)
	// DefaultRatePerMin は、そのチャネルの公式が勧める 1 分あたりの配信上限を返します。チャネルを新規作成するときの
	// 既定の制限値です。0 は既知の制限が無いことを示します。
	DefaultRatePerMin() int
	// SecretKeys は、そのチャネル設定のうち認証情報にあたるキー名を返します。API の表示ではこれらの値をマスクし、
	// 更新時にマスク値を受け取ったら保存済みの元の値を残します。どのフィールドが認証情報かは実装だけが知っています
	// （企業 WeChat は Webhook アドレス全体が認証情報で、DingTalk は secret だけです）。
	// この知識はチャネルが提供し、上位が推測してはいけません。
	SecretKeys() []string
	// DestinationKeys は、そのチャネル設定のうち「メッセージをどこへ送るか」を決めるキー名を返します。
	//
	// SecretKeys と同じく安全に関わるものです。宛先アドレスと認証情報は別々のフィールドで、
	// 「アドレスだけ変えて、認証情報はそのまま残す」を許すと、チャネル設定を変えられる人が保存済みの本物の認証情報を
	// 自分の制御するサーバーへ送れます。チャネル設定のマスクが意味を失います。
	// PrepareConfigUpdate を参照してください。
	DestinationKeys() []string
}

// registry はチャネルの登録表です。init() の自己登録ではなく、意図して明示的なリテラルを使います。「どのチャネルがあるか」を
// 1 か所で見渡せ、チャネルを足し忘れたときは実行時の副作用ではなくコンパイル時に分かります。
var registry = map[string]Channel{
	KindDingTalk: dingTalkChannel{},
	KindFeishu:   feishuChannel{},
	KindWeCom:    weComChannel{},
	KindWebhook:  webhookChannel{},
	KindTelegram: telegramChannel{},
	KindEmail:    emailChannel{},
}

// Get は種別に対応するチャネル実装を取ります。
func Get(kind string) (Channel, bool) {
	c, ok := registry[kind]
	return c, ok
}

// ValidKind は、kind が対応しているチャネル種別かを報告します。
func ValidKind(kind string) bool {
	_, ok := registry[kind]
	return ok
}

// Kinds は対応しているチャネル種別をすべて返します。辞書順です（UI のドロップダウンを安定して出すため）。
func Kinds() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// PermanentError は、再試行してはいけない配信失敗を示します。認証情報の誤り、宛先の拒否、リクエスト本文の不正などです。
// 再試行が意味を持つのは一時的な障害（ネットワークの揺れ、制限、相手の 5xx）だけです。永続的な失敗を退避しながら繰り返すと、
// 成功せず、本当のエラーが再試行ログに埋もれます。
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent は err を永続的な失敗として印を付けます。err が nil のときは nil を返し、
// `return Permanent(someCheck())` と書けます。
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &PermanentError{Err: err}
}

// IsPermanent は、err の連鎖に永続的な失敗の印があるかを報告します。
func IsPermanent(err error) bool {
	var pe *PermanentError
	return errors.As(err, &pe)
}

// ---- 設定読み取りの helper ----
//
// チャネル設定はデータベースの JSONB 列から来て、encoding/json で復元すると map[string]any になります。
// 数値はすべて float64、配列は []any です。以下の helper はこの変換をまとめ、ユーザーが
// UI で空欄にしたときの型のずれ（ポートを文字列で入れるなど）も受けます。

// cfgString は文字列の設定項目を取ります。前後の空白はすべて切ります。Web フォームからのコピーには空白が付きやすいです。
func cfgString(cfg map[string]any, key string) string {
	v, ok := cfg[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

// cfgInt は整数の設定項目を取ります。float64（JSON の既定）と文字列の両方を受けます。
func cfgInt(cfg map[string]any, key string) int {
	switch v := cfg[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}

// cfgBool は真偽の設定項目を取ります。文字列 "true"/"1" も受けます。
func cfgBool(cfg map[string]any, key string) bool {
	switch v := cfg[key].(type) {
	case bool:
		return v
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		return s == "true" || s == "1" || s == "yes"
	default:
		return false
	}
}

// cfgStrings は文字列配列の設定項目を取ります。空白を切り、空文字は捨てます。
func cfgStrings(cfg map[string]any, key string) []string {
	raw, ok := cfg[key].([]any)
	if !ok {
		// 文字列 1 つも受けます。値が 1 つのフォーム送信を楽にするためです。
		if s := cfgString(cfg, key); s != "" {
			return []string{s}
		}
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// cfgMap は文字列マップの設定項目（カスタム HTTP ヘッダーなど）を取ります。キーと値の空白を切り、空のキーは捨てます。
func cfgMap(cfg map[string]any, key string) map[string]string {
	raw, ok := cfg[key].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		out[k] = s
	}
	return out
}
