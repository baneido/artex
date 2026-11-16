package notify

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// emailDialTimeout / emailSessionTimeout は、接続確立と SMTP セッション全体をそれぞれ制限します。
// net/smtp 自体にはタイムアウトがありません。この 2 つが無いと、止まった相手が
// 配信 goroutine を永久に止めます。dispatcher は単一 goroutine で直列に処理するので、
// 通知システム全体が止まります。
const (
	emailDialTimeout    = 10 * time.Second
	emailSessionTimeout = 45 * time.Second
)

// emailChannel は SMTP メール配信を実装します。
type emailChannel struct{}

func (emailChannel) Kind() string { return KindEmail }

// メールにプラットフォームの制限はありません。ただし画面を埋めないよう、余裕のある既定値を付けます。
func (emailChannel) DefaultRatePerMin() int { return 60 }

// パスワードだけをマスクします。SMTP ホスト、アカウント、宛先は秘密ではなく、マスクすると編集が面倒になるだけです。
func (emailChannel) SecretKeys() []string { return []string{"password"} }

// host/port はパスワードをどのサーバーへ渡すかを決め、tls は転送を暗号化するかを決めます。この 3 つのどれかが変わっても
// パスワードの再指定を要求します。「TLS を切る」を、認証情報を明示せずに済ませないためです。
func (emailChannel) DestinationKeys() []string { return []string{"host", "port", "tls"} }

func (emailChannel) Validate(cfg map[string]any) error {
	if cfgString(cfg, "host") == "" {
		return errors.New("SMTP サーバーのアドレスがありません")
	}
	port := cfgInt(cfg, "port")
	if port <= 0 || port > 65535 {
		return errors.New("SMTP ポートが無効です（1 から 65535 にしてください）")
	}
	if cfgString(cfg, "from") == "" {
		return errors.New("送信元アドレスがありません")
	}
	if len(cfgStrings(cfg, "to")) == 0 {
		return errors.New("宛先アドレスが 1 つ以上必要です")
	}
	return nil
}

func (c emailChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	host := cfgString(cfg, "host")
	port := cfgInt(cfg, "port")
	from := cfgString(cfg, "from")
	to := cfgStrings(cfg, "to")
	username := cfgString(cfg, "username")
	password := cfgString(cfg, "password")
	implicitTLS := cfgBool(cfg, "tls")

	msg, err := buildEmailMessage(from, to, m)
	if err != nil {
		return 0, Permanent(err)
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	client, err := emailDial(ctx, addr, host, implicitTLS)
	if err != nil {
		return 0, err
	}
	defer client.Close()

	// STARTTLS: 相手が対応していれば昇格します。平文セッションでは認証情報を送りません（下の auth の説明を参照）。
	if !implicitTLS {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
				return 0, fmt.Errorf("STARTTLS に失敗しました: %w", err)
			}
		}
	}
	if username != "" {
		if err := client.Auth(smtp.PlainAuth("", username, password, host)); err != nil {
			// smtp.PlainAuth は、暗号化されていない接続では認証情報の送信を拒否します（宛先が localhost のときを除きます）。
			// これは正しい安全動作で、回避してはいけません。ただし理由は分かる日本語にします。
			// そうしないと、利用者は「unencrypted connection」しか見ず、どうすればよいか分かりません。
			if strings.Contains(err.Error(), "unencrypted connection") {
				return 0, Permanent(fmt.Errorf("認証情報は送りません。接続が暗号化されていません。TLS を有効にするか、465 ポート（暗黙の TLS）を使うか、「TLS を有効にする」にチェックを入れてください (%w)", err))
			}
			return 0, Permanent(fmt.Errorf("SMTP 認証に失敗しました: %w", err))
		}
	}
	if err := client.Mail(from); err != nil {
		return 0, smtpStageError(fmt.Sprintf("送信元 %s は拒否されました", from), err)
	}
	for _, rcpt := range to {
		if err := client.Rcpt(rcpt); err != nil {
			return 0, smtpStageError(fmt.Sprintf("宛先 %s は拒否されました", rcpt), err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return 0, fmt.Errorf("SMTP DATA に失敗しました: %w", err)
	}
	if _, err := w.Write([]byte(msg)); err != nil {
		return 0, fmt.Errorf("メール本文を書き込めませんでした: %w", err)
	}
	if err := w.Close(); err != nil {
		return 0, fmt.Errorf("メールを送信できませんでした: %w", err)
	}
	// Quit の失敗は「サーバーがメールを受け取った」という事実を変えません。エラーは無視します。
	_ = client.Quit()
	// メールは長さで切りません（HTML 本文をすべて送ります）。バッチ全体を届いたものとします。
	return len(m.Items), nil
}

// emailDial は SMTP 接続を作ります。
//
// implicitTLS=true は 465 のような「接続した瞬間から TLS」です。false は 25/587 で平文接続してから
// STARTTLS します。両者は混ぜられません。465 へ平文の greeting を送ると、すぐに切られます。
//
// セッション期限は接続を作る時点で設定します（後から補いません）。net/smtp の Client は下位の
// 接続を非公開フィールドに隠し、外から取れないからです。接続を渡したあとは、事前の deadline
// で受けるしかありません。これでハンドシェイク中の停止も覆います。
// Control に blockInternalDial を付け、HTTP 系チャネルと同じ防御を共有します。付けないと SMTP が
// SSRF 防御全体の抜けになります。host に 169.254.169.254 や 127.0.0.1 を入れると直接つながり、
// smtp.NewClient のハンドシェイク失敗は相手が返した行をエラーに包み、last_error を経て
// 配信履歴 API が表示します。これが半盲読みのプリミティブになります。「接続拒否とタイムアウト」の所要時間の差でも
// ポートを探れます。ダイヤル段階が最終的に効く点で、DNS リバインディングも覆います。
func emailDial(ctx context.Context, addr, host string, implicitTLS bool) (*smtp.Client, error) {
	d := &net.Dialer{Timeout: emailDialTimeout, Control: blockInternalDial}
	var conn net.Conn
	var err error
	if implicitTLS {
		conn, err = tls.DialWithDialer(d, "tcp", addr, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("SMTP サーバーに接続できませんでした: %w", err)
	}
	_ = conn.SetDeadline(time.Now().Add(emailSessionTimeout))
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("SMTP のハンドシェイクに失敗しました: %w", err)
	}
	return client, nil
}

// smtpStageError は、SMTP の応答コードで、ある段階の失敗を「再試行できる」と「永続的な失敗」に分けます。
//
// 分ける理由: SMTP の 4xx と 5xx は意味が全く違います。
//   - 4xx（450 のグレーリスト、451 のローカルエラー、452 の容量不足）は一時的な拒否です。
//     正規の扱いは後で再試行することです。グレーリストは、ほぼ毎回の初回配信で出会います。
//   - 5xx（550 のユーザー不存在、553 のアドレス不正）は永続的な拒否で、再試行に意味はありません。
//
// すべて永続的な失敗にすると、グレーリストを有効にしたメールサーバーでは、通知のすべてが最初の
// 試行のあと failed に落ちます。この種の失敗こそ、自動再試行が働くべき場面です。
// 応答コードはエラー文の先頭 3 桁です。コードが取れないときは再試行可能にします（1 回多く試し、
// 解析できないだけで一時的な障害を死んだものと判定しません）。
func smtpStageError(what string, err error) error {
	code := smtpReplyCode(err.Error())
	if code >= 500 && code < 600 {
		return Permanent(fmt.Errorf("%s: %w", what, err))
	}
	return fmt.Errorf("%s: %w", what, err)
}

// smtpReplyCode は SMTP のエラー文から先頭の 3 桁応答コードを取ります。取れなければ 0 を返します。
// net/smtp はエラーコードのフィールドを公開していないので、文から取るしかありません。形式は「450 4.7.1 ...」です。
func smtpReplyCode(text string) int {
	if len(text) < 3 {
		return 0
	}
	n, err := strconv.Atoi(text[:3])
	if err != nil {
		return 0
	}
	return n
}

// buildEmailMessage は完全な RFC 5322 メールを組み立てます。
//
// 本文を base64 にする理由は 2 つです。SMTP は 1 行 1000 バイト以下を定め、HTML
// 本文（特にまとめメール）は長い行になりやすいことです。もう 1 つは、base64 には "." で始まる
// 行が自然に出ないので、SMTP のドットエスケープを省けることです。
func buildEmailMessage(from string, to []string, m Message) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	// 中国語の件名は RFC 2047 で符号化する必要があります。符号化しないとクライアントが文字化けして表示します。
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", htmlTitle(m)))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/html; charset=\"UTF-8\"\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n")
	// メールに長さの上限は無いので、本文は切りません。
	b.WriteString("\r\n")
	encoded := base64.StdEncoding.EncodeToString([]byte(htmlBody(m, 0)))
	// base64 は 76 文字で折り、RFC 2045 に合わせます。
	for len(encoded) > 76 {
		b.WriteString(encoded[:76] + "\r\n")
		encoded = encoded[76:]
	}
	b.WriteString(encoded + "\r\n")
	return b.String(), nil
}
