package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// GetSetting は「キーが無い」と「読み取りエラー」の戻りが error の有無だけ違う。どちらの value も
// 空文字である。パスワードの handler が error を「まだパスワードが無い」と扱うと、データベースが揺れている間
// 初期化口を開けてしまう。authInit は未認証のリクエストを通し、既存の管理者パスワードを上書きする。
// authStatus はフロントを /setup へ送り、同じことをさせる。
//
// この 2 つのテストは handler の接続プールを閉じて読み取り失敗を作り、どちらも fail closed だと確認する。
func TestAuthStatusFailsClosedWhenDataSourceUnavailable(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer m.Close()
	// プールを閉じ、その後の GetSetting が sql.ErrNoRows でなく error を返すようにする。
	if err := m.pg.Close(); err != nil {
		t.Fatal(err)
	}

	s := &Server{m: m}
	w := httptest.NewRecorder()
	s.authStatus(w, httptest.NewRequest("GET", "/api/auth/status", nil))

	if w.Code != 503 {
		t.Fatalf("status=%d want 503 (読み取り失敗を未初期化と扱うと、ユーザーを /setup へ送りパスワードを上書きする); body=%s", w.Code, w.Body.String())
	}
	var payload struct {
		Initialized *bool `json:"initialized"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err == nil && payload.Initialized != nil {
		t.Fatalf("読み取り失敗のとき initialized を返してはいけません。得た値 %v", *payload.Initialized)
	}
}

func TestAuthInitFailsClosedWhenDataSourceUnavailable(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer m.Close()
	if err := m.pg.Close(); err != nil {
		t.Fatal(err)
	}

	s := &Server{m: m}
	w := httptest.NewRecorder()
	body := strings.NewReader(`{"password":"correct horse battery"}`)
	s.authInit(w, httptest.NewRequest("POST", "/api/auth/init", body))

	if w.Code != 503 {
		t.Fatalf("status=%d want 503 (読み取り失敗で通すと、未認証のリクエストが既存パスワードを上書きする); body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "token") {
		t.Fatalf("読み取り失敗のとき token を発行してはいけません: %s", w.Body.String())
	}
}

func TestValidatePassword(t *testing.T) {
	for _, tc := range []struct {
		name, pw string
		wantErr  bool
	}{
		{"空文字", "", true},
		{"7文字", "1234567", true},
		{"8文字", "12345678", false},
		{"漢字 8 文字はバイト数ではなく文字数で数える", "密码密码密码密码", false},
		{"漢字 3 文字は 9 バイトでも文字数は 3", "密码强", true},
		{"72 バイト", strings.Repeat("a", 72), false},
		{"73 バイトは bcrypt の上限を超える", strings.Repeat("a", 73), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := validatePassword(tc.pw); (got != "") != tc.wantErr {
				t.Fatalf("validatePassword(%q)=%q, wantErr=%v", tc.pw, got, tc.wantErr)
			}
		})
	}
}
