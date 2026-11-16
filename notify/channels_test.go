package notify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

// singleMsg は、引用符と改行を含む 1 件のメッセージを作ります。タイトルと要約に `"` と `\n` を意図して入れます。
// テンプレートの埋め込みが、不正な JSON を最も出しやすい入力です。
func singleMsg() Message {
	return Message{
		Items: []Item{{
			FindingID: 42,
			Name:      `ログイン箇所の "SQLインジェクション" リスク`,
			VulnClass: "SQLインジェクション",
			Severity:  "high",
			Summary:   "パラメータ id\n未フィルタのため注入",
			Assets:    []string{"a.example.com", "b.example.com"},
			DetailURL: "https://artex.local/function/findings/detail?id=42",
		}},
	}
}

// batchMsg は、まとめ用のメッセージを作ります。名前の「漏洞」は、マルチバイトで長さ上限を超えるためのフィクスチャです。
func batchMsg(n int) Message {
	m := Message{Batch: true, WindowMinutes: 30, HomeURL: "https://artex.local/function/findings"}
	for i := 0; i < n; i++ {
		m.Items = append(m.Items, Item{
			FindingID: int64(i + 1),
			Name:      "漏洞" + itoa(i+1),
			VulnClass: "XSS",
			Severity:  "medium",
			Summary:   "反射型跨站脚本",
			Assets:    []string{"target.example.com"},
		})
	}
	return m
}

// capturePost は仮の受信端を立て、受け取った本文とヘッダーを断言関数へ渡します。
func capturePost(t *testing.T, respBody string, assert func(t *testing.T, body map[string]any, r *http.Request)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("リクエスト本文が正当な JSON ではありません: %v\n原文: %s", err, raw)
			}
		}
		if assert != nil {
			assert(t, body, r)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respBody)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDingTalkSendsActionCardWhenLinkPresent(t *testing.T) {
	srv := capturePost(t, `{"errcode":0,"errmsg":"ok"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["msgtype"] != "actionCard" {
			t.Fatalf("戻りリンクがあるときは actionCard を送る必要があります。実際は %v です", body["msgtype"])
		}
		card, _ := body["actionCard"].(map[string]any)
		if card["singleURL"] != "https://artex.local/function/findings/detail?id=42" {
			t.Errorf("戻りリンクが失われています: %v", card["singleURL"])
		}
	})
	if _, err := (dingTalkChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, singleMsg()); err != nil {
		t.Fatalf("配信に失敗しました: %v", err)
	}
}

func TestDingTalkFallsBackToMarkdownForBatch(t *testing.T) {
	srv := capturePost(t, `{"errcode":0,"errmsg":"ok"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["msgtype"] != "markdown" {
			t.Fatalf("まとめメッセージは markdown を送る必要があります。実際は %v です", body["msgtype"])
		}
		md, _ := body["markdown"].(map[string]any)
		if !strings.Contains(md["text"].(string), "直近 30 分") {
			t.Errorf("まとめ本文に時間窓がありません: %v", md["text"])
		}
	})
	if _, err := (dingTalkChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, batchMsg(3)); err != nil {
		t.Fatalf("配信に失敗しました: %v", err)
	}
}

// TestDingTalkBusinessErrorIsPermanent は「HTTP 200 でも errcode が 0 でない」判定を固定します。
// errcode を見ないと、配信失敗を成功と記録します。国内の IM プラットフォームに共通する落とし穴です。
func TestDingTalkBusinessErrorIsPermanent(t *testing.T) {
	srv := capturePost(t, `{"errcode":310000,"errmsg":"keywords not in content"}`, nil)
	_, err := (dingTalkChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, singleMsg())
	if err == nil {
		t.Fatal("errcode が 0 でないときはエラーになる必要があります")
	}
	if !IsPermanent(err) {
		t.Fatalf("キーワード不一致は設定の誤りです。永久失敗である必要があります。実際は %v です", err)
	}
	if !strings.Contains(err.Error(), "310000") {
		t.Errorf("エラー情報にプラットフォームのエラーコードが必要です。実際は %v です", err)
	}
}

func TestWeComTruncatesCJKWithinByteLimit(t *testing.T) {
	var contentLen int
	srv := capturePost(t, `{"errcode":0,"errmsg":"ok"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		md, _ := body["markdown"].(map[string]any)
		content, _ := md["content"].(string)
		contentLen = len(content)
		if !utf8.ValidString(content) {
			t.Fatal("切り詰め後が正当な UTF-8 ではありません。企業 WeChat はメッセージ全体を拒否します")
		}
	})
	// 十分に長い中国語のまとめを作ります。4096 バイトを必ず超えます。
	m := batchMsg(200)
	if _, err := (weComChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, m); err != nil {
		t.Fatalf("配信に失敗しました: %v", err)
	}
	if contentLen > weComMarkdownLimit {
		t.Fatalf("本文は %d バイトで、企業 WeChat の上限 %d を超えています", contentLen, weComMarkdownLimit)
	}
	if contentLen == 0 {
		t.Fatal("本文が空です")
	}
}

func TestWeComRateLimitIsRetryableButKeyErrorIsPermanent(t *testing.T) {
	limited := capturePost(t, `{"errcode":45009,"errmsg":"api freq out of limit"}`, nil)
	_, err := (weComChannel{}).Send(context.Background(), map[string]any{"webhook": limited.URL}, singleMsg())
	if err == nil || IsPermanent(err) {
		t.Fatalf("45009 はローリングウィンドウの制限です。再試行できる必要があります。実際は %v です", err)
	}

	badKey := capturePost(t, `{"errcode":93000,"errmsg":"invalid webhook url"}`, nil)
	_, err = (weComChannel{}).Send(context.Background(), map[string]any{"webhook": badKey.URL}, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("93000 は key が無効です。再試行では直りません。永久失敗である必要があります。実際は %v です", err)
	}
}

func TestFeishuCardStructureAndSign(t *testing.T) {
	const secret = "SECtest123"
	srv := capturePost(t, `{"code":0,"msg":"success"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["msg_type"] != "interactive" {
			t.Fatalf("対話式カードを送る必要があります。実際は %v です", body["msg_type"])
		}
		card, _ := body["card"].(map[string]any)
		header, _ := card["header"].(map[string]any)
		if header["template"] != "orange" {
			t.Errorf("high の配色は orange である必要があります。実際は %v です", header["template"])
		}
		// secret があるときは署名パラメータが必要です。無いと Feishu は 19021 で拒否します。
		if body["sign"] == nil || body["timestamp"] == nil {
			t.Fatalf("署名パラメータがありません: %v", body)
		}
		// カードの要素にボタンが 1 つあり、その url は脆弱性の詳細を指します。
		elements, _ := card["elements"].([]any)
		foundButton := false
		for _, e := range elements {
			em, _ := e.(map[string]any)
			if em["tag"] != "action" {
				continue
			}
			actions, _ := em["actions"].([]any)
			for _, a := range actions {
				am, _ := a.(map[string]any)
				if am["url"] == "https://artex.local/function/findings/detail?id=42" {
					foundButton = true
				}
			}
		}
		if !foundButton {
			t.Fatal("カードに、詳細ページを指すボタンがありません")
		}
	})
	cfg := map[string]any{"webhook": srv.URL, "secret": secret}
	if _, err := (feishuChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("配信に失敗しました: %v", err)
	}
}

func TestFeishuWithoutSecretOmitsSign(t *testing.T) {
	srv := capturePost(t, `{"code":0,"msg":"success"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["sign"] != nil || body["timestamp"] != nil {
			t.Fatalf("secret が無いときは署名パラメータを付けてはいけません: %v", body)
		}
	})
	if _, err := (feishuChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, singleMsg()); err != nil {
		t.Fatalf("配信に失敗しました: %v", err)
	}
}

func TestTelegramEscapesHTMLInUntrustedContent(t *testing.T) {
	var text string
	srv := capturePost(t, `{"ok":true}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		text, _ = body["text"].(string)
		if body["parse_mode"] != "HTML" {
			t.Fatalf("HTML の解析モードである必要があります。実際は %v です", body["parse_mode"])
		}
	})
	m := Message{Items: []Item{{
		Severity: "high",
		// タイトルと要約は、対象やモデルの出力から来ます。信頼できない内容です。
		Name:    `<script>alert(1)</script>`,
		Summary: "a & b < c",
	}}}
	if _, err := (telegramChannel{}).Send(context.Background(),
		map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": srv.URL}, m); err != nil {
		t.Fatalf("配信に失敗しました: %v", err)
	}
	if strings.Contains(text, "<script>") {
		t.Fatalf("HTML がエスケープされていません。注入できます: %q", text)
	}
	if !strings.Contains(text, "&lt;script&gt;") {
		t.Fatalf("エスケープ後のエンティティが必要です。実際は %q です", text)
	}
	if !strings.Contains(text, "a &amp; b") {
		t.Fatalf("& がエスケープされていません。実際は %q です", text)
	}
}

func TestTelegramErrorClassification(t *testing.T) {
	rateLimited := capturePost(t, `{"ok":false,"error_code":429,"description":"Too Many Requests"}`, nil)
	_, err := (telegramChannel{}).Send(context.Background(),
		map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": rateLimited.URL}, singleMsg())
	if err == nil || IsPermanent(err) {
		t.Fatalf("429 は再試行できる必要があります。実際は %v です", err)
	}

	forbidden := capturePost(t, `{"ok":false,"error_code":403,"description":"bot was blocked by the user"}`, nil)
	_, err = (telegramChannel{}).Send(context.Background(),
		map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": forbidden.URL}, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("403 は設定の問題です。永久失敗である必要があります。実際は %v です", err)
	}
}

func TestWebhookDefaultTemplateProducesValidJSON(t *testing.T) {
	// 既定テンプレートがある理由です。タイトルに引用符と改行があると、素朴な
	// `"title": "{{.Title}}"` は不正な JSON を出します。{{json .}} は出しません。
	srv := capturePost(t, `{"ok":true}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["title"] != `[🟠 高] ログイン箇所の "SQLインジェクション" リスク` {
			t.Errorf("タイトルが正しく戻っていません: %v", body["title"])
		}
		items, _ := body["items"].([]any)
		if len(items) != 1 {
			t.Fatalf("items の数は 1 である必要があります。実際は %d です", len(items))
		}
		it, _ := items[0].(map[string]any)
		if it["summary"] != "パラメータ id\n未フィルタのため注入" {
			t.Errorf("要約が正しく戻っていません: %v", it["summary"])
		}
		// 数値は JSON の数値である必要があります。文字列になってはいけません（json:"...,string" のような書き方が、この落とし穴に入ります）。
		if _, ok := it["finding_id"].(float64); !ok {
			t.Errorf("finding_id は数値である必要があります。実際は %T です", it["finding_id"])
		}
	})
	if _, err := (webhookChannel{}).Send(context.Background(), map[string]any{"url": srv.URL}, singleMsg()); err != nil {
		t.Fatalf("配信に失敗しました: %v", err)
	}
}

func TestWebhookCustomTemplateAndHeaders(t *testing.T) {
	srv := capturePost(t, `{"ok":true}`, func(t *testing.T, body map[string]any, r *http.Request) {
		if r.Header.Get("X-Token") != "s3cret" {
			t.Errorf("独自ヘッダーが失われています: %v", r.Header)
		}
		if body["msg"] != "3 件" {
			t.Errorf("独自テンプレートの描画が違います: %v", body["msg"])
		}
		if body["first"] != "漏洞1" {
			t.Errorf("range の取り出しが違います: %v", body["first"])
		}
	})
	cfg := map[string]any{
		"url":           srv.URL,
		"headers":       map[string]any{"X-Token": "s3cret"},
		"body_template": `{"msg": {{json (printf "%d 件" .Count)}}, "first": {{json (index .Items 0).Name}}}`,
	}
	if _, err := (webhookChannel{}).Send(context.Background(), cfg, batchMsg(3)); err != nil {
		t.Fatalf("配信に失敗しました: %v", err)
	}
}

func TestWebhookRejectsNonJSONRenderResult(t *testing.T) {
	cfg := map[string]any{"url": "https://example.com/hook", "body_template": `not json at all`}
	_, err := (webhookChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("描画結果が JSON でないときは永久失敗である必要があります（テンプレートの誤りで、再試行は無意味です）。実際は %v です", err)
	}
}

func TestWebhookValidateCatchesBadConfigEarly(t *testing.T) {
	bad := []map[string]any{
		{},
		{"url": "file:///etc/passwd"},
		{"url": "https://example.com", "method": "DELETE"},
		{"url": "https://example.com", "body_template": `{{.Items.`},
	}
	for i, cfg := range bad {
		if err := (webhookChannel{}).Validate(cfg); err == nil {
			t.Errorf("%d 組目の設定は拒否される必要があります: %v", i, cfg)
		}
	}
}

func TestEmailMessageIsWellFormed(t *testing.T) {
	msg, err := buildEmailMessage("artex@example.com", []string{"a@example.com", "b@example.com"}, singleMsg())
	if err != nil {
		t.Fatalf("メールの組み立てに失敗しました: %v", err)
	}
	if !strings.HasPrefix(msg, "From: artex@example.com\r\n") {
		t.Fatalf("From ヘッダーが違います:\n%s", msg)
	}
	if !strings.Contains(msg, "To: a@example.com, b@example.com\r\n") {
		t.Fatalf("To ヘッダーが違います:\n%s", msg)
	}
	// 日本語の件名は RFC 2047 でエンコードします。しないと、クライアントは文字化けして表示します。
	if !strings.Contains(msg, "Subject: =?utf-8?") {
		t.Fatalf("件名が RFC 2047 でエンコードされていません:\n%s", msg)
	}
	if dec, err := new(mime.WordDecoder).DecodeHeader(mustExtractHeader(t, msg, "Subject")); err != nil {
		t.Fatalf("件名をデコードできません: %v", err)
	} else if !strings.Contains(dec, "SQLインジェクション") {
		t.Fatalf("件名をデコードした内容が違います: %q", dec)
	}

	// 本文は base64 です。解くと正当な HTML になります。
	parts := strings.SplitN(msg, "\r\n\r\n", 2)
	if len(parts) != 2 {
		t.Fatal("メールにヘッダーと本文の区切りがありません")
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(strings.TrimSpace(parts[1]), "\r\n", ""))
	if err != nil {
		t.Fatalf("本文の base64 デコードに失敗しました: %v", err)
	}
	html := string(decoded)
	if !strings.HasPrefix(html, "<div") {
		t.Fatalf("本文が HTML ではありません: %.80s", html)
	}
	// タイトルはテキストの位置にそのまま出ます。HTML のテキスト内容では、二重引用符は正当な文字で、エスケープは不要です。
	// 「そのまま残る」と断言するのは、あとから引用符のエスケープを 1 層足して、引用符が
	// &quot; と表示されるのを防ぐためです。
	if !strings.Contains(html, `"SQLインジェクション"`) {
		t.Fatalf("タイトルの引用符は、テキストの位置ではそのまま残る必要があります: %.200s", html)
	}
}

// TestEmailEscapesStructuralInjection は、メール本文が本当に防ぐ注入を覆います。
// 脆弱性のタイトルと要約は、対象とモデルの出力から来ます。信頼できない内容です。テキストの位置では
// & < > をエスケープします（しないとタグを注入できます）。属性の位置では引用符もエスケープします（しないと href を閉じられます）。
func TestEmailEscapesStructuralInjection(t *testing.T) {
	m := Message{
		Items: []Item{{
			Severity:  "high",
			Name:      `<script>alert(1)</script>`,
			Summary:   "a & b > c",
			DetailURL: `https://artex.local/x?a="onmouseover=alert(1)`,
		}},
	}
	html := htmlBody(m, 0)
	if strings.Contains(html, "<script>") {
		t.Fatalf("タイトルがエスケープされていません。タグを注入できます: %s", html)
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Fatalf("エスケープ後のエンティティが必要です: %s", html)
	}
	if !strings.Contains(html, "a &amp; b &gt; c") {
		t.Fatalf("& と > がエスケープされていません: %s", html)
	}
	// 戻りリンクは、管理者が設定できる public_base_url です。信頼度は高いですが、属性の位置では
	// 引用符をエスケープします。引用符を含むアドレスが href を閉じ、イベントハンドラを注入できます。
	if strings.Contains(html, `onmouseover=alert(1)">`) {
		t.Fatalf("href 属性が正しくエスケープされていません: %s", html)
	}
	if !strings.Contains(html, "&quot;") {
		t.Fatalf("属性の位置の引用符はエスケープされる必要があります: %s", html)
	}
}

func mustExtractHeader(t *testing.T, msg, name string) string {
	t.Helper()
	for _, line := range strings.Split(msg, "\r\n") {
		if strings.HasPrefix(line, name+": ") {
			return strings.TrimPrefix(line, name+": ")
		}
	}
	t.Fatalf("%s ヘッダーが見つかりません", name)
	return ""
}

func TestChannelValidateReportsMissingFields(t *testing.T) {
	// 検証エラーは設定する人に直接出ます。何が欠けているかを具体的に言います。「設定が無効」のような漠然とした文では足りません。
	cases := []struct {
		kind   string
		cfg    map[string]any
		substr string
	}{
		{KindDingTalk, map[string]any{}, "Webhook"},
		{KindFeishu, map[string]any{}, "Webhook"},
		{KindWeCom, map[string]any{}, "Webhook"},
		{KindTelegram, map[string]any{}, "Bot Token"},
		{KindTelegram, map[string]any{"bot_token": "t"}, "Chat ID"},
		{KindEmail, map[string]any{}, "SMTP"},
		{KindEmail, map[string]any{"host": "h"}, "ポート"},
		{KindEmail, map[string]any{"host": "h", "port": 587, "from": "f"}, "宛先"},
	}
	for _, tc := range cases {
		ch, ok := Get(tc.kind)
		if !ok {
			t.Fatalf("チャネル %s は登録されていません", tc.kind)
		}
		err := ch.Validate(tc.cfg)
		if err == nil {
			t.Errorf("%s の設定 %v は検証に失敗する必要があります", tc.kind, tc.cfg)
			continue
		}
		if !strings.Contains(err.Error(), tc.substr) {
			t.Errorf("%s のエラー情報は %q に触れる必要があります。実際は %q です", tc.kind, tc.substr, err.Error())
		}
	}
}

// TestEmailSMTPErrorClassification は、SMTP の 4xx と 5xx の意味の区別を固定します。
// 4xx も永久失敗にすると、グレーリストを有効にしたメールサーバーは、すべての配信が最初の
// 試行のあと failed に落ちます。グレーリストは、自動再試行が最も効く場面です。
func TestEmailSMTPErrorClassification(t *testing.T) {
	cases := []struct {
		reply     string
		permanent bool
	}{
		{"450 4.7.1 Greylisting in action, please come back later", false},
		{"451 4.3.0 Temporary system failure", false},
		{"452 4.2.2 Mailbox full", false},
		{"550 5.1.1 User unknown", true},
		{"553 5.1.3 Bad address syntax", true},
		{"554 5.7.1 Relay access denied", true},
		// 応答コードが取れないときは「再試行できる」として扱います。1 回多く試します。一瞬の
		// 障害を、直らない障害にしてはいけません。
		{"unexpected EOF", false},
		{"", false},
	}
	for _, tc := range cases {
		err := smtpStageError("宛先が拒否された", errors.New(tc.reply))
		if got := IsPermanent(err); got != tc.permanent {
			t.Errorf("応答 %q: 期待する permanent=%v、実際は %v です", tc.reply, tc.permanent, got)
		}
		// 分類がどちらでも、原文は使う人が調査できるよう残します。
		if tc.reply != "" && !strings.Contains(err.Error(), tc.reply) {
			t.Errorf("応答 %q の原文が捨てられています: %v", tc.reply, err)
		}
	}
}

func TestRegistryCoversAllKinds(t *testing.T) {
	// 6 つのチャネルは 1 つも欠けてはいけません。1 つ欠けると、UI の選択肢から静かに消えます。
	want := []string{KindDingTalk, KindEmail, KindFeishu, KindTelegram, KindWebhook, KindWeCom}
	got := Kinds()
	if len(got) != len(want) {
		t.Fatalf("チャネル数は %d である必要があります。実際は %d です: %v", len(want), len(got), got)
	}
	for _, k := range want {
		if !ValidKind(k) {
			t.Errorf("チャネル %s は登録されていません", k)
		}
		if ch, ok := Get(k); !ok || ch.Kind() != k {
			t.Errorf("チャネル %s の Kind() が登録キーと一致しません", k)
		}
	}
	if ValidKind("nope") {
		t.Error("登録されていない種別が検証を通ってはいけません")
	}
}

func TestPermanentErrorUnwrap(t *testing.T) {
	base := &permanentSentinel{}
	err := Permanent(base)
	if !IsPermanent(err) {
		t.Fatal("永久失敗と識別される必要があります")
	}
	if !strings.Contains(err.Error(), "sentinel") {
		t.Fatalf("エラー情報は下位の内容を通す必要があります: %v", err)
	}
	if Permanent(nil) != nil {
		t.Fatal("Permanent(nil) は nil を返す必要があります")
	}
	if IsPermanent(nil) {
		t.Fatal("nil は永久失敗ではありません")
	}
}

type permanentSentinel struct{}

func (*permanentSentinel) Error() string { return "sentinel" }
