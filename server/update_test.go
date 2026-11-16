package server

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Autumn-27/artex/selfupdate"
)

// releaseCache は GitHub の枠を守る層である。未認証の API は 60 回/時/IP だけで、
// 上部の「新しいバージョンがある」表示はページを読み込むたびに 1 回調べる。キャッシュが効かないと、タブをいくつか開くだけで
// 枠を使い切り、そのあと本当にアップデートしたくても調べられなくなる。

func newTestCache(fetch func(context.Context, *http.Client) (*selfupdate.Release, error)) *releaseCache {
	return &releaseCache{fetch: fetch}
}

func TestReleaseCacheServesFromCache(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	for range 5 {
		rel, err := c.get(t.Context(), nil, false)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if rel.TagName != "v0.3.8" {
			t.Fatalf("TagName = %q", rel.TagName)
		}
	}
	if calls != 1 {
		t.Errorf("5 回の問い合わせで取得は 1 回であるべきです。実際は %d 回", calls)
	}
}

func TestReleaseCacheForceBypasses(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	// ユーザーが「アップデートを確認」を押したときは、その時点の結果が要る。そうでないと、公開直後のバージョンはキャッシュが切れるまで見えない。
	if _, err := c.get(t.Context(), nil, true); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("force はキャッシュを避け、取得は 2 回であるべきです。実際は %d 回", calls)
	}
}

func TestReleaseCacheExpiresAfterTTL(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return &selfupdate.Release{TagName: "v0.3.8"}, nil
	})

	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	// 保存時刻を、切れた直後まで戻し、TTL が来た状態を作る。
	c.at = time.Now().Add(-releaseTTL - time.Second)
	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("TTL 切れのあとは取り直すので 2 回であるべきです。実際は %d 回", calls)
	}
}

func TestReleaseCacheUsesShorterTTLForErrors(t *testing.T) {
	calls := 0
	c := newTestCache(func(context.Context, *http.Client) (*selfupdate.Release, error) {
		calls++
		return nil, errors.New("github に到達できません")
	})

	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("エラーが返るべきです")
	}
	// 失敗の結果も少しの間キャッシュする。そうでないと、GitHub に届かないとき、ページを開くたびにタイムアウトを待つ。
	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("エラーが返るべきです")
	}
	if calls != 1 {
		t.Errorf("エラーは短時間キャッシュするので、取得は 1 回であるべきです。実際は %d 回", calls)
	}

	// ただしエラーの TTL は成功時より明らかに短くする。ネットワークが戻ったら、すぐ自分で直る。
	if releaseErrTTL >= releaseTTL {
		t.Fatalf("エラーの TTL(%v) は成功時の TTL(%v) より短くなければなりません", releaseErrTTL, releaseTTL)
	}
	c.at = time.Now().Add(-releaseErrTTL - time.Second)
	if _, err := c.get(t.Context(), nil, false); err == nil {
		t.Fatal("エラーが返るべきです")
	}
	if calls != 2 {
		t.Errorf("エラー TTL のあと再試行するので 2 回であるべきです。実際は %d 回", calls)
	}
}

func TestReleaseCacheDoesNotPoisonOnCallerCancel(t *testing.T) {
	good := &selfupdate.Release{TagName: "v0.3.8"}
	c := newTestCache(func(ctx context.Context, _ *http.Client) (*selfupdate.Release, error) {
		return good, nil
	})
	if _, err := c.get(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}

	// 訪問者がタブを閉じるとリクエストはキャンセルされる。それは GitHub の障害ではない。「キャンセル済み」を
	// キャッシュへ書いてはいけない。書くと、その後 30 分、どの訪問者も理由の分からないエラーを受け取る。
	c.fetch = func(ctx context.Context, _ *http.Client) (*selfupdate.Release, error) {
		return nil, ctx.Err()
	}
	c.at = time.Now().Add(-releaseTTL - time.Second) // キャッシュを期限切れにし、取得し直させる

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.get(ctx, nil, false); err == nil {
		t.Fatal("呼び出し元がキャンセルしたときは、そのエラーをそのまま返すべきです")
	}

	// 守る不変条件。キャンセルされた 1 回は痕跡を残さない。キャッシュに「キャンセル済み」のエラーは無く、
	// 前回の正常な結果も残っている。
	if c.err != nil {
		t.Fatalf("キャンセルのエラーをキャッシュしてはいけません。得た値 %v", c.err)
	}
	if c.rel == nil || c.rel.TagName != "v0.3.8" {
		t.Fatalf("キャッシュは前回の正常な結果を残すべきです。得た値 %+v", c.rel)
	}

	// そのキャンセルは新しいデータを持ってこない。次の訪問者は取り直す。結果は正常に取れ、
	// 前回のキャンセルの影響は受けない。
	c.fetch = func(context.Context, *http.Client) (*selfupdate.Release, error) {
		return good, nil
	}
	rel, err := c.get(t.Context(), nil, false)
	if err != nil {
		t.Fatalf("キャンセルのあとの通常リクエストがエラーになりました: %v", err)
	}
	if rel == nil || rel.TagName != "v0.3.8" {
		t.Fatalf("正常な結果が返るべきです。得た値 %+v", rel)
	}
}
