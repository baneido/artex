package notify

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

// このファイルは、関連する 2 つの強化を覆います。
//   (1) 配信先が、サーバーを踏み台にして内部ネットワークやクラウドのメタデータへ届いてはいけません（SSRF）
//   (2) アドレス検証のエラー情報に、アドレス内の認証情報を出してはいけません
//
// テスト環境について。このパッケージの多くの用例は 127.0.0.1 の httptest の仮受信端を使います。
// ガードは既定でそれらを止めます。そのため TestMain で AllowLocalTargetsEnv を開き、
// 下の各 SSRF 用例は自分でそれを消し、既定の拒否を断言します。

func TestMain(m *testing.M) {
	// 通常の用例がローカルの仮受信端へ繋がるようにします。SSRF 用例は自分で一時的に消します。
	_ = os.Setenv(AllowLocalTargetsEnv, "1")
	os.Exit(m.Run())
}

// TestDialGuardRejectsLoopbackByDefault は SSRF 防護の中心の断言です。
// 既定では、ループバックへの配信は接続層で拒否されます。
func TestDialGuardRejectsLoopbackByDefault(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		_, _ = io.WriteString(w, `{"errcode":0}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "") // 逃げ口を閉じる。これが既定の挙動です
	_, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}})
	if err == nil {
		t.Fatal("既定では、ループバックへの配信を許してはいけません")
	}
	if hit {
		t.Fatal("リクエストがこのマシンのサービスに届きました。ガードが効いていません")
	}
	// エラー情報は、どう開けばよいかを案内します（このマシンの SMTP リレーは正当な設定です）。
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("拒否の説明に、明示して開く方法が必要です: %v", err)
	}
}

// TestDialGuardAllowsLoopbackWhenOptedIn は逆の用例です。明示して開いたあとは使えなければなりません。
// 使えないと、このマシンの postfix や内部ネットワークのリレーのような正当な構成が、まとめて使えなくなります。
func TestDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}}); err != nil {
		t.Fatalf("明示して開いたあとは配信できる必要があります: %v", err)
	}
}

func TestIsBlockedDialIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.1.2.3", "::1",
		"169.254.169.254", // クラウドのメタデータ端点です。この関数がある主な理由です
		"169.254.1.1", "fe80::1",
		"0.0.0.0", "::",
		"224.0.0.1", "ff02::1",
		"::ffff:127.0.0.1", // IPv4-mapped は戻してから判定します。戻さないと迂回口になります
		"",
	}
	for _, s := range blocked {
		if !isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s は拒否される必要があります", s)
		}
	}
	// RFC1918 のプライベートネットワークは意図して通します。内部の Mattermost や SMTP リレーは正当な使い方として多いです。
	// この断言がその取捨を固定します。あとから誰かがプライベートネットワークの判定を足すと、ここが失敗し、
	// 静かに構成を壊す前に、意識した決定を求めます。
	allowed := []string{"10.0.0.5", "172.16.3.4", "192.168.1.10", "8.8.8.8", "2606:4700::1111"}
	for _, s := range allowed {
		if isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s は通す必要があります（プライベートネットワークは正当な配信先として多いです）", s)
		}
	}
}

// TestValidateHTTPURLRejectsLiteralPrivateTargets は、設定段階の事前の案内を覆います。
// リテラル IP は保存時に拒否します。最初の配信失敗まで待ってはいけません。
func TestValidateHTTPURLRejectsLiteralPrivateTargets(t *testing.T) {
	t.Setenv(AllowLocalTargetsEnv, "")
	for _, raw := range []string{
		"http://127.0.0.1:8080/hook",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]:8080/hook",
	} {
		if err := validateHTTPURL(raw); err == nil {
			t.Errorf("%s は設定段階で拒否される必要があります", raw)
		}
	}
	// 公開アドレスとプライベートアドレスはそのまま通ります（プライベートはダイヤル段階に残し、そこでは止めません）。
	for _, raw := range []string{"https://oapi.dingtalk.com/robot/send", "http://10.0.0.9/hook"} {
		if err := validateHTTPURL(raw); err != nil {
			t.Errorf("%s は検証を通る必要があります: %v", raw, err)
		}
	}
}

// TestValidateHTTPURLErrorNeverLeaksCredentials は、監査で指摘された、前の回で漏れていた分岐です。
//
// url.Parse が失敗すると *url.Error を返します。その Error() には元のアドレス全体が入ります。
// 前の回は http.Client.Do のエラーだけを伏せ、ここを漏らしました。そのとき足した「永久失敗の経路」の用例
// （file://、gopher://、ftp://）は url.Parse が成功し、scheme の分岐を通ります。
// そのため全緑でも、この経路が安全だとは証明できません。偽の保証でした。
func TestValidateHTTPURLErrorNeverLeaksCredentials(t *testing.T) {
	cases := []string{
		"http://127.0.0.1/%zz?access_token=" + leakProbeToken,         // 不正なパーセントエスケープ
		"https://a.example.com:port/x?access_token=" + leakProbeToken, // ポートが数字ではない
		"http://[::1?access_token=" + leakProbeToken,                  // 括弧が対になっていない
	}
	for _, raw := range cases {
		// この入力が本当に url.Parse を失敗させることを先に確認します。これをしないと、
		// 気づかないまま別の分岐へ進みます（前の回の偽の保証は、こうしてできました）。
		if _, err := url.Parse(raw); err == nil {
			t.Errorf("%q は解析に失敗する必要があります。失敗しないと、この用例は対象の分岐を覆っていません", raw)
			continue
		}
		err := validateHTTPURL(raw)
		if err == nil {
			t.Errorf("%q は検証に失敗する必要があります", raw)
			continue
		}
		assertNoSecret(t, err.Error(), leakProbeToken)
	}
	// チャネル層の包みも、アドレスを外へ出していないことを確認します。
	t.Setenv(AllowLocalTargetsEnv, "")
	err := (dingTalkChannel{}).Validate(map[string]any{"webhook": cases[0]})
	if err == nil {
		t.Fatal("不正なアドレスは検証に失敗する必要があります")
	}
	assertNoSecret(t, err.Error(), leakProbeToken)
}

// TestEmailDialGuardRejectsLoopbackByDefault は、SMTP チャネルのダイヤルガードを覆います。
//
// メールチャネルは以前、素の net.Dialer を使っていました。SSRF 防護の中で唯一の穴でした。
// host を 169.254.169.254 や 127.0.0.1 にすると直接繋がり、smtp.NewClient のハンドシェイク失敗時には
// 相手が返した行がエラーに入り、last_error から配信履歴の API に表示されます。ほかのチャネルが
// すでに閉じていた、半分見えない読み取りです。「接続拒否とタイムアウト」の時間差で、ポートの探査にも使えます。
//
// このパッケージの TestMain は AllowLocalTargetsEnv を全体で開きます（多くの用例が 127.0.0.1 の
// 仮受信端を使います）。この用例は自分でそれを消します。消さないと、ガードの有無にかかわらず通り、
// この穴が当初どのテストにも見つからなかった理由でもあります。
func TestEmailDialGuardRejectsLoopbackByDefault(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "") // 逃げ口を閉じる。これが既定の挙動です
	_, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil {
		t.Fatal("既定では、ループバックへのメール配信を許してはいけません")
	}
	// 接続は確立してはいけません。ガードは Control フックで止め、EHLO は決して出ません。
	if f.sawCommand("EHLO") || f.sawCommand("HELO") {
		t.Fatal("SMTP セッションが確立しました。ガードが効いていません")
	}
	// エラー情報は、どう開けばよいかを案内します（このマシンの postfix リレーは正当な設定です）。
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("拒否の説明に、明示して開く方法が必要です: %v", err)
	}
}

// TestEmailDialGuardAllowsLoopbackWhenOptedIn は対になる逆の用例です。明示して開いたあとは
// 正常に配信できる必要があります。内部の自前 SMTP や、このマシンのリレーは非常に多い構成です。ガードがまとめて止めてはいけません。
func TestEmailDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("明示して開いたあとは、このマシンの SMTP に配信できる必要があります: %v", err)
	}
	if !f.sawCommand("EHLO") {
		t.Fatal("EHLO が見えません。セッションは確立していません")
	}
}
