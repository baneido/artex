// Package notify は、検出した脆弱性を IM とメールへ送るチャネルアダプタ層です。
//
// 層: このパッケージは葉であり、標準ライブラリだけに依存します。データベースも server も知りません。
// チャネル設定は map[string]any で渡します（notification_channels.config の JSONB 列に対応します）。
// 送る内容は Message で渡します。こう分けると、署名計算、UTF-8 の切り詰め、フィルタ一致のように
// 誤りやすい部分を PostgreSQL なしで単体テストでき、ホストは server 側で編成するだけになります。
//
// 並行の約束: Channel の実装は状態を持ってはいけません。同じ Channel インスタンスを、複数のチャネル設定
// （同じチャネルの複数ボットを含む）が同時に使い回します。資格情報はすべて cfg 引数から渡し、
// webhook URL のようなものを実装自身のフィールドへキャッシュしてはいけません。
package notify

// チャネル種別の識別子。値は notification_channels.kind の合法な集合でもあり、server 側の
// 許可リストで検証します（findings.status と同じく DB CHECK は使わず、後からチャネルを足しやすくします）。
const (
	KindDingTalk = "dingtalk" // DingTalk のカスタムボット
	KindFeishu   = "feishu"   // Feishu（Lark を含む）のカスタムボット
	KindWeCom    = "wecom"    // WeCom のグループボット
	KindWebhook  = "webhook"  // 汎用 Webhook。メソッド、ヘッダ、JSON テンプレートを指定する
	KindTelegram = "telegram" // Telegram Bot API
	KindEmail    = "email"    // SMTP メール
)

// イベント種別。notification_events.kind に対応します。
const (
	EventFindingCreated       = "finding_created"
	EventFindingStatusChanged = "finding_status_changed"
)

// InitKind は、config の kind が空のときの既定値です。
const InitKind = KindDingTalk

// severityRank は脆弱性の深刻度を比較できる序数へ写します。未知の深刻度は 0 を返すので、
// min_severity を設定すると未知の深刻度は外れます。疑わしいときは送らず、誤検知で画面を埋めません。
var severityRank = map[string]int{
	"low":      1,
	"medium":   2,
	"high":     3,
	"critical": 4,
}

// SeverityRank は深刻度の序数を返します。未知の深刻度は 0 です。
func SeverityRank(severity string) int { return severityRank[severity] }

// SeverityLabel は絵文字付きの深刻度名を返します。メッセージのタイトルとカードの色に使います。
// 未知の深刻度はそのまま返し、作りません。
func SeverityLabel(severity string) string {
	switch severity {
	case "critical":
		return "🔴 重大"
	case "high":
		return "🟠 高"
	case "medium":
		return "🟡 中"
	case "low":
		return "🔵 低"
	default:
		return severity
	}
}

// StatusLabel は対応状態を日本語にし、状態変更メッセージに使います。
func StatusLabel(status string) string {
	switch status {
	case "pending":
		return "未対応"
	case "in_progress":
		return "対応中"
	case "confirmed":
		return "確認済み"
	case "resolved":
		return "対応済み"
	case "fixed":
		return "修正済み"
	case "false_positive":
		return "誤検知"
	case "ignored":
		return "無視"
	case "duplicate":
		return "重複"
	case "risk_accepted":
		return "リスク受容"
	default:
		return status
	}
}

// AtLeast は severity が min のしきい値に達しているかを返します。min が空なら、しきい値は無く、常に通ります。
// 未知の severity の序数は 0 なので、空でない min ならすべて拒否されます（severityRank のコメントを参照）。
func AtLeast(severity, min string) bool {
	if min == "" {
		return true
	}
	return SeverityRank(severity) >= SeverityRank(min)
}
