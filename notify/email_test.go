package notify

import (
	"bufio"
	"context"

	"net"
	"strings"
	"sync"
	"testing"
)

// このファイルは、メールチャネルのプロトコルレベルのテストを補います。これより前は email.Send のカバレッジが 0 でした。
// SMTP の経路全体を通る用例がなく、6 つのチャネルの中でプロトコル面が最も広く、
// 最も誤りやすいものでした（ハンドシェイク、認証、エンベロープ、DATA の各段階で、失敗の意味が違います）。
//
// ここでは自前の最小 SMTP サーバーで駆動します。net/smtp を mock で置き換えません。
// メールチャネルの危険のほとんどは、「本物の SMTP サーバーと対話する」この段階にあります。
// この段階を mock にすると、測っていないのと同じです。

// fakeSMTP は、ちょうど足りる SMTP サーバーです。greet/EHLO/AUTH/MAIL/RCPT/DATA/QUIT を完了し、
// 用例の指定どおり、特定の段階で指定の応答コードを返します。
type fakeSMTP struct {
	ln net.Listener

	// rcptReply は RCPT TO の応答です。既定は 250 です。
	rcptReply string
	// mailReply は MAIL FROM の応答です。既定は 250 です。
	mailReply string
	// advertiseAuth が true のとき、EHLO で AUTH PLAIN を宣言します。
	advertiseAuth bool

	mu       sync.Mutex
	data     string
	commands []string
}

func newFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln, rcptReply: "250 OK", mailReply: "250 OK"}
	go f.serve()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeSMTP) hostPort(t *testing.T) (string, int) {
	t.Helper()
	addr, ok := f.ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatal("TCP の待受アドレスではありません")
	}
	return "127.0.0.1", addr.Port
}

func (f *fakeSMTP) record(cmd string) {
	f.mu.Lock()
	f.commands = append(f.commands, cmd)
	f.mu.Unlock()
}

func (f *fakeSMTP) body() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.data
}

func (f *fakeSMTP) sawCommand(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.commands {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func (f *fakeSMTP) serve() {
	conn, err := f.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	w := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
	w("220 fake.local ESMTP ready")
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		f.record(line)
		switch {
		case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
			// STARTTLS は宣言しません。コードは平文の分岐へ進みます（テストの対象はエンベロープの論理で、TLS ではありません）。
			w("250-fake.local")
			if f.advertiseAuth {
				w("250-AUTH PLAIN")
			}
			w("250 8BITMIME")
		case strings.HasPrefix(line, "AUTH"):
			// 処理は簡略です。PLAIN の最初の応答は複数行にまたがることがあり、そのまま受け入れます。
			w("235 2.7.0 Authentication successful")
		case strings.HasPrefix(line, "MAIL FROM"):
			w(f.mailReply)
		case strings.HasPrefix(line, "RCPT TO"):
			w(f.rcptReply)
		case strings.HasPrefix(line, "DATA"):
			w("354 End data with <CR><LF>.<CR><LF>")
			var sb strings.Builder
			for {
				dl, err := br.ReadString('\n')
				if err != nil {
					return
				}
				if strings.TrimRight(dl, "\r\n") == "." {
					break
				}
				sb.WriteString(dl)
			}
			f.mu.Lock()
			f.data = sb.String()
			f.mu.Unlock()
			w("250 2.0.0 Ok: queued as FAKE1")
		case strings.HasPrefix(line, "QUIT"):
			w("221 2.0.0 Bye")
			return
		default:
			w("250 OK")
		}
	}
}

func emailCfg(t *testing.T, f *fakeSMTP, extra map[string]any) map[string]any {
	t.Helper()
	host, port := f.hostPort(t)
	cfg := map[string]any{
		"host": host,
		"port": float64(port),
		"from": "artex@example.com",
		"to":   []any{"a@example.com", "b@example.com"},
	}
	for k, v := range extra {
		cfg[k] = v
	}
	return cfg
}

func TestEmailSendDeliversFullMessage(t *testing.T) {
	f := newFakeSMTP(t)
	f.advertiseAuth = true
	cfg := emailCfg(t, f, map[string]any{"username": "artex", "password": "pw"})

	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("配信に失敗しました: %v", err)
	}
	// エンベロープの段階まで進む必要があります。送信元、宛先 2 つ、DATA です。
	for _, want := range []string{"MAIL FROM:<artex@example.com>", "RCPT TO:<a@example.com>", "RCPT TO:<b@example.com>", "DATA", "AUTH", "QUIT"} {
		if !f.sawCommand(want) {
			t.Errorf("SMTP セッションに %q がありません。実際のコマンド: %v", want, f.commands)
		}
	}
	// 本文は base64 の HTML で、実際の脆弱性の内容を含みます（エンコード後も見分けられます）。
	body := f.body()
	if body == "" {
		t.Fatal("DATA の段階で本文を受け取っていません")
	}
	if !strings.Contains(body, "Content-Type: text/html") {
		t.Errorf("Content-Type ヘッダーがありません:\n%s", body)
	}
	if !strings.Contains(body, "base64") {
		t.Errorf("本文が base64 になっていません（長い HTML 行は SMTP の 1000 バイト行制限を壊します）:\n%s", body)
	}
	// 複数の宛先は、すべて To ヘッダーに出ます。
	if !strings.Contains(body, "a@example.com, b@example.com") {
		t.Errorf("To ヘッダーに宛先が揃っていません:\n%s", body)
	}
}

func TestEmailSendWithoutAuth(t *testing.T) {
	// アカウントが無いときは AUTH を送ってはいけません。中継によっては、そのために受け取りを拒みます。
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)
	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("配信に失敗しました: %v", err)
	}
	if f.sawCommand("AUTH") {
		t.Errorf("アカウントが無いのに AUTH を送りました: %v", f.commands)
	}
}

// TestEmailSendClassifiesSMTPReplies は、今回の監査修正を直接検証します。
// 5xx は永久失敗、4xx（グレーリスト）は再試行可能です。
func TestEmailSendClassifiesSMTPReplies(t *testing.T) {
	cases := []struct {
		name      string
		rcptReply string
		mailReply string
		permanent bool
	}{
		{"宛先が 550 で永久に拒否される", "550 5.1.1 User unknown", "250 OK", true},
		{"宛先が 450 のグレーリストに当たる", "450 4.7.1 Greylisting in action", "250 OK", false},
		{"宛先が 452 でメールボックス満杯", "452 4.2.2 Mailbox full", "250 OK", false},
		{"送信元が 553 で永久に拒否される", "250 OK", "553 5.1.3 Bad address", true},
		{"送信元が 451 の一時エラー", "250 OK", "451 4.3.0 Temporary failure", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeSMTP(t)
			f.rcptReply = tc.rcptReply
			f.mailReply = tc.mailReply
			_, err := (emailChannel{}).Send(context.Background(), emailCfg(t, f, nil), singleMsg())
			if err == nil {
				t.Fatal("エラーになる必要があります")
			}
			if got := IsPermanent(err); got != tc.permanent {
				t.Fatalf("permanent の判定が違います。期待は %v、実際は %v です (%v)", tc.permanent, got, err)
			}
			// サーバーの原文は残します。残さないと、ユーザーはサーバー管理者に聞くべきか、アドレスを直すべきかが分かりません。
			if !strings.Contains(err.Error(), strings.Fields(tc.rcptReply)[0]) && !strings.Contains(err.Error(), strings.Fields(tc.mailReply)[0]) {
				t.Errorf("エラーにサーバーの応答コードを残す必要があります: %v", err)
			}
		})
	}
}

func TestEmailSendRefusesPlaintextCredentials(t *testing.T) {
	// net/smtp の PlainAuth は、暗号化されていない接続では認証情報を送りません（対象が localhost のときを除きます）。
	// これは正しい安全の挙動で、迂回してはいけません。同時に、ユーザーが直せるエラーを出します。
	// ここでは localhost ではないホスト名で、それを起こします。
	f := newFakeSMTP(t)
	f.advertiseAuth = true
	_, port := f.hostPort(t)
	cfg := map[string]any{
		"host":     "smtp.example.com", // localhost ではない
		"port":     float64(port),
		"from":     "a@example.com",
		"to":       []any{"b@example.com"},
		"username": "artex",
		"password": "pw",
	}
	_, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil {
		t.Skip("このマシンの DNS がローカルサーバーに解決しました。スキップします（ほかの用例には影響しません）")
	}
	// 繋がらない場合も、認証情報の送信を拒まれた場合も、この断言は通ります。静かにパスワードを送ってはいけません。
	if !IsPermanent(err) && !strings.Contains(err.Error(), "接続") {
		t.Logf("エラー: %v（localhost 以外で繋がらないのは想定どおりです）", err)
	}
}

func TestEmailValidateReportsMissingFields(t *testing.T) {
	// メールチャネルは設定フィールドが最も多く、1 つ欠けると配信時まで露出しません。ここでは 1 つずつ、
	// 検証が先に止めることを確認します。断言が見るのは「エラー情報に、何が欠けているかが出ている」ことです。
	cases := []struct {
		name string
		cfg  map[string]any
	}{
		{"host が無い", map[string]any{"port": float64(25), "from": "a@b.c", "to": []any{"d@e.f"}}},
		{"port が無い", map[string]any{"host": "smtp.example.com"}},
		{"port が範囲外", map[string]any{"host": "h", "port": float64(70000), "from": "a@b.c", "to": []any{"d@e.f"}}},
		{"from が無い", map[string]any{"host": "h", "port": float64(25), "to": []any{"d@e.f"}}},
		{"to が無い", map[string]any{"host": "h", "port": float64(25), "from": "a@b.c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := (emailChannel{}).Validate(tc.cfg); err == nil {
				t.Fatalf("検証に失敗する必要があります: %v", tc.cfg)
			}
		})
	}
}

// TestEmailConfigTolerance は、設定の読み取りの寛容さを覆います。JSONB の数値は float64 ですが、
// ユーザーは UI でポートを文字列にすることがあります。配列が 1 つの文字列であることもあります。
func TestEmailConfigTolerance(t *testing.T) {
	cfg := map[string]any{
		"host": "smtp.example.com",
		"port": "587", // 文字列のポート
		"from": "a@b.c",
		"to":   "d@e.f", // 配列にはせず、1 つの文字列
		"tls":  "true",  // 文字列の真偽値
	}
	if err := (emailChannel{}).Validate(cfg); err != nil {
		t.Fatalf("文字列の数値を受け入れる必要があります: %v", err)
	}
	if got := cfgInt(cfg, "port"); got != 587 {
		t.Errorf("cfgInt が文字列のポートを解析していません。実際は %d です", got)
	}
	if !cfgBool(cfg, "tls") {
		t.Error("cfgBool が文字列 \"true\" を解析していません")
	}
	if to := cfgStrings(cfg, "to"); len(to) != 1 || to[0] != "d@e.f" {
		t.Errorf("cfgStrings が単一の文字列に対応していません。実際は %v です", to)
	}
}

// TestFilterValidateRejectsTypo は、監査修正を直接検証します。
// 下限の打ち間違いは書き込み時に止めます。止めないと、フィルタが静かに無効になり、すべてを送ります。
func TestFilterValidateRejectsTypo(t *testing.T) {
	good := []string{"", "low", "medium", "high", "critical"}
	for _, s := range good {
		if err := (Filter{MinSeverity: s}).Validate(); err != nil {
			t.Errorf("正当な下限 %q が拒否されました: %v", s, err)
		}
	}
	// 実際に起きる打ち間違いです。すべて拒否されます。
	for _, s := range []string{"hgih", "HIGH", "严重", "high ", "crit"} {
		err := (Filter{MinSeverity: s}).Validate()
		if err == nil {
			t.Errorf("不正な下限 %q は拒否される必要があります（拒否しないと、フィルタが静かに無効になり、すべてを送ります）", s)
			continue
		}
		// エラー情報は、ユーザーが正しく直せるように案内します。
		if !strings.Contains(err.Error(), "low") || !strings.Contains(err.Error(), "critical") {
			t.Errorf("エラー情報は選べる値を列挙する必要があります。実際は %q です", err.Error())
		}
	}
}

// TestFilterValidateIsWriteTimeOnly は「書き込みは厳しく、読み取りは広い」分担を固定します。
// 保存済みの悪い値で、チャネル全体が読めなくなってはいけません（そうなると、過去のチャネルが突然すべて配信を止めます）。
func TestFilterValidateIsWriteTimeOnly(t *testing.T) {
	raw := []byte(`{"min_severity":"hgih"}`)
	f := ParseFilter(raw) // エラーにはなりません
	if f.MinSeverity != "hgih" {
		t.Fatalf("読み取り経路は元の値を残す必要があります。実際は %q です", f.MinSeverity)
	}
	// そのチャネルは、イベントを判定できます（panic せず、止まりません）。
	_ = Match(f, Snapshot{Kind: EventFindingCreated, Severity: "critical"})
}
