package notify

import (
	"net/url"
	"testing"
	"time"
)

// 署名の基準値は OpenSSL が独立に計算したもので、このパッケージ自身の実装から生成していない。
// 自分の実装だと「コードが変わっていない」ことしか証明できず、「アルゴリズムが正しい」ことは証明できない。
//
//	TS=1700000000000, SECRET=SECtest123
//	DingTalk: printf '%s\n%s' "$TS" "$SECRET" | openssl dgst -sha256 -hmac "$SECRET" -binary | openssl base64 -A
//	      -> w3RMHXzixTMdzr8OHJUmVLS4IoPJVdu+Ut1LE48MePE=
//	Feishu: printf '' | openssl dgst -sha256 -hmac "$(printf '%s\n%s' "$TS" "$SECRET")" -binary | openssl base64 -A
//	      -> Hd4xFWQU6R6ad4nzy4ETIznzlqebqH7xcTFVmONTudo=
const (
	signTestTSMillis = int64(1700000000000)
	signTestSecret   = "SECtest123"
	dingTalkExpected = "w3RMHXzixTMdzr8OHJUmVLS4IoPJVdu+Ut1LE48MePE="
	feishuExpected   = "Hd4xFWQU6R6ad4nzy4ETIznzlqebqH7xcTFVmONTudo="
)

func TestDingTalkSignMatchesReference(t *testing.T) {
	got, err := dingTalkSignedURL("https://oapi.dingtalk.com/robot/send?access_token=tok", signTestSecret, time.UnixMilli(signTestTSMillis))
	if err != nil {
		t.Fatalf("署名に失敗しました: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("出力アドレスを解析できません: %v", err)
	}
	q := u.Query()
	if q.Get("sign") != dingTalkExpected {
		t.Errorf("署名が一致しません\n期待 %s\n実際 %s", dingTalkExpected, q.Get("sign"))
	}
	if q.Get("timestamp") != "1700000000000" {
		t.Errorf("タイムスタンプはミリ秒のまま付く必要があります。実際は %q", q.Get("timestamp"))
	}
	// 元の query パラメータ（access_token）を、署名で上書きしてはいけない。
	if q.Get("access_token") != "tok" {
		t.Errorf("元の query パラメータが失われました。実際は %q", q.Get("access_token"))
	}
}

func TestFeishuSignMatchesReference(t *testing.T) {
	got := feishuSign("1700000000000", signTestSecret)
	if got != feishuExpected {
		t.Errorf("署名が一致しません\n期待 %s\n実際 %s", feishuExpected, got)
	}
}

// TestSignAlgorithmsDiffer は、2 社のアルゴリズムの違いを固定する。両者はちょうど引数の順が逆だ
// （DingTalk は key=secret、Feishu は key=署名対象文字列）。もう一方を写すと検証は必ず失敗する。
// この用例は、将来のリファクタで両者を同じ関数にまとめないようにする。
func TestSignAlgorithmsDiffer(t *testing.T) {
	ts := "1700000000000"
	dingURL, err := dingTalkSignedURL("https://example.com/hook", signTestSecret, time.UnixMilli(signTestTSMillis))
	if err != nil {
		t.Fatal(err)
	}
	dq, _ := url.Parse(dingURL)
	if dq.Query().Get("sign") == feishuSign(ts, signTestSecret) {
		t.Fatal("DingTalk と Feishu の署名が同じです。どちらかのアルゴリズム実装が間違っています")
	}
}

func TestDingTalkNoSecretLeavesURLUntouched(t *testing.T) {
	// 署名を有効にしていないロボットでは、timestamp や sign を勝手に足してはいけない。
	const hook = "https://oapi.dingtalk.com/robot/send?access_token=tok"
	got, err := dingTalkSignedURL(hook, "", time.UnixMilli(signTestTSMillis))
	if err != nil {
		t.Fatal(err)
	}
	if got != hook {
		t.Fatalf("secret が未設定のとき、アドレスは変わってはいけません。実際は %q", got)
	}
}

func TestValidateHTTPURL(t *testing.T) {
	ok := []string{"https://example.com/hook", "http://10.0.0.1:8080/x?y=1"}
	for _, s := range ok {
		if err := validateHTTPURL(s); err != nil {
			t.Errorf("%q は受け付ける必要があります: %v", s, err)
		}
	}
	// file:// のような指定は通さない。http.Client での扱いは想定の範囲を超える。
	bad := []string{"", "file:///etc/passwd", "ftp://example.com", "https://", "gopher://x"}
	for _, s := range bad {
		if err := validateHTTPURL(s); err == nil {
			t.Errorf("%q は拒否する必要があります", s)
		}
	}
}
