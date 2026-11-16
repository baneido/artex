package selfupdate

import (
	"archive/zip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// testPaths は隔離したアップデート用ディレクトリを作る。ResolvePaths() を直接使ってはいけない。
// テストバイナリ自身を指し、実行すると go test の実行ファイルの名前が変わる。
func testPaths(t *testing.T) Paths {
	t.Helper()
	dir := t.TempDir()
	return Paths{
		Dir:     dir,
		Current: filepath.Join(dir, "artex"),
		New:     filepath.Join(dir, "artex.new"),
		Sum:     filepath.Join(dir, "artex.new.sha256"),
		Old:     filepath.Join(dir, "artex.old"),
		Marker:  filepath.Join(dir, "artex.upgrade.json"),
	}
}

// fakeBin は実行できるシェルスクリプトを書き、artex の代わりにする。smokeTest は -h で起動して終了コードを見るだけなので、
// スクリプトで足りる。本物のバイナリをコンパイルするよりずっと速い。
func fakeBin(t *testing.T, path, marker string, exitCode int) {
	t.Helper()
	script := "#!/bin/sh\necho " + marker + "\nexit " + itoa(exitCode) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("仮のバイナリ %s を書けない: %v", path, err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	return string(rune('0' + n))
}

// stage は bin を「一時保存済みで、入れ替え待ち」の形にする。artex.new とその検証和を書く。
func stage(t *testing.T, p Paths, marker string, exitCode int) {
	t.Helper()
	fakeBin(t, p.New, marker, exitCode)
	sum, err := fileSHA256(p.New)
	if err != nil {
		t.Fatalf("検証和を計算できない: %v", err)
	}
	if err := os.WriteFile(p.Sum, []byte(sum), 0o644); err != nil {
		t.Fatalf("検証和を書けない: %v", err)
	}
}

func readAll(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s を読めない: %v", path, err)
	}
	return string(b)
}

func requireUnix(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("仮のバイナリは sh スクリプトなので、Windows では実行できない")
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b       string
		want       int
		comparable bool
	}{
		{"0.3.7", "0.3.8", -1, true},
		{"0.3.8", "0.3.7", 1, true},
		{"0.3.7", "0.3.7", 0, true},
		{"v0.3.7", "0.3.8", -1, true}, // build.sh は v を外し、tag は v を付ける。どちらも認識する
		{"0.3.7", "v0.3.7", 0, true},
		{"0.9.0", "0.10.0", -1, true}, // 辞書順にはせず、数値で比べる
		{"1.0.0", "0.99.99", 1, true},
		// 開発ビルドは比較できないと判定する。比較できると、正式版が未コミットの変更を上書きする。
		{"dev", "0.3.8", 0, false},
		{"0.3.7-2-gabc1234", "0.3.8", 0, false},
		{"0.3.7-dirty", "0.3.8", 0, false},
		{"0.3", "0.3.8", 0, false},
		{"", "0.3.8", 0, false},
	}
	for _, c := range cases {
		got, ok := CompareVersions(c.a, c.b)
		if ok != c.comparable {
			t.Errorf("CompareVersions(%q,%q) comparable=%v、期待は %v", c.a, c.b, ok, c.comparable)
			continue
		}
		if ok && got != c.want {
			t.Errorf("CompareVersions(%q,%q)=%d、期待は %d", c.a, c.b, got, c.want)
		}
	}
}

func TestResolvePathsNaming(t *testing.T) {
	p, err := ResolvePaths()
	if err != nil {
		t.Fatalf("ResolvePaths: %v", err)
	}
	// 重要な不変条件。アップデート用のファイルは、すべて実行ファイルと同じディレクトリに置く。CWD に落ちると、サービスとしての実行
	// （作業ディレクトリが / のことがある）で、入れ替えが完全に効かなくなる。
	for name, path := range map[string]string{"New": p.New, "Sum": p.Sum, "Old": p.Old, "Marker": p.Marker} {
		if filepath.Dir(path) != p.Dir {
			t.Errorf("%s が実行ファイルのディレクトリに無い: %s (期待 %s)", name, path, p.Dir)
		}
	}
	// Windows では .new と .old が .exe を残す。残さないと、スモークテストと入れ替え後の実行が失敗する。
	if runtime.GOOS == "windows" {
		if !strings.HasSuffix(p.New, ".exe") || !strings.HasSuffix(p.Old, ".exe") {
			t.Errorf("Windows では .new と .old が .exe で終わる必要がある: new=%s old=%s", p.New, p.Old)
		}
	}
}

func TestVerifyStagedRejectsTamperedBinary(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	stage(t, p, "new", 0)

	// 検証和を書いたあとにファイルを変え、ダウンロードの破損とすり替えを模す。
	fakeBin(t, p.New, "tampered", 0)
	if err := verifyStaged(p); err == nil {
		t.Fatal("SHA256 不一致は拒否される必要がある。通過した")
	}
}

func TestVerifyStagedRejectsUnrunnableBinary(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	stage(t, p, "broken", 1) // 実行できるが終了コードは 0 ではない

	if err := verifyStaged(p); err == nil {
		t.Fatal("スモークテストの失敗は拒否される必要がある。通過した")
	}
}

func TestApplyStagedHappyPath(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "old", 0)
	stage(t, p, "new", 0)
	if err := writeMarker(p.Marker, marker{From: "0.3.7", To: "0.3.8"}); err != nil {
		t.Fatalf("印を書けない: %v", err)
	}

	action, st := applyStaged(p)
	if action != Restart {
		t.Fatalf("Restart である必要がある。実際は %v", action)
	}
	if !st.Pending {
		t.Error("入れ替え後の状態は Pending である必要がある")
	}
	if !strings.Contains(readAll(t, p.Current), "new") {
		t.Error("artex は新しいバージョンへ置き換わっている必要がある")
	}
	if !strings.Contains(readAll(t, p.Old), "old") {
		t.Error("旧バージョンは artex.old へバックアップされている必要がある")
	}
	if _, err := os.Stat(p.New); !os.IsNotExist(err) {
		t.Error("入れ替え後、artex.new は消えている必要がある")
	}
	if _, err := os.Stat(p.Sum); !os.IsNotExist(err) {
		t.Error("入れ替え後、検証和のファイルは掃除されている必要がある")
	}
	// 印は残す。次の起動（動くのは新しいバージョン）が、これで回数を数え、必要ならロールバックする。
	if _, ok := readMarker(p.Marker); !ok {
		t.Error("入れ替え後、アップデートの印は残っている必要がある")
	}
}

func TestApplyStagedKeepsCurrentWhenVerifyFails(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "old", 0)
	stage(t, p, "new", 0)
	fakeBin(t, p.New, "tampered", 0) // 検証和を壊す

	action, st := applyStaged(p)
	if action != Continue {
		t.Fatalf("検証失敗のときは Continue である必要がある。実際は %v", action)
	}
	if !st.FailedStage {
		t.Error("状態は FailedStage と印づけられる必要がある")
	}
	if !strings.Contains(readAll(t, p.Current), "old") {
		t.Fatal("検証に失敗したとき、現在のバージョンを動かしてはいけない")
	}
	if _, err := os.Stat(p.New); !os.IsNotExist(err) {
		t.Error("検証に失敗した一時保存は掃除する必要がある。残すと、次の起動でもう一度試す")
	}
}

func TestSwapOverwritesPreviousBackup(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "v2", 0)
	fakeBin(t, p.Old, "v1", 0) // 前回のアップデートが残したバックアップ
	stage(t, p, "v3", 0)

	if err := swap(p); err != nil {
		t.Fatalf("swap: %v", err)
	}
	if !strings.Contains(readAll(t, p.Current), "v3") {
		t.Error("v3 へ入れ替わっている必要がある")
	}
	if !strings.Contains(readAll(t, p.Old), "v2") {
		t.Error("バックアップは、いま外された v2 に更新されている必要がある")
	}
}

func TestConfirmCountsAttemptsThenRollsBack(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "broken-new", 0)
	fakeBin(t, p.Old, "good-old", 0)
	m := marker{From: "0.3.7", To: "0.3.8"}

	// 最初の maxAttempts 回の起動は回数を積むだけにする。新しいバージョンが自分で安定する機会を残す。
	for i := 1; i <= maxAttempts; i++ {
		action, st := confirmOrRollback(p, m)
		if action != Continue {
			t.Fatalf("%d 回目は Continue である必要がある。実際は %v", i, action)
		}
		if !st.Pending {
			t.Errorf("%d 回目の状態は Pending である必要がある", i)
		}
		got, ok := readMarker(p.Marker)
		if !ok || got.Attempts != i {
			t.Fatalf("%d 回目のあと attempts=%d（ok=%v）。期待は %d", i, got.Attempts, ok, i)
		}
		m = got
	}

	// もう一度落ちると上限を超え、旧バージョンを自動で戻す。
	action, st := confirmOrRollback(p, m)
	if action != Restart {
		t.Fatalf("試行上限を超えたときは Restart である必要がある。実際は %v", action)
	}
	if !st.RolledBack {
		t.Error("状態は RolledBack と印づけられる必要がある")
	}
	if !strings.Contains(readAll(t, p.Current), "good-old") {
		t.Fatal("旧バージョンへロールバックされている必要がある")
	}
	if _, err := os.Stat(p.Marker); !os.IsNotExist(err) {
		t.Error("ロールバック後、印は消える必要がある。残すとロールバックが終わらない")
	}
	// 起動できなかったバージョンは調査用に残す。すぐには消さない。
	if _, err := os.Stat(p.Current + ".failed"); err != nil {
		t.Error("失敗したバージョンは調査用に .failed として残る必要がある")
	}
}

func TestManualRollbackIsReversible(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "v2", 0)
	fakeBin(t, p.Old, "v1", 0)

	// Rollback() は ResolvePaths() を通る。ここでは下位の入れ替えの意味を直接測る。
	tmp := p.Current + ".swap"
	if err := os.Rename(p.Current, tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, p.Old); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readAll(t, p.Current), "v1") {
		t.Error("ロールバック後の現在のバージョンは v1 である必要がある")
	}
	if !strings.Contains(readAll(t, p.Old), "v2") {
		t.Error("ロールバック後のバックアップは v2 になる必要がある。もう一度戻せる")
	}
}

func TestParseSums(t *testing.T) {
	const (
		linuxSum = "1111111111111111111111111111111111111111111111111111111111111111"
		winSum   = "ABCDEF0000000000000000000000000000000000000000000000000000000000"
	)
	// sha256sum の出力は空白 2 つ区切りである。shasum -a 256 はバイナリモードでファイル名の前に * を付ける。
	raw := linuxSum + "  artex-0.3.8-linux-amd64.zip\n" +
		winSum + " *artex-0.3.8-windows-amd64.zip\n" +
		"\n" +
		"garbage line\n" + // フィールドはちょうど 2 つだが、1 つ目は検証和ではない
		"deadbeef  artex-0.3.8-darwin-arm64.zip\n" // 検証和の長さが違う

	out := parseSums(raw)
	if out["artex-0.3.8-linux-amd64.zip"] != linuxSum {
		t.Errorf("linux の項目の解析が違う: %v", out)
	}
	// 検証和は小文字に揃える。比べるとき、大文字小文字で不一致と誤って判定しない。
	if got := out["artex-0.3.8-windows-amd64.zip"]; got != strings.ToLower(winSum) {
		t.Errorf("windows の項目が違う（* 接頭辞は外し、検証和は小文字にする）: %q", got)
	}
	if len(out) != 2 {
		t.Errorf("空行、検証和ではない行、長さが違う行は無視する必要がある。実際は %v", out)
	}
}

func TestExtractBinaryFindsNestedEntry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("パッケージ内のベース名は Windows では artex.exe である。この用例は Unix の名前で作っている")
	}
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "release.zip")

	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	// 実際のリリースパッケージの構造は artex-<バージョン>-<os>-<arch>/artex で、邪魔になるファイルがいくつか付く。
	for name, body := range map[string]string{
		"artex-0.3.8-linux-amd64/README.md":           "readme",
		"artex-0.3.8-linux-amd64/skills/a.md":         "skill",
		"artex-0.3.8-linux-amd64/artex":               "#!/bin/sh\nexit 0\n",
		"artex-0.3.8-linux-amd64/config.example.json": "{}",
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()

	dst := filepath.Join(dir, "out")
	if err := extractBinary(zipPath, dst); err != nil {
		t.Fatalf("extractBinary: %v", err)
	}
	if got := readAll(t, dst); !strings.Contains(got, "exit 0") {
		t.Errorf("展開されたものが artex の実行ファイルではない: %q", got)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Error("展開したバイナリには実行ビットが必要である")
	}
}

func TestExtractBinaryMissingEntry(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "release.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, _ := zw.Create("artex-0.3.8-linux-amd64/README.md")
	_, _ = w.Write([]byte("readme"))
	_ = zw.Close()
	f.Close()

	if err := extractBinary(zipPath, filepath.Join(dir, "out")); err == nil {
		t.Fatal("パッケージ内に実行ファイルが無いときはエラーになる必要がある")
	}
}

func TestCheckURLRejectsNonGitHub(t *testing.T) {
	bad := []string{
		"http://github.com/x",           // HTTPS ではない
		"https://evil.com/artex.zip",    // ドメインが許可リストに無い
		"https://github.com.evil.com/x", // 接尾辞を偽装している
		"https://raw.githubusercontent.com.evil.com/x",
	}
	for _, raw := range bad {
		u := mustParse(t, raw)
		if err := checkURL(u); err == nil {
			t.Errorf("checkURL(%q) は拒否する必要がある", raw)
		}
	}
	good := []string{
		"https://api.github.com/repos/x/releases/latest",
		"https://objects.githubusercontent.com/blah",
		"https://GitHub.com/x", // ドメインの大文字小文字は区別しない
	}
	for _, raw := range good {
		u := mustParse(t, raw)
		if err := checkURL(u); err != nil {
			t.Errorf("checkURL(%q) は通す必要がある。エラー: %v", raw, err)
		}
	}
}

func TestAssetNameMatchesBuildScript(t *testing.T) {
	// build.sh の package_binary は artex-<バージョン>-<os>-<arch>.zip を使い、バージョン番号から
	// v の接頭辞を外している。ここを 1 文字間違えると、どのプラットフォームのワンクリックアップデートも資産を見つけられない。
	if got := AssetName("v0.3.8", "linux", "amd64"); got != "artex-0.3.8-linux-amd64.zip" {
		t.Errorf("AssetName = %q", got)
	}
	if got := AssetName("0.3.8", "windows", "amd64"); got != "artex-0.3.8-windows-amd64.zip" {
		t.Errorf("AssetName = %q", got)
	}
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("%q を解析できない: %v", raw, err)
	}
	return u
}

func TestSettleClearsMarkerAndStopsRollback(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "new", 0)
	fakeBin(t, p.Old, "old", 0)
	if err := writeMarker(p.Marker, marker{From: "0.3.7", To: "0.3.8", Attempts: 2}); err != nil {
		t.Fatal(err)
	}

	settle(p)

	if _, err := os.Stat(p.Marker); !os.IsNotExist(err) {
		t.Fatal("安定を確認したあとは、アップデートの印を消す必要がある")
	}
	// 印が無いと、その後の通常の再起動は回数を積まず、ロールバックを誤って起こさない。
	if _, ok := readMarker(p.Marker); ok {
		t.Error("印の読み取りは失敗する必要がある")
	}
	// バックアップは残す。ユーザーは手動でロールバックできる。
	if _, err := os.Stat(p.Old); err != nil {
		t.Error("安定を確認したあとも、前のバージョンのバックアップは残る必要がある")
	}
}

func TestSettleIsNoopWithoutMarker(t *testing.T) {
	requireUnix(t)
	p := testPaths(t)
	fakeBin(t, p.Current, "cur", 0)
	settle(p) // 通常の起動経路。panic してはいけないし、どのファイルも動かしてはいけない
	if _, err := os.Stat(p.Current); err != nil {
		t.Error("印が無いとき、settle はどのファイルにも影響してはいけない")
	}
}
