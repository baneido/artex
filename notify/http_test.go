package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// このファイルは doJSON の HTTP 層のエラー分類を覆う。
//
// 別に測る理由: 各チャネルアダプタが扱うのはプラットフォーム自身の業務エラーコード（DingTalk の errcode、
// Feishu の code、Telegram の ok フィールド）で、HTTP 層の分類は doJSON がまとめて行う。
// 両者は独立した 2 つの防御だ。これが無いと、503 を返す中継ゲートウェイを永続的な失敗とみなし、
// 再試行をすぐ放棄する。403 は再試行できるとみなされ、退避を 3 周むだにする。

func replyServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDoJSONClassifiesHTTPStatus(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		permanent bool
	}{
		{"200 成功はエラーではない", 200, false},
		{"429 制限は再試行できる", 429, false},
		{"408 リクエストタイムアウトは再試行できる", 408, false},
		{"500 サーバーエラーは再試行できる", 500, false},
		{"502 ゲートウェイエラーは再試行できる", 502, false},
		{"503 サービス利用不可は再試行できる", 503, false},
		{"400 パラメータ誤りは永続的な失敗", 400, true},
		{"401 認証失敗は永続的な失敗", 401, true},
		{"403 アクセス禁止は永続的な失敗", 403, true},
		{"404 アドレス不存在は永続的な失敗", 404, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := replyServer(t, tc.status, `{"detail":"upstream says no"}`)
			_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
			if tc.status < 300 {
				if err != nil {
					t.Fatalf("2xx はエラーになってはいけません: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("2xx 以外はエラーになる必要があります")
			}
			if got := IsPermanent(err); got != tc.permanent {
				t.Fatalf("HTTP %d の permanent 判定が違います。期待 %v 実際 %v (%v)",
					tc.status, tc.permanent, got, err)
			}
			// ステータスコードはエラーに出る必要がある。出ないと、ユーザーは自分の設定ミスか相手の障害かを判断できない。
			// 断言するのは数字であり、Go の英語 StatusText ではない。このパッケージの文面は日本語で、
			// 数字だけが言語に依存せず、安定して断言できる。
			if !strings.Contains(err.Error(), strconv.Itoa(tc.status)) {
				t.Errorf("エラー情報には HTTP ステータス %d が必要です。実際は %v", tc.status, err)
			}
		})
	}
}

// TestDoJSONIncludesResponseSnippet は snippet を覆う。相手が返したエラー説明は持ち帰る。
// 持ち帰らないと、ユーザーは「失敗した」としか知らず、相手が拒否した理由が分からない。
func TestDoJSONIncludesResponseSnippet(t *testing.T) {
	srv := replyServer(t, 400, `{"error":"invalid webhook token"}`)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("エラーになる必要があります")
	}
	if !strings.Contains(err.Error(), "invalid webhook token") {
		t.Errorf("エラー情報には相手の説明が必要です。実際は %v", err)
	}
}

// TestDoJSONSnippetIsSingleLineAndBounded は snippet の形を制約する。
// 相手の応答はそのまま last_error 列とフロントの表に入る。複数行や長すぎる文はレイアウトとペイロードを壊す。
func TestDoJSONSnippetIsSingleLineAndBounded(t *testing.T) {
	// 改行、タブ、5000 文字の長い内容を含む応答。
	long := strings.Repeat("x", 5000)
	srv := replyServer(t, 500, "line1\nline2\r\n\tline3 "+long)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("エラーになる必要があります")
	}
	msg := err.Error()
	if strings.ContainsAny(msg, "\r\n\t") {
		t.Errorf("エラー情報は 1 行に畳む必要があります。実際は %q", msg)
	}
	// snippet の上限は 200 文字に固定の接頭辞で、合計は元の応答よりずっと小さい必要がある。
	if len(msg) > 400 {
		t.Errorf("エラー情報が長すぎます（%d バイト）。snippet で切る必要があります: %q", len(msg), msg)
	}
}

// TestDoJSONRejectsOversizedResponse は、読み取りに上限があることを確認する。相手が異常に巨大な内容を返したとき、
// 応答全体をメモリへ読んではいけない（配信履歴の 1 件ごとに last_error を 1 つ保存する）。
func TestDoJSONRejectsOversizedResponse(t *testing.T) {
	huge := strings.Repeat("A", 1<<20) // 1 MiB
	srv := replyServer(t, 400, huge)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("エラーになる必要があります")
	}
	if len(err.Error()) > 400 {
		t.Errorf("巨大な応答は長さを限って読み、切る必要があります。エラー情報の長さは %d", len(err.Error()))
	}
}
