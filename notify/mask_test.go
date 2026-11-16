package notify

import (
	"errors"
	"strings"
	"testing"
)

func TestMaskedValueHidesBodyButKeepsTailHint(t *testing.T) {
	const secret = "https://oapi.dingtalk.com/robot/send?access_token=abcdef123456"
	got := MaskedValue(secret)
	if strings.Contains(got, "abcdef123456") {
		t.Fatalf("マスク値が認証情報の全体を漏らしています: %q", got)
	}
	if strings.Contains(got, "oapi.dingtalk.com") {
		t.Fatalf("マスク値がアドレスの本体を出してはいけません: %q", got)
	}
	// 末尾 6 文字は残します。ユーザーがどのロボットかを見分けられます。
	if !strings.HasSuffix(got, "123456") {
		t.Fatalf("見分けの手がかりとして、末尾 6 文字を残す必要があります: %q", got)
	}
	if !IsMasked(got) {
		t.Fatalf("マスク値は IsMasked が識別できる必要があります: %q", got)
	}
}

func TestMaskedValueShortSecretGivesNoHint(t *testing.T) {
	// 短い認証情報でも末尾 6 文字を出すと、認証情報の全体を出すことになります。
	for _, s := range []string{"abc", "abcdef", ""} {
		got := MaskedValue(s)
		if got != MaskedPrefix {
			t.Fatalf("長さ %d の認証情報は末尾の手がかりを出してはいけません。実際は %q です", len(s), got)
		}
		if s != "" && strings.Contains(got, s) {
			t.Fatalf("マスク値に元の値が入っています: %q", got)
		}
	}
}

func TestMaskConfigMasksOnlySecrets(t *testing.T) {
	cfg := map[string]any{
		"webhook": "https://example.com/hook?token=SECRETVALUE",
		"secret":  "SECtest123456",
		"port":    float64(587),
		"host":    "smtp.example.com",
	}
	masked := MaskConfig(KindDingTalk, cfg)
	for _, k := range []string{"webhook", "secret"} {
		s, _ := masked[k].(string)
		if !IsMasked(s) {
			t.Errorf("%s はマスクされる必要があります。実際は %q です", k, s)
		}
	}
	// 認証情報でないフィールドはそのまま残します。残さないと UI に出せません。
	if masked["port"] != float64(587) {
		t.Errorf("認証情報でないフィールド port を変えてはいけません: %v", masked["port"])
	}
}

func TestMaskConfigUnknownKindReturnsEmpty(t *testing.T) {
	// チャネル種別を識別できないときは、UI に空の設定を出します。認証情報を含みうる元の内容を返してはいけません。
	got := MaskConfig("nope", map[string]any{"webhook": "https://x/y?token=LEAK"})
	if len(got) != 0 {
		t.Fatalf("未知のチャネル種別は空の設定を返す必要があります。実際は %v です", got)
	}
}

func TestMaskConfigDoesNotMutateInput(t *testing.T) {
	// マスクは表示の振る舞いです。保存先の本当の値を書き換えてはいけません。
	cfg := map[string]any{"webhook": "https://example.com/hook", "secret": "SECtest123456"}
	_ = MaskConfig(KindDingTalk, cfg)
	if IsMasked(cfg["secret"].(string)) {
		t.Fatal("MaskConfig が引数を書き換えました。本当の認証情報がマスク値で上書きされます")
	}
}

func TestMergeConfigKeepsStoredOnMaskedIncoming(t *testing.T) {
	stored := map[string]any{"webhook": "https://real/hook", "secret": "REALSECRET", "method": "POST"}
	// ユーザーが変えたのは method だけです。ブラウザが送るのはマスク値と新しい method です。
	incoming := map[string]any{
		"webhook": MaskedValue("https://real/hook"),
		"secret":  MaskedValue("REALSECRET"),
		"method":  "PUT",
	}
	got := MergeConfig(stored, incoming)
	if got["webhook"] != "https://real/hook" || got["secret"] != "REALSECRET" {
		t.Fatalf("マスクされたフィールドは、保存済みの値を残す必要があります。実際は %v です", got)
	}
	if got["method"] != "PUT" {
		t.Fatalf("変更されたフィールドは反映される必要があります。実際は %v です", got["method"])
	}
}

func TestMergeConfigEmptyStringClears(t *testing.T) {
	stored := map[string]any{"webhook": "https://real/hook", "secret": "REALSECRET"}
	got := MergeConfig(stored, map[string]any{"secret": ""})
	if _, ok := got["secret"]; ok {
		t.Fatalf("空文字はそのフィールドを消す必要があります。実際は %v です", got)
	}
	// 言及されていないフィールドは残します（部分更新の意味です）。
	if got["webhook"] != "https://real/hook" {
		t.Fatalf("言及されていないフィールドは残す必要があります。実際は %v です", got)
	}
}

func TestMergeConfigKeepsUnmentionedStoredKeys(t *testing.T) {
	stored := map[string]any{"host": "smtp.example.com", "port": float64(587), "password": "pw"}
	got := MergeConfig(stored, map[string]any{"port": float64(465)})
	if got["host"] != "smtp.example.com" || got["password"] != "pw" {
		t.Fatalf("言及されていないフィールドは残す必要があります。実際は %v です", got)
	}
	if got["port"] != float64(465) {
		t.Fatalf("言及されたフィールドは更新される必要があります。実際は %v です", got["port"])
	}
}

// TestPrepareConfigUpdateBlocksDestinationSwap は、このパッケージで最も重要な安全の不変条件です。
// 宛先アドレスを変えるとき、古い認証情報を新しい宛先へ持っていってはいけません。
//
// これらの用例は攻撃の形の入力です（アドレスだけを変え、認証情報には触れません）。
// 防御の正しい入力だけを測ると、防御が効いていなくても全緑になります。
func TestPrepareConfigUpdateBlocksDestinationSwap(t *testing.T) {
	cases := []struct {
		name     string
		kind     string
		stored   map[string]any
		incoming map[string]any
		// wantMissing は、名指しされることが期待される認証情報のキーです。
		wantMissing string
	}{
		{
			name: "汎用 Webhook がアドレスを変え、Authorization ヘッダーを流用しようとする",
			kind: KindWebhook,
			stored: map[string]any{
				"url":     "https://legit.example.com/hook",
				"headers": map[string]any{"Authorization": "Bearer REAL-TOKEN"},
			},
			incoming:    map[string]any{"url": "https://attacker.tld/c"},
			wantMissing: "headers",
		},
		{
			name:        "Telegram が base_url を変え、Bot Token を自分の端点へ送ろうとする",
			kind:        KindTelegram,
			stored:      map[string]any{"bot_token": "123456:REAL", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming:    map[string]any{"base_url": "https://attacker.tld"},
			wantMissing: "bot_token",
		},
		{
			name:        "メールが SMTP ホストを変え、パスワードを渡そうとする",
			kind:        KindEmail,
			stored:      map[string]any{"host": "smtp.corp.com", "port": 587, "password": "REALPW", "from": "a@b.c", "to": []any{"d@e.f"}},
			incoming:    map[string]any{"host": "smtp.attacker.tld"},
			wantMissing: "password",
		},
		{
			name:        "メールが TLS を切るときも、パスワードを改めて示す必要がある",
			kind:        KindEmail,
			stored:      map[string]any{"host": "smtp.corp.com", "port": 587, "tls": false, "password": "REALPW", "from": "a@b.c", "to": []any{"d@e.f"}},
			incoming:    map[string]any{"tls": true},
			wantMissing: "password",
		},
		{
			// マスク値は「古い認証情報を流用する」という意味です。アドレス変更の文脈では、同じように拒否します。
			name:        "マスクした認証情報を戻し、新しいアドレスを付ける",
			kind:        KindTelegram,
			stored:      map[string]any{"bot_token": "123456:REAL", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming:    map[string]any{"base_url": "https://attacker.tld", "bot_token": MaskedValue("123456:REAL")},
			wantMissing: "bot_token",
		},
		{
			name:        "DingTalk が Webhook を変え、署名用シークレットを流用しようとする",
			kind:        KindDingTalk,
			stored:      map[string]any{"webhook": "https://oapi.dingtalk.com/robot/send?access_token=OLD", "secret": "REALSEC"},
			incoming:    map[string]any{"webhook": "https://attacker.tld/hook"},
			wantMissing: "secret",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged, err := PrepareConfigUpdate(tc.kind, tc.stored, tc.incoming)
			if err == nil {
				t.Fatalf("アドレスを変え、認証情報を改めて示していないので、拒否される必要があります。実際の設定は %v です", merged)
			}
			var target *ErrDestinationChangedWithoutCredentials
			if !errors.As(err, &target) {
				t.Fatalf("操作できる案内を出すため、専用のエラー型を返す必要があります。実際は %T: %v です", err, err)
			}
			found := false
			for _, m := range target.Missing {
				if m == tc.wantMissing {
					found = true
				}
			}
			if !found {
				t.Fatalf("欠けている認証情報のキー %q を名指しする必要があります。実際は %v です", tc.wantMissing, target.Missing)
			}
			// エラー情報は、操作する人がどう直すかを案内します。
			if !strings.Contains(err.Error(), tc.wantMissing) {
				t.Errorf("エラー情報は %q に触れる必要があります: %v", tc.wantMissing, err)
			}
		})
	}
}

// TestPrepareConfigUpdateAllowsLegitimateEdits は逆の用例です。正常な編集を誤って止めてはいけません。
// 止めると、この防護は「面倒」を理由に迂回されるか、消されます。
func TestPrepareConfigUpdateAllowsLegitimateEdits(t *testing.T) {
	cases := []struct {
		name     string
		kind     string
		stored   map[string]any
		incoming map[string]any
	}{
		{
			name:     "名前だけを変える（設定はそのまま戻る）",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://legit.example.com/hook", "headers": map[string]any{"Authorization": "Bearer REAL"}},
			incoming: map[string]any{"url": MaskedValue("https://legit.example.com/hook")},
		},
		{
			name:     "リクエストメソッドだけを変える。アドレスと認証情報は動かない",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://legit.example.com/hook", "method": "POST"},
			incoming: map[string]any{"method": "PUT"},
		},
		{
			name:     "アドレスを変え、同時に新しい認証情報を渡す",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://old.example.com/hook", "headers": map[string]any{"Authorization": "Bearer OLD"}},
			incoming: map[string]any{"url": "https://new.example.com/hook", "headers": map[string]any{"Authorization": "Bearer NEW"}},
		},
		{
			name:     "アドレスを変え、認証情報が不要であることを明示する",
			kind:     KindWebhook,
			stored:   map[string]any{"url": "https://old.example.com/hook", "headers": map[string]any{"Authorization": "Bearer OLD"}},
			incoming: map[string]any{"url": "https://new.example.com/hook", "headers": ""},
		},
		{
			name:     "Telegram の chat_id を変える（宛先ではない）",
			kind:     KindTelegram,
			stored:   map[string]any{"bot_token": "t", "chat_id": "1", "base_url": "https://api.telegram.org"},
			incoming: map[string]any{"chat_id": "-100200"},
		},
		{
			name:     "メールの宛先を変える（配信先のホストではない）",
			kind:     KindEmail,
			stored:   map[string]any{"host": "smtp.corp.com", "port": 587, "password": "PW", "from": "a@b.c", "to": []any{"x@y.z"}},
			incoming: map[string]any{"to": []any{"new@y.z"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged, err := PrepareConfigUpdate(tc.kind, tc.stored, tc.incoming)
			if err != nil {
				t.Fatalf("正当な編集が誤って止まりました: %v", err)
			}
			if merged == nil {
				t.Fatal("マージ結果を返す必要があります")
			}
		})
	}
}

// TestPrepareConfigUpdatePortTypeTolerance は、誤判定しやすい細部を覆います。
// フロントが送るポートは JSON の数値（float64）で、保存先から読んだ値も float64 です。
// ただし 2 つの値の型が違うことがあります（int と float64 など）。== で比べると「変えていない」を「変えた」と判定し、
// 名前だけを変えたユーザーに「パスワードを記入し直してください」と出します。誤報は、この防護への信頼を落とします。
func TestPrepareConfigUpdatePortTypeTolerance(t *testing.T) {
	stored := map[string]any{"host": "smtp.corp.com", "port": float64(587), "password": "PW"}
	// 同じポートを int で送ります。
	if _, err := PrepareConfigUpdate(KindEmail, stored, map[string]any{"port": 587}); err != nil {
		t.Fatalf("ポートの値が同じで型だけ違うとき、アドレス変更と判定してはいけません: %v", err)
	}
	// 本当にポートを変えたときは止めます。
	if _, err := PrepareConfigUpdate(KindEmail, stored, map[string]any{"port": 25}); err == nil {
		t.Fatal("ポートの変更は止まる必要があります")
	}
}

// TestPrepareConfigUpdateSurvivesRepeatedSaveWithBlankDestination は、「空にできる
// 宛先フィールド」の経路を覆います。Telegram の base_url を空にすると、公式 API のアドレスを使います。
//
// 以前は、チャネルが 2 回目の保存から永久に保存できなくなっていました。
//
//	新規作成時、保存先には base_url:"" が入ります（作成経路はフロントが送った config を直接保存し、MergeConfig を通りません）
//	→ 1 回目の保存で、MergeConfig は空文字を明示的な消去と見なし、そのキーを delete します
//	→ 2 回目の保存で、incoming は依然 "" ですが、stored にはもうそのキーが無く、「アドレスが変わった」と判定されます
//	→ bot_token はマスクの再表示値です → 400 で、宛先が変わったので認証情報を記入し直すよう求められます
//
// ユーザーは何も変えていないのに、それ以降は保存できません。Bot Token を貼り直すまで戻りません。
func TestPrepareConfigUpdateSurvivesRepeatedSaveWithBlankDestination(t *testing.T) {
	stored := map[string]any{"bot_token": "123:ABC", "chat_id": "-100", "base_url": ""}

	// フロントの buildConfig() は、そのチャネルのフィールド定義ごとに値を送ります。認証情報はマスクを戻し、
	// 空のテキスト欄は空文字を送ります。変えたキーだけを送るのではありません。その出力をそのまま再現します。
	submit := func() map[string]any {
		return map[string]any{
			"bot_token": MaskedValue("123:ABC"),
			"chat_id":   "-100",
			"base_url":  "",
		}
	}

	// 1 回目の保存です。変えたのはチャネル名だけで、config はそのまま戻ります。
	merged, err := PrepareConfigUpdate(KindTelegram, stored, submit())
	if err != nil {
		t.Fatalf("1 回目の保存が誤って止まりました: %v", err)
	}
	if _, ok := merged["base_url"]; ok {
		t.Fatal("前提が変わりました。空文字は MergeConfig が消す必要があります。この用例が覆うのは、キーが消えたあとの段階です")
	}

	// 2 回目の保存です。送る内容は前回と完全に同じで、ユーザーは何も変えていません。
	merged2, err := PrepareConfigUpdate(KindTelegram, merged, submit())
	if err != nil {
		t.Fatalf("2 回目の保存が誤って止まりました（ユーザーは何も変えていません）: %v", err)
	}
	// 3 回目です。1 回だけ間違うのではありません。安定して保存できることを確認します。
	if _, err := PrepareConfigUpdate(KindTelegram, merged2, submit()); err != nil {
		t.Fatalf("3 回目の保存が誤って止まりました: %v", err)
	}
	// 認証情報は最後まで残ります。空文字の論理が一緒に消してはいけません。
	if got := merged2["bot_token"]; got != "123:ABC" {
		t.Fatalf("Bot Token は元の値を流用する必要があります。実際は %v です", got)
	}
}

// TestPrepareConfigUpdateStillGuardsBlankDestinationChanges は、上の用例と対になる
// 断言です。空文字と「キーが無い」を同じと見なしても、本当のアドレス変更を通してはいけません。
// どちらの方向も、認証情報が外へ出る本物の経路です。Telegram の Bot Token は URL のパスに乗り、
// base_url を変えると、Token を新しいアドレスへ渡すことになります。
func TestPrepareConfigUpdateStillGuardsBlankDestinationChanges(t *testing.T) {
	// 方向 1。空（公式アドレス）から、自前のアドレスへ変えます。
	official := map[string]any{"bot_token": "123:ABC", "chat_id": "-100"}
	if _, err := PrepareConfigUpdate(KindTelegram, official, map[string]any{
		"bot_token": MaskedValue("123:ABC"),
		"base_url":  "https://tg-proxy.attacker.tld",
	}); err == nil {
		t.Fatal("公式アドレスから自前のアドレスへ変えるときは、Token の再記入を求める必要があります")
	}

	// 方向 2。自前のアドレスを空にする（公式 API へ戻す）ことも、アドレスの変更です。
	proxied := map[string]any{"bot_token": "123:ABC", "base_url": "https://proxy.internal/bot"}
	if _, err := PrepareConfigUpdate(KindTelegram, proxied, map[string]any{
		"bot_token": MaskedValue("123:ABC"),
		"base_url":  "",
	}); err == nil {
		t.Fatal("自前のアドレスを空にする（公式 API へ戻す）こともアドレスの変更です。Token の再記入を求める必要があります")
	}
}

func TestDestinationKeysDeclaredForEveryKind(t *testing.T) {
	// SecretKeys と同じです。チャネルが宛先キーの宣言を忘れると、PrepareConfigUpdate はそれを守れません。
	for kind, ch := range registry {
		if len(ch.DestinationKeys()) == 0 {
			t.Errorf("チャネル %s は宛先キーを宣言していません。アドレス変更で認証情報を持ち出す防護が、このチャネルには効きません", kind)
		}
		if len(ch.SecretKeys()) == 0 {
			t.Errorf("チャネル %s は認証情報のキーを宣言していません", kind)
		}
	}
}

func TestSecretKeysDeclaredForEveryKind(t *testing.T) {
	// コンパイラは、各チャネルに SecretKeys の実装を強制しています。ここではさらに、
	// 「マスクで白紙を出しているチャネルが無い」ことを確認します。空のスライスを返すチャネルは、認証情報を平文でブラウザへ返します。
	expect := map[string]bool{
		KindDingTalk: true, KindFeishu: true, KindWeCom: true,
		KindWebhook: true, KindTelegram: true, KindEmail: true,
	}
	for kind, ch := range registry {
		if !expect[kind] {
			t.Errorf("チャネル %s は、テストにマスクの期待が登録されていません", kind)
			continue
		}
		if len(ch.SecretKeys()) == 0 {
			t.Errorf("チャネル %s は認証情報フィールドを 1 つも宣言していません。設定が平文で戻ります", kind)
		}
	}
}

// TestPrepareConfigUpdateRejectsMaskedInContainer は、監査で指摘された穴を覆います。
// マスクの番兵を、文字列でない構造（webhook.headers のようなオブジェクト）へ入れると、
// MergeConfig は「文字列であり、接頭辞が付いている」ものだけをマスクと見なします。リテラルの "__masked__" が
// 本当のヘッダー値として保存されます。その後の認証は静かに無効になり、エラーは出ません。
func TestPrepareConfigUpdateRejectsMaskedInContainer(t *testing.T) {
	stored := map[string]any{
		"url":     "https://legit.example.com/hook",
		"headers": map[string]any{"Authorization": "Bearer REAL"},
	}
	// オブジェクトの内部にマスクの番兵が混ざっています。
	incoming := map[string]any{
		"headers": map[string]any{"Authorization": MaskedPrefix},
	}
	if _, err := PrepareConfigUpdate(KindWebhook, stored, incoming); err == nil {
		t.Fatal("構造の内部に混ざったマスクの番兵は拒否される必要があります（拒否しないと、リテラルが保存されます）")
	}
	// オブジェクト全体の送信（本当の新しい値）は、これまでどおり受け入れます。
	ok := map[string]any{"headers": map[string]any{"Authorization": "Bearer NEW"}}
	if _, err := PrepareConfigUpdate(KindWebhook, stored, ok); err != nil {
		t.Fatalf("新しいリクエストヘッダーの正常な送信を止めてはいけません: %v", err)
	}
}
