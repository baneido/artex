package notify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// このファイルは不変条件のテストです。チャネル実装から出るエラーテキストは、認証情報を含んではいけません。
//
// ファイルを分けた理由です。最初のチャネル用例は成功経路とプラットフォームの業務エラーだけで、
// 転送層の失敗を見ていませんでした。危険なのは転送層のエラー（接続拒否、DNS 失敗、タイムアウト）です。
// http.Client.Do が返す *url.Error は、完全な URL をエラーテキストに入れます。この機能の
// これらのサービスの認証情報は URL の中にあります。認証情報はこの流れで 4 つの出口へ出ます。
//
//	notification_deliveries.last_error  → 平文で保存される
//	GET /api/notify/deliveries の応答    → チャネル設定のマスクを迂回し、ブラウザへ表示される
//	サーバーのログ                       → 外へ残されることが多い
//	テスト送信 API の 502 応答           → フロントに直接出る
//
// そのため、ここでは関数 1 つを測るのではありません。チャネルごとに、必ず失敗するリクエストを 1 回送り、
// エラーテキストにその認証情報が無いことを断言します。

// credentialCases は、「認証情報が URL の中にある」チャネルの形をすべて覆います。
// DingTalk と企業 WeChat は query、Feishu はパスの末尾、Telegram はパスの途中です。
var credentialCases = []struct {
	name   string
	ch     Channel
	cfg    map[string]any
	secret string
}{
	{
		name:   "DingTalk の access_token が query にある",
		ch:     dingTalkChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/robot/send?access_token=" + leakProbeToken},
		secret: leakProbeToken,
	},
	{
		name:   "企業 WeChat の key が query にある",
		ch:     weComChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/cgi-bin/webhook/send?key=" + leakProbeToken},
		secret: leakProbeToken,
	},
	{
		name:   "Feishu の hook id がパスの末尾にある",
		ch:     feishuChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/open-apis/bot/v2/hook/" + leakProbeToken},
		secret: leakProbeToken,
	},
	{
		name:   "Telegram の bot token がパスの途中にある",
		ch:     telegramChannel{},
		cfg:    map[string]any{"bot_token": leakProbeToken, "chat_id": "1", "base_url": "http://127.0.0.1:1"},
		secret: leakProbeToken,
	},
	{
		name:   "DingTalk の署名用シークレット",
		ch:     dingTalkChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/robot/send", "secret": leakProbeToken},
		secret: leakProbeToken,
	},
}

// leakProbeToken は、本物の認証情報ではありえない番兵です。エラーテキストの中を検索します。
const leakProbeToken = "LEAKPROBE0123456789abcdef"

// TestChannelErrorsNeverLeakCredentials は中心の不変条件です。
func TestChannelErrorsNeverLeakCredentials(t *testing.T) {
	for _, tc := range credentialCases {
		t.Run(tc.name, func(t *testing.T) {
			// 必ず失敗する相手です。127.0.0.1:1 は誰も待っておらず、接続拒否の経路を通ります。
			_, err := tc.ch.Send(context.Background(), tc.cfg, Message{
				Items: []Item{{FindingID: 1, Severity: "high", Name: "漏えいプローブ"}},
			})
			if err == nil {
				t.Fatal("到達できないアドレスはエラーになる必要があります")
			}
			assertNoSecret(t, err.Error(), tc.secret)
		})
	}
}

// TestChannelErrorsNeverLeakCredentialsInPermanentPath は永久失敗の分岐を覆います。
// URL 検証の失敗やプラットフォームの業務エラーもエラーテキストを外へ出し、同じように認証情報を含んではいけません。
func TestChannelErrorsNeverLeakCredentialsInPermanentPath(t *testing.T) {
	cases := []struct {
		name string
		ch   Channel
		cfg  map[string]any
	}{
		// アドレスに認証情報があり、形式が不正です。validateHTTPURL と url.Parse の分岐を起こします。
		{"DingTalk のアドレスが不正", dingTalkChannel{}, map[string]any{"webhook": "file:///" + leakProbeToken}},
		{"企業 WeChat のアドレスが不正", weComChannel{}, map[string]any{"webhook": "gopher://" + leakProbeToken}},
		{"Feishu のアドレスが不正", feishuChannel{}, map[string]any{"webhook": "ftp://" + leakProbeToken + "/hook"}},
		{"Telegram API のアドレスが不正", telegramChannel{}, map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": "file://" + leakProbeToken}},
		{"汎用 Webhook のアドレスが不正", webhookChannel{}, map[string]any{"url": "javascript:" + leakProbeToken}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.ch.Send(context.Background(), tc.cfg, Message{Items: []Item{{Severity: "high"}}})
			if err == nil {
				t.Fatal("不正な設定はエラーになる必要があります")
			}
			assertNoSecret(t, err.Error(), leakProbeToken)
		})
	}
}

func assertNoSecret(t *testing.T, text, secret string) {
	t.Helper()
	if strings.Contains(text, secret) {
		t.Fatalf("エラーテキストが認証情報 %q を漏らしています:\n    %s", secret, text)
	}
}

func TestRedactRequestTargetKeepsOnlySchemeAndHost(t *testing.T) {
	cases := map[string]string{
		"https://oapi.dingtalk.com/robot/send?access_token=S1":    "https://oapi.dingtalk.com/…",
		"https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=S2": "https://qyapi.weixin.qq.com/…",
		"https://open.feishu.cn/open-apis/bot/v2/hook/S3":         "https://open.feishu.cn/…",
		"https://api.telegram.org/botS4/sendMessage":              "https://api.telegram.org/…",
		"http://10.0.0.5:8080/hook":                               "http://10.0.0.5:8080/…",
	}
	for in, want := range cases {
		got := redactRequestTarget(in)
		if got != want {
			t.Errorf("redactRequestTarget(%q) = %q、期待は %q です", in, got, want)
		}
		// 伏せた結果自体に、元のアドレスのパスや query の断片が残ってはいけません。
		if parts := strings.SplitN(in, "://", 2); len(parts) == 2 {
			if hostAndRest := strings.SplitN(parts[1], "/", 2); len(hostAndRest) == 2 && hostAndRest[1] != "" {
				if strings.Contains(got, hostAndRest[1]) {
					t.Errorf("伏せたあともパスや query の断片 %q が残っています: %q", hostAndRest[1], got)
				}
			}
		}
	}
	// 解析できない入力は、元の文字列を決して返しません。
	for _, bad := range []string{"", "://", "not a url", "http://"} {
		if got := redactRequestTarget(bad); strings.Contains(got, bad) && bad != "" {
			t.Errorf("解析できない入力 %q が %q として返っています", bad, got)
		}
	}
}

// TestRedactTransportErrorStripsURL は、*url.Error という具体的な型を見ます。
// これは http.Client.Do の戻り値の型であり、漏えいの最初の現場です。
func TestRedactTransportErrorStripsURL(t *testing.T) {
	inner := errors.New("dial tcp 127.0.0.1:1: connect: connection refused")
	uerr := &url.Error{
		Op:  "Post",
		URL: "https://api.telegram.org/bot" + leakProbeToken + "/sendMessage",
		Err: inner,
	}
	got := redactTransportError(uerr)
	assertNoSecret(t, got, leakProbeToken)
	if !strings.Contains(got, "api.telegram.org") {
		t.Errorf("調査のため host は残す必要があります。実際は %q です", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Errorf("調査のため下位の原因は残す必要があります。実際は %q です", got)
	}
	// Op も残します（POST か GET かは調査に意味があります）。
	if !strings.Contains(got, "Post") {
		t.Errorf("操作名は残す必要があります。実際は %q です", got)
	}
}

// TestRedactURLsInTextHandlesFallback は受け皿の経路です。*url.Error ではない独自エラー
// （リダイレクト方針が返すエラーなど）の中のアドレスも、同じように取り除きます。
func TestRedactURLsInTextHandlesFallback(t *testing.T) {
	in := fmt.Sprintf("別のホストへのリダイレクトは拒否します（a.example → http://b.example/bot%s/send）", leakProbeToken)
	got := redactURLsInText(in)
	assertNoSecret(t, got, leakProbeToken)
	if !strings.Contains(got, "http://b.example/…") {
		t.Errorf("アドレスは伏せた形に置き換える必要があります。実際は %q です", got)
	}
	// アドレスを含まないテキストはそのまま残します。
	if plain := "dial tcp: connection refused"; redactURLsInText(plain) != plain {
		t.Error("アドレスを含まないテキストを変えてはいけません")
	}
}

// TestCrossHostRedirectRefused は「認証情報が URL にあり、別ホストへのリダイレクトに従うと、認証情報を渡す」を覆います。
// httptest の 2 つのサービスは 127.0.0.1 の別ポートで待ちます。ポートが違えば Host が違い、
// ちょうど別ホストへのリダイレクトになります。
func TestCrossHostRedirectRefused(t *testing.T) {
	var hit bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/robot/send?access_token="+leakProbeToken, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	_, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": redirector.URL + "/robot/send?access_token=" + leakProbeToken},
		Message{Items: []Item{{Severity: "high"}}})
	if err == nil {
		t.Fatal("別ホストへのリダイレクトは拒否される必要があります")
	}
	if hit {
		t.Fatal("リダイレクト先にアクセスしました。認証情報がリダイレクトとともに外へ出ています")
	}
	assertNoSecret(t, err.Error(), leakProbeToken)
}

// TestSameHostRedirectAllowed は逆の用例です。同じホストへのリダイレクト（末尾のスラッシュを足すなど）は、引き続き使えます。
// 使えないと、正常な作業の流れまで止まります。
func TestSameHostRedirectAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robot/send" {
			// 同じホスト、同じポートへのリダイレクトです。
			http.Redirect(w, r, "/robot/send/", http.StatusTemporaryRedirect)
			return
		}
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer srv.Close()

	if _, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"},
		Message{Items: []Item{{Severity: "high"}}}); err != nil {
		t.Fatalf("同じホストへのリダイレクトを拒否してはいけません: %v", err)
	}
}
