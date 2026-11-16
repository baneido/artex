package agent

import (
	"log"
	"path/filepath"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/noaadapter"
)

// noaWarn returns a diagnostics sink tagging non-fatal noa messages with the
// session, routed through the package logger (agents have no per-instance one).
func noaWarn(session string) func(string) {
	return func(msg string) { log.Printf("[noa] %s: %s", session, msg) }
}

// noa は norma v0.4.0 が導入した「モデル駆動のコンテキスト圧縮」である。プラットフォームの実験機能として、ユーザーが
// システム設定で切り替える。組み込み compaction とは排他である。noaadapter.Enable が唯一の入口で、一度に
// コンテキストの接管器（Compactor）、Compress ツール、常駐プロンプト 3 つを載せる。Enable を呼ばなければオフで、
// 組み込み compaction はそのまま動く。スイッチは各 agent が注入する noaEnabledFn が解決し、run ごとに 1 回読む。
// そのため切り替えは、その後に起動する run にだけ効き、agent の再構築は要らない。

// enableNoa は、解決器が有効と報告したときに noa を opts へ接続する。archiveRoot は圧縮原文を永続化する基準ディレクトリである
// （グローバルな workDir を取る。各 agent は一律に <workDir>/noa へ置き、タスクやインテントのディレクトリには散らさない）。sessionID
// がその下のアーカイブ子ディレクトリ名になる（グローバルに一意なので、同じ基準ディレクトリ内では衝突しない）。
//
// noa は実験機能である。接続に失敗しても、実際のタスクを中断してはならない。エラー時は onWarn で報告し、組み込み圧縮へ戻す。
// 有効化に成功したら opts.Compaction を消し、agentcore が「コンテキスト管理器が 2 つ同時に設定されている」と警告しないようにする。
func enableNoa(opts *agentcore.Options, enabled func() bool, archiveRoot, sessionID string, onWarn func(string)) {
	if enabled == nil || !enabled() {
		return
	}
	if opts.OnWarn == nil {
		opts.OnWarn = onWarn
	}
	if err := noaadapter.Enable(opts, noaadapter.Options{
		ArchiveBaseDir: filepath.Join(archiveRoot, "noa"),
		SessionID:      sessionID,
		OnWarn:         onWarn,
	}); err != nil {
		if onWarn != nil {
			onWarn("noa 圧縮の有効化に失敗したため、組み込み圧縮へ戻します:" + err.Error())
		}
		return
	}
	// Compactor は Compaction を覆う。ただし両方が残ると agentcore が毎回警告するので、明示的に消す。
	opts.Compaction = nil
}
