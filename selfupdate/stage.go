package selfupdate

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"runtime"
	"strings"
	"time"
)

// sumsAsset は release.yml が生成する検証和の一覧で、Release 内の zip をすべて覆う。
const sumsAsset = "SHA256SUMS"

// maxBinarySize は展開したバイナリの大きさを制限し、壊れた zip がディスクを埋めないようにする。
const maxBinarySize = 512 << 20 // 512 MiB

// Phase はアップデート中の段階で、SSE イベントの phase フィールドにそのまま使う。
type Phase string

const (
	PhaseIdle     Phase = "idle"
	PhaseDownload Phase = "downloading"
	PhaseVerify   Phase = "verifying"
	PhaseExtract  Phase = "extracting"
	PhaseStaged   Phase = "staged"
	PhaseFailed   Phase = "failed"
)

// Progress は呼び出し側が渡し、進捗をフロントへ送る。pct に意味があるのはダウンロード段階だけ（0-100）。
// それ以外の段階は -1 を渡す。
type Progress func(ph Phase, pct int, msg string)

// Stage は指定 Release の、現在のプラットフォーム向けリリースパッケージをダウンロードし、検証して新しいバイナリを artex.new として一時保存する。
//
// 裸のバイナリではなく完全な zip を使う。理由は 2 つだ。既存 Release の SHA256SUMS はもともと
// zip だけを覆い、zip なら CI を変えずに済み、公開済みの過去バージョンとも合う。zip には
// skills/ も入っていて、将来組み込み skill を同期する余地になる。増えるのは skills の数百 KB だけだ。
//
// 関数が戻った時点で一時保存は完了している。呼び出し側はそのあと穏やかに閉じ、ExitRestart で終了する。
func Stage(ctx context.Context, c *http.Client, rel *Release, currentVersion string, prog Progress) error {
	// Disabled in this fork (see Enabled in github.go): refuse before any
	// download so this binary never stages a remotely fetched replacement.
	if !Enabled {
		return ErrDisabled
	}
	if prog == nil {
		prog = func(Phase, int, string) {}
	}
	p, err := ResolvePaths()
	if err != nil {
		return err
	}
	if err := checkWritable(p.Dir); err != nil {
		return err
	}

	name := AssetName(rel.TagName, runtime.GOOS, runtime.GOARCH)
	asset, ok := rel.FindAsset(name)
	if !ok {
		return fmt.Errorf("このバージョンには %s/%s のリリースパッケージがありません（%s がありません）", runtime.GOOS, runtime.GOARCH, name)
	}

	prog(PhaseDownload, 0, "検証和の一覧を取得しています…")
	sums, err := fetchSums(ctx, c, rel)
	if err != nil {
		return err
	}
	want, ok := sums[name]
	if !ok {
		return fmt.Errorf("%s に %s が含まれていません。検証していないバイナリのインストールは拒否します", sumsAsset, name)
	}

	// 一時ファイルはすべて対象ディレクトリに置く。最後の rename を同じファイルシステム内の原子操作にするためだ
	// （デバイスをまたぐ rename は失敗し、/tmp は独立したマウント点であることが多い）。
	zipPath := p.New + ".zip.part"
	binPath := p.New + ".part"
	defer func() {
		_ = os.Remove(zipPath)
		_ = os.Remove(binPath)
	}()

	prog(PhaseDownload, 0, fmt.Sprintf("%s をダウンロードしています（%s）…", name, humanSize(asset.Size)))
	got, err := download(ctx, c, asset, zipPath, prog)
	if err != nil {
		return err
	}

	prog(PhaseVerify, -1, "SHA256 を検証しています…")
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("SHA256 が一致しません。期待値は %s、実際は %s です（ダウンロードの破損、または改ざん）", short(want), short(got))
	}

	prog(PhaseExtract, -1, "展開してスモークテストをしています…")
	if err := extractBinary(zipPath, binPath); err != nil {
		return err
	}
	if err := smokeTest(binPath); err != nil {
		return fmt.Errorf("新しいバージョンは現在のシステムで実行できません: %w", err)
	}

	// 一時ファイル自身の sha256 を別に保存する。次回起動で入れ替える前にもう一度検証し、
	// 一時保存から再起動までのあいだにファイルが変わったり壊れたりしないようにする。
	binSum, err := fileSHA256(binPath)
	if err != nil {
		return fmt.Errorf("新しいバイナリの検証和を計算できませんでした: %w", err)
	}
	if err := os.WriteFile(p.Sum, []byte(binSum), 0o644); err != nil {
		return fmt.Errorf("検証和を書き込めませんでした: %w", err)
	}
	if err := os.Rename(binPath, p.New); err != nil {
		_ = os.Remove(p.Sum)
		return fmt.Errorf("新しいバージョンを一時保存できませんでした: %w", err)
	}

	if err := writeMarker(p.Marker, marker{
		From:     currentVersion,
		To:       strings.TrimPrefix(rel.TagName, "v"),
		StagedAt: time.Now().Unix(),
	}); err != nil {
		// 印が影響するのは自動ロールバックの能力だけで、一時ファイル自体は既に揃っている。この失敗ではアップデートを中断しない。
		prog(PhaseStaged, -1, "警告: アップデートの印を書き込めませんでした。今回のアップデートには自動ロールバックの保護がありません")
	}

	prog(PhaseStaged, 100, "新しいバージョンの準備ができました。再起動中…")
	return nil
}

// fetchSums は SHA256SUMS をダウンロードして解析し、ファイル名から 16 進のダイジェストを返す。
func fetchSums(ctx context.Context, c *http.Client, rel *Release) (map[string]string, error) {
	asset, ok := rel.FindAsset(sumsAsset)
	if !ok {
		return nil, fmt.Errorf("この Release に %s が無く、完全性を検証できないので、アップデートは拒否します", sumsAsset)
	}
	body, err := get(ctx, c, asset.URL)
	if err != nil {
		return nil, fmt.Errorf("%s をダウンロードできませんでした: %w", sumsAsset, err)
	}
	defer body.Close()

	raw, err := io.ReadAll(io.LimitReader(body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%s を読み取れませんでした: %w", sumsAsset, err)
	}
	out := parseSums(string(raw))
	if len(out) == 0 {
		return nil, fmt.Errorf("%s の内容が空か、形式を識別できません", sumsAsset)
	}
	return out, nil
}

// parseSums は sha256sum 形式の一覧を解析し、ファイル名から 16 進のダイジェストを返す。
//
// 収録するのは、最初のフィールドが 64 桁の 16 進である行だけだ。「ちょうど 2 フィールド」だけでは足りない。
// 単語が 2 つの説明文が正当な項目になり、ゴミの値がダイジェスト表に入り、
// 本物の資産が誤ったダイジェストに一致し得る。
func parseSums(raw string) map[string]string {
	out := map[string]string{}
	for line := range strings.Lines(raw) {
		// 形式は "<sha256>  <filename>" だ（sha256sum は空白 2 つ。shasum のバイナリ
		// モードはファイル名に * 接頭辞を付ける）。
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 2 || !isHexSHA256(fields[0]) {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name == "" {
			continue
		}
		out[name] = strings.ToLower(fields[0])
	}
	return out
}

func isHexSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// download は資産を dst に書き、同時に SHA256 を計算し、Content-Length に沿って進捗を報告する。
func download(ctx context.Context, c *http.Client, a Asset, dst string, prog Progress) (string, error) {
	body, err := get(ctx, c, a.URL)
	if err != nil {
		return "", fmt.Errorf("%s をダウンロードできませんでした: %w", a.Name, err)
	}
	defer body.Close()

	f, err := os.Create(dst)
	if err != nil {
		return "", fmt.Errorf("一時ファイルを作成できませんでした: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	pw := &progressWriter{total: a.Size, prog: prog, name: a.Name, last: time.Now()}
	if _, err := io.Copy(io.MultiWriter(f, h, pw), body); err != nil {
		return "", fmt.Errorf("ダウンロードが中断しました: %w", err)
	}
	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("ディスクへの書き込みに失敗しました: %w", err)
	}
	if a.Size > 0 && pw.written != a.Size {
		return "", fmt.Errorf("ダウンロードが不完全です。期待は %d バイト、実際は %d バイトです", a.Size, pw.written)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// get は許可リストに縛られた GET を 1 回送り、応答本文を返す。
func get(ctx context.Context, c *http.Client, rawURL string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if err := checkURL(req.URL); err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "artex-selfupdate")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// extractBinary はリリースパッケージから artex 実行ファイルを取り出す。
//
// パッケージ内の構造は artex-<バージョン>-<os>-<arch>/artex だが、ここでは完全なパスを組まず、ベース名で一致させる。
// バージョン番号はパッケージ名に一度出ており、1 文字間違えるとアップデート全体が失敗する。ベース名の方が変更に強い。
func extractBinary(zipPath, dst string) error {
	want := "artex"
	if runtime.GOOS == "windows" {
		want = "artex.exe"
	}
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("リリースパッケージを開けませんでした: %w", err)
	}
	defer zr.Close()

	for _, entry := range zr.File {
		if entry.FileInfo().IsDir() || !strings.EqualFold(path.Base(entry.Name), want) {
			continue
		}
		rc, err := entry.Open()
		if err != nil {
			return fmt.Errorf("%s を読み取れませんでした: %w", entry.Name, err)
		}
		defer rc.Close()

		f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			return fmt.Errorf("新しいバイナリを書き出せませんでした: %w", err)
		}
		defer f.Close()

		n, err := io.Copy(f, io.LimitReader(rc, maxBinarySize+1))
		if err != nil {
			return fmt.Errorf("%s を展開できませんでした: %w", entry.Name, err)
		}
		if n > maxBinarySize {
			return fmt.Errorf("リリースパッケージ内の実行ファイルが %s を超えているので、展開は拒否します", humanSize(maxBinarySize))
		}
		if n == 0 {
			return fmt.Errorf("リリースパッケージ内の %s は空のファイルです", want)
		}
		return f.Sync()
	}
	return fmt.Errorf("リリースパッケージに %s が見つかりません", want)
}

// checkWritable は、ディレクトリが書き込めることを先に確認する。これが無いと、root 以外での実行や、バイナリがシステムの
// ディレクトリにあるとき、数十 MB をダウンロードしたあと、入れ替えの瞬間に失敗する。
func checkWritable(dir string) error {
	probe, err := os.CreateTemp(dir, ".artex-update-probe-*")
	if err != nil {
		return fmt.Errorf("プログラムのディレクトリ %s は書き込めず、自動アップデートはできません（権限を確認するか、手動アップデートを使ってください）: %w", dir, err)
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return nil
}

// progressWriter は書き込み済みバイトを数え、報告の頻度を抑える。32KiB の塊ごとに SSE を 1 本送らないためだ。
type progressWriter struct {
	total   int64
	written int64
	name    string
	prog    Progress
	last    time.Time
}

func (w *progressWriter) Write(b []byte) (int, error) {
	w.written += int64(len(b))
	if time.Since(w.last) < 300*time.Millisecond {
		return len(b), nil
	}
	w.last = time.Now()
	pct := -1
	if w.total > 0 {
		pct = int(w.written * 100 / w.total)
	}
	w.prog(PhaseDownload, pct, fmt.Sprintf("ダウンロード中 %s / %s", humanSize(w.written), humanSize(w.total)))
	return len(b), nil
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

func short(sum string) string {
	if len(sum) > 12 {
		return sum[:12] + "…"
	}
	return sum
}
