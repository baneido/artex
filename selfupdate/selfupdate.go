// Package selfupdate implements ARTEX の画面からのワンクリックアップデートだ。GitHub Release から新しい
// バイナリを取得し、検証し、一時保存し、次回起動時に原子的に入れ替える。
//
// 全体の分担（start.sh / start.bat を参照）:
//
//	起動スクリプト = 単純な守護ループ。プロセス終了後、終了コードで再起動するかを決めるだけ
//	このパッケージ = 誤りやすい処理のすべて（ダウンロード / SHA256 検証 / スモーク / 入れ替え / 失敗時のロールバック）
//
// 入れ替えをスクリプトではなく Go に置く理由は、sha256 検証とスモークテストを sh と bat で
// 2 通り書く必要があるからだ（sha256sum / shasum / certutil）。ここが最も誤ってはいけない箇所だ。動かない
// バイナリに入れ替えると、守護プロセスは忠実に繰り返し起動し、ユーザーはマシンに入って手で救うしかない。
//
// 1 回の完全なアップデートは、プロセス起動を 3 回通る。
//
//	① 旧版 server が /api/update/apply を受ける → ダウンロードして検証 → artex.new を一時保存 → exit 75
//	② スクリプトが旧版を再起動 → Bootstrap が artex.new を見つける → 検証とスモーク → 入れ替え → exit 75
//	③ スクリプトが再起動し、この時点で新版 → Bootstrap が試行を 1 回記録 → 起動成功後に印を消す
//
// どの段階が失敗しても旧版へ戻る。② の検証が通らなければ一時ファイルを消して旧版を動かし続ける。③ で印を消すまで
// 3 回連続で生きなければ（起動できず落ちる）、artex.old を自動で戻す。
package selfupdate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ExitRestart は「守護プロセスは私を再起動してください」という終了コードだ（EX_TEMPFAIL）。起動スクリプトはこれを見ると
// すぐに再実行し、クラッシュの退避には数えない。0 はユーザーが正常に停止したことを示す（スクリプトはループを抜ける）。それ以外はクラッシュとみなす。
const ExitRestart = 75

// maxAttempts は、入れ替え後に許す起動試行回数だ。新版は起動のたびにカウントを +1 し、settleDelay を生き抜けば
// 印を消す。maxAttempts 回連続で落ちるのは、新版がそもそも起動できないということで、自動でロールバックする。
const maxAttempts = 3

// Paths は 1 回のアップデートに関わるファイル一式で、実行ファイルのあるディレクトリにまとめて置く。
// CWD は意図して使わない。サービスとして動くときの作業ディレクトリは / や任意のパスになり得て、CWD を使うと一時ファイルが
// 別の場所に落ち、入れ替えがそのまま無効になる。
type Paths struct {
	Dir     string // 実行ファイルのあるディレクトリ
	Current string // 現在動いているバイナリ        artex      / artex.exe
	New     string // 一時保存した新版            artex.new  / artex.new.exe
	Sum     string // 新版の sha256（hex）  artex.new.sha256 / artex.new.exe.sha256
	Old     string // 入れ替え前に退避した旧版      artex.old  / artex.old.exe
	Marker  string // アップデート状態の印            artex.upgrade.json
}

// ResolvePaths は、現在の実行ファイルからアップデート用のパスをすべて導く。
//
// Windows では .new/.old にも .exe 接尾辞が要る。無いとスモークテストも入れ替え後の実行も失敗する。
// そのため先に接尾辞を外してから繋ぎ、両プラットフォームの名前を対称にする。
func ResolvePaths() (Paths, error) {
	exe, err := os.Executable()
	if err != nil {
		return Paths{}, fmt.Errorf("実行ファイルを特定できませんでした: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	dir := filepath.Dir(exe)
	name := filepath.Base(exe)
	ext := filepath.Ext(name) // Windows では ".exe"。Unix では通常空
	stem := strings.TrimSuffix(name, ext)

	join := func(suffix string) string { return filepath.Join(dir, stem+suffix+ext) }
	return Paths{
		Dir:     dir,
		Current: exe,
		New:     join(".new"),
		Sum:     join(".new") + ".sha256",
		Old:     join(".old"),
		Marker:  filepath.Join(dir, stem+".upgrade.json"),
	}, nil
}

// marker は 1 回の入れ替えの進捗を記録し、新版が起動できないときに自動ロールバックを起こす。
type marker struct {
	From     string `json:"from"`     // アップデート前のバージョン
	To       string `json:"to"`       // 目標バージョン
	Attempts int    `json:"attempts"` // 入れ替え後に起動を試した回数
	StagedAt int64  `json:"staged_at"`
}

func readMarker(path string) (marker, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return marker{}, false
	}
	var m marker
	if json.Unmarshal(b, &m) != nil {
		return marker{}, false
	}
	return m, true
}

func writeMarker(path string, m marker) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// cleanStaged は一時ファイルを消す。入れ替え成功、検証失敗、ユーザーの取り消しはどれもここを通り、残った
// artex.new が次回起動で再試行されないようにする。
func cleanStaged(p Paths) {
	_ = os.Remove(p.New)
	_ = os.Remove(p.Sum)
}

// CompareVersions は 2 つのバージョン番号を比べ、-1/0/1 を返す（a<b / a==b / a>b）。
// ok=false は、少なくとも片方が比較できるバージョン番号ではないことを示す（ローカル開発ビルドの "dev" や
// git describe が出す "0.3.7-2-gabc1234-dirty"）。このとき呼び出し側はワンクリックアップデートを無効にする。
// 無効にしないと、開発中のビルドを正式版へ「アップデート」し、未コミットの変更を上書きする。
func CompareVersions(a, b string) (int, bool) {
	av, aok := parseVersion(a)
	bv, bok := parseVersion(b)
	if !aok || !bok {
		return 0, false
	}
	for i := range 3 {
		if av[i] != bv[i] {
			if av[i] < bv[i] {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

// parseVersion は "v0.3.7" / "0.3.7" 形式のバージョン番号を [3]int に解析する。
//
// 受け付けるのは純粋な 3 区切りだけだ。build.sh は tag 以外のビルドで git describe を使い、
// "0.3.7-2-gabc1234" のような接尾辞付きバージョンを出す。それらは比較不能と判定し、0.3.7 として扱わない。
// そうしないと、開発ビルドを「最新である」と誤るか、正式版で上書きする。
func parseVersion(s string) ([3]int, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return [3]int{}, false
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// InDocker は、プロセスがコンテナ内で動いているかを報告する。Docker での入れ替えが書くのはコンテナの書き込み層で、
// `docker compose up -d` でコンテナを作り直すとイメージ同梱のバージョンへ戻る。これは想定どおりの動作だ
// （そのときユーザーは新しいイメージを取得している）。フロントはこの事実を説明できる必要がある。
func InDocker() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	b, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false
	}
	s := string(b)
	return strings.Contains(s, "docker") || strings.Contains(s, "containerd")
}
