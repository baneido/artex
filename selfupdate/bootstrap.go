package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

// smokeEnv は、スモークテストが起動した子プロセスに Bootstrap をそのまま飛ばさせる。
//
// 厳密には無くても事故にはならない。子プロセスの os.Executable() は artex.new で、導かれる
// パスはすべて .new 接頭辞が付き、本当のアップデートファイルには触れない。ただしこの偶然に依存するのは脆い。
// 明示的に短絡した方が一目で分かり、子プロセスの無駄なディスク探索も省ける。
const smokeEnv = "ARTEX_SELFUPDATE_SMOKE"

// Action は Bootstrap が main に渡す指示だ。
type Action int

const (
	// Continue: 通常どおり server を起動する。
	Continue Action = iota
	// Restart: すぐに ExitRestart で終了し、守護スクリプトに再起動させる。
	Restart
)

// State は今回の起動時のアップデート状態で、/api/update/check がフロントへ
// 「前回のアップデートは成功したか、ロールバックされたか」をそのまま伝える。
type State struct {
	Pending     bool   // 入れ替え後、安定をまだ確認していない
	RolledBack  bool   // 今回の起動で自動ロールバックを実行した直後
	FailedStage bool   // 一時ファイルの検証またはスモークが通らず、破棄した
	Detail      string // ユーザー向けの 1 文の説明
}

// Bootstrap は main のいちばん最初に動く。ポート待ちやデータベースを開くより前に呼ぶ。
//
// 3 つの局面:
//
//	① 一時ファイル artex.new がある → 検証とスモーク。通れば入れ替えて再起動を要求する。通らなければ破棄し、旧版を動かし続ける
//	② 印のファイルだけが残る → 入れ替え直後なので、試行を 1 回積む。連続失敗が回数に達したらロールバックする
//	③ 何も無い → 通常起動
func Bootstrap() (Action, State) {
	if os.Getenv(smokeEnv) != "" {
		return Continue, State{}
	}
	p, err := ResolvePaths()
	if err != nil {
		log.Printf("[update] 自前準備を飛ばします: %v", err)
		return Continue, State{}
	}

	if _, err := os.Stat(p.New); err == nil {
		return applyStaged(p)
	}

	m, ok := readMarker(p.Marker)
	if !ok {
		return Continue, State{}
	}
	return confirmOrRollback(p, m)
}

// applyStaged は「一時ファイルがある」局面を扱う。検証が通れば入れ替え、失敗すれば破棄する。
//
// ここがアップデート経路で実行ファイルを上書きする唯一の場所で、最後の門でもある。スモークテストは
// ダウンロードの破損、アーキテクチャの選び間違い、動的リンクの欠落を止める。動かないバイナリを通すと、
// 守護スクリプトは休まず繰り返し起動し、Go のコードは走る機会が無く、自動ロールバックも始められない。
func applyStaged(p Paths) (Action, State) {
	m, _ := readMarker(p.Marker)

	if err := verifyStaged(p); err != nil {
		log.Printf("[update] 一時保存した新しいバージョンは検証を通らなかったので破棄し、現在のバージョンを動かし続けます: %v", err)
		cleanStaged(p)
		_ = os.Remove(p.Marker)
		return Continue, State{FailedStage: true, Detail: "新しいバージョンの検証に失敗したので、破棄しました: " + err.Error()}
	}

	if err := swap(p); err != nil {
		log.Printf("[update] 入れ替えに失敗したので、現在のバージョンを動かし続けます: %v", err)
		cleanStaged(p)
		_ = os.Remove(p.Marker)
		return Continue, State{FailedStage: true, Detail: "入れ替えに失敗しました: " + err.Error()}
	}

	// 入れ替えは成功した。印は残し、次の起動（動くのは新版）が安定を確認する。
	m.Attempts = 0
	if m.StagedAt == 0 {
		m.StagedAt = time.Now().Unix()
	}
	if err := writeMarker(p.Marker, m); err != nil {
		log.Printf("[update] アップデートの印を書き込めませんでした（自動ロールバックができなくなります）: %v", err)
	}
	log.Printf("[update] %s へ入れ替えました。再起動のため終了します（exit %d）", orUnknown(m.To), ExitRestart)
	return Restart, State{Pending: true}
}

// confirmOrRollback は「入れ替え後の起動」を扱う。試行回数を積み、上限を超えたら旧版を戻す。
//
// カウントが増えるのは Go のコードが動き始めてからなので、覆うのは「実行はできるが初期化で落ちる」
// 障害だ（設定の非互換、ポートの占有、DB マイグレーションの失敗）。「そもそも exec できない」は入れ替え前の
// スモークテストが止める。両方が揃って完全になる。
func confirmOrRollback(p Paths, m marker) (Action, State) {
	m.Attempts++
	if m.Attempts > maxAttempts {
		if err := rollback(p); err != nil {
			// ロールバックまで失敗したら、これ以上再起動しない。無限再起動になる。印を消し、
			// プロセスは今の状態で起動する。起動できなければ、ユーザーは少なくともログで理由を見られる。
			log.Printf("[update] 新しいバージョンは %d 回連続で起動に失敗し、ロールバックも失敗しました: %v", maxAttempts, err)
			_ = os.Remove(p.Marker)
			return Continue, State{Detail: "新しいバージョンの起動に失敗し、ロールバックも失敗しました: " + err.Error()}
		}
		log.Printf("[update] 新しいバージョンは %d 回連続で起動に失敗したので %s へロールバックしました。再起動のため終了します（exit %d）",
			maxAttempts, orUnknown(m.From), ExitRestart)
		_ = os.Remove(p.Marker)
		return Restart, State{RolledBack: true, Detail: fmt.Sprintf("新しいバージョンの起動に失敗したので、%s へロールバックしました", orUnknown(m.From))}
	}
	if err := writeMarker(p.Marker, m); err != nil {
		log.Printf("[update] アップデートの印を更新できませんでした: %v", err)
	}
	log.Printf("[update] 新しいバージョンを起動しています（%d/%d 回目）。安定して動いたあと、アップデートを確定します",
		m.Attempts, maxAttempts)
	return Continue, State{Pending: true}
}

// Settle は新しいバージョンが安定して動いたことを確認し、アップデートの印を消す。
//
// main が HTTP の待ち受けを始めたあと、遅らせて呼ぶ。この時間を生きて初めて確定する。そうでなければ印は残り、
// 次の起動で試行回数を積み続け、ロールバックが起きる。
func Settle() {
	p, err := ResolvePaths()
	if err != nil {
		return
	}
	settle(p)
}

func settle(p Paths) {
	if _, ok := readMarker(p.Marker); !ok {
		return // アップデート後の起動ではない。することは無い
	}
	if err := os.Remove(p.Marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("[update] アップデートの印を消せませんでした: %v", err)
		return
	}
	log.Printf("[update] 新しいバージョンは安定して動いています。アップデートは完了です（前のバージョンは %s に残します）", p.Old)
}

// SettleDelay は「新しいバージョンは生きている」と判定するのに必要な稼働時間だ。
const SettleDelay = 30 * time.Second

// verifyStaged は一時ファイルを検証する。先に SHA256 を照合し、そのあと実際に起動して 1 回動かす。
func verifyStaged(p Paths) error {
	want, err := os.ReadFile(p.Sum)
	if err != nil {
		return fmt.Errorf("検証和を読み取れませんでした: %w", err)
	}
	got, err := fileSHA256(p.New)
	if err != nil {
		return fmt.Errorf("検証和を計算できませんでした: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(string(want)), got) {
		return errors.New("SHA256 が一致しません（ダウンロードの破損、または改ざん）")
	}
	return smokeTest(p.New)
}

// smokeTest は -h で新しいバイナリを起動し、現在のシステムで本当に実行できることを確認する。
// ダウンロードの途中切れ、アーキテクチャの選び間違い（exec format error）、依存の欠落といった一群を止められる。
func smokeTest(bin string) error {
	if err := os.Chmod(bin, 0o755); err != nil {
		return fmt.Errorf("実行権限を付けられませんでした: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "-h")
	cmd.Env = append(os.Environ(), smokeEnv+"=1")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return errors.New("スモークテストがタイムアウトしました（新しいバイナリが応答しません）")
	}
	if err != nil {
		snippet := strings.TrimSpace(string(out))
		if len(snippet) > 300 {
			snippet = snippet[:300] + "…"
		}
		return fmt.Errorf("スモークテストに失敗しました: %v: %s", err, snippet)
	}
	return nil
}

// swap は現在のバイナリを、一時保存した新しいバージョンへ入れ替える。
//
// Unix も Windows も、実行中の実行ファイルの rename は許す（Windows が禁じるのは削除と
// 上書きで、rename は含まれない）。そのためここではプラットフォームを分けず、自分を先に止める必要も無い。
func swap(p Paths) error {
	// Windows の rename は既存の対象を上書きしない。前回のアップデートが残した .old は先に消す。
	if err := os.Remove(p.Old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("古いバックアップ %s を消せませんでした: %w", p.Old, err)
	}
	if err := os.Rename(p.Current, p.Old); err != nil {
		return fmt.Errorf("現在のバージョンをバックアップできませんでした: %w", err)
	}
	if err := os.Rename(p.New, p.Current); err != nil {
		// 入れ替えは失敗したが、現在のバージョンは既に移動している。そのまま戻す。戻さないと次回起動に実行ファイルが無い。
		if rerr := os.Rename(p.Old, p.Current); rerr != nil {
			return fmt.Errorf("新しいバージョンの組み込みに失敗し（%v）、現在のバージョンの復元も失敗しました: %w", err, rerr)
		}
		return fmt.Errorf("新しいバージョンを組み込めませんでした: %w", err)
	}
	_ = os.Remove(p.Sum)
	return nil
}

// rollback は、swap がバックアップした旧版を戻す。
func rollback(p Paths) error {
	if _, err := os.Stat(p.Old); err != nil {
		return fmt.Errorf("ロールバックできるバックアップ %s がありません: %w", p.Old, err)
	}
	// 起動できない新版は、消さずに .failed へ移して調査に残す。
	failed := p.Current + ".failed"
	_ = os.Remove(failed)
	if err := os.Rename(p.Current, failed); err != nil {
		return fmt.Errorf("失敗したバージョンを移動できませんでした: %w", err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		return fmt.Errorf("旧バージョンを復元できませんでした: %w", err)
	}
	return nil
}

// Rollback は /api/update/rollback の実装で、前のバージョンへ自ら戻す。
// 行うのは入れ替えだけで、再起動は同じく守護スクリプトに任せる（呼び出し側が続けて ExitRestart で終了する）。
func Rollback() error {
	p, err := ResolvePaths()
	if err != nil {
		return err
	}
	if _, err := os.Stat(p.Old); err != nil {
		return errors.New("ロールバックできる前のバージョンがありません（" + p.Old + " が存在しません）")
	}
	cleanStaged(p)
	if err := smokeTest(p.Old); err != nil {
		return fmt.Errorf("前のバージョンを実行できないので、ロールバックは拒否します: %w", err)
	}
	// 現在とバックアップを入れ替える。ロールバックのあと、もう一度戻せる。
	tmp := p.Current + ".swap"
	_ = os.Remove(tmp)
	if err := os.Rename(p.Current, tmp); err != nil {
		return fmt.Errorf("現在のバージョンを移動できませんでした: %w", err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		_ = os.Rename(tmp, p.Current)
		return fmt.Errorf("前のバージョンを組み込めませんでした: %w", err)
	}
	if err := os.Rename(tmp, p.Old); err != nil {
		log.Printf("[update] ロールバック後のバックアップ整理に失敗しました（動作には影響しません）: %v", err)
	}
	_ = os.Remove(p.Marker)
	return nil
}

// HasBackup は、ロールバックできる前のバージョンがあるかを報告する。フロントがロールバックボタンを出すかの判断に使う。
func HasBackup() bool {
	p, err := ResolvePaths()
	if err != nil {
		return false
	}
	_, err = os.Stat(p.Old)
	return err == nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "不明なバージョン"
	}
	return s
}
