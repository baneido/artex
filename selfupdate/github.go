package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Enabled reports whether online self-update is compiled into this build.
//
// It is forced OFF in this fork and is a const, not a setting: no config key,
// environment variable, or admin session can turn it back on without editing
// and rebuilding the source. Every online entry point (FetchLatest, Stage)
// checks it and refuses before touching the network, so this binary never
// reaches a remote release feed. Re-enabling online update means flipping this
// to true, re-pointing Repo at a trusted release source, and re-auditing the
// download/verify path.
const Enabled = false

// ErrDisabled is returned by every online-update entry point while Enabled is
// false.
var ErrDisabled = errors.New("オンラインアップデートは無効です。ローカルビルドはリモートのバージョンを確認せず、ダウンロードもしません")

// Repo remains a fixed source identifier; online access is disabled above.
// Keep the URL derived from it so a future reviewed source edit cannot leave
// a second, stale release source behind.
const Repo = "failsafe-security/artex"
const latestURL = "https://api.github.com/repos/" + Repo + "/releases/latest"

// allowedHosts は、アップデート経路がアクセスできるドメインを限る。下の checkRedirect と合わせ、
// どのホップでも名簿外のホストへリダイレクトされたら、そのまま失敗する。これが DNS 汚染や中間者による
// バイナリ差し替えを止める最初の門で、2 番目は SHA256SUMS の照合だ。
var allowedHosts = map[string]bool{
	"api.github.com":                       true,
	"github.com":                           true,
	"objects.githubusercontent.com":        true, // release 資産が実際に置かれるオブジェクトストレージ
	"release-assets.githubusercontent.com": true,
	"raw.githubusercontent.com":            true,
}

// Release は、GitHub Release のうちこのパッケージが使うフィールドだ。
type Release struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	HTMLURL     string    `json:"html_url"`
	Assets      []Asset   `json:"assets"`
}

// Asset は Release に付いた 1 ファイルだ。
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// NewClient は、GitHub のドメインだけを認める HTTP クライアントを作る。proxy が空なら直接接続する。
//
// 既定の Transport は意図して使い回さない。アップデート経路は TLS を強制し、証明書を検証する。別の場所で
// 設定した InsecureSkipVerify などの影響を受けない。
func NewClient(proxy string) *http.Client {
	tr := &http.Transport{
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 15 * time.Second,
	}
	if p := strings.TrimSpace(proxy); p != "" {
		if pu, err := url.Parse(p); err == nil {
			tr.Proxy = http.ProxyURL(pu)
		}
	}
	return &http.Client{
		Transport: tr,
		Timeout:   30 * time.Minute, // パッケージ全体のダウンロード。リクエスト単位のタイムアウトで止めてはいけない
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("リダイレクトが多すぎます")
			}
			return checkURL(req.URL)
		},
	}
}

// checkURL は https とドメイン許可リストを強制する。
func checkURL(u *url.URL) error {
	if u.Scheme != "https" {
		return fmt.Errorf("HTTPS 以外のアドレスは拒否します: %s", u.Scheme+"://"+u.Host)
	}
	if !allowedHosts[strings.ToLower(u.Hostname())] {
		return fmt.Errorf("GitHub 以外のドメインは拒否します: %s", u.Hostname())
	}
	return nil
}

// FetchLatest は最新の正式版を問い合わせる。
//
// Disabled in this fork: it returns ErrDisabled without making any network
// request while Enabled is false.
func FetchLatest(ctx context.Context, c *http.Client) (*Release, error) {
	if !Enabled {
		return nil, ErrDisabled
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latestURL, nil)
	if err != nil {
		return nil, err
	}
	if err := checkURL(req.URL); err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "artex-selfupdate")

	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GitHub にアクセスできませんでした（システム設定でグローバルプロキシを設定できます）: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusForbidden, resp.StatusCode == http.StatusTooManyRequests:
		// 未認証の GitHub API は IP ごとに 1 時間 60 回で、出口 IP を共有しているとすぐに当たる。
		return nil, fmt.Errorf("GitHub API が制限しています（1 時間に 60 回）。しばらくしてから再試行してください")
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("リポジトリ %s にアクセスできないか、正式版がまだ公開されていません", Repo)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("GitHub が %d を返しました", resp.StatusCode)
	}

	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("Release を解析できませんでした: %w", err)
	}
	if strings.TrimSpace(rel.TagName) == "" {
		return nil, fmt.Errorf("Release に tag がありません")
	}
	return &rel, nil
}

// AssetName は現在のプラットフォームに対応するリリースパッケージ名を返す。build.sh の package_binary と揃える。
// artex-<バージョン>-<os>-<arch>.zip（バージョン番号に v 接頭辞は付けない）。
func AssetName(tag, goos, goarch string) string {
	return fmt.Sprintf("artex-%s-%s-%s.zip", strings.TrimPrefix(tag, "v"), goos, goarch)
}

// FindAsset は Release の中を名前で資産を探す。
func (r *Release) FindAsset(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if strings.EqualFold(a.Name, name) {
			return a, true
		}
	}
	return Asset{}, false
}
