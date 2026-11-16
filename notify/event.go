package notify

// Snapshot は notification_events.snapshot という JSONB 列の契約です。書き手は db 層の
// 脆弱性保存トランザクション、読み手は server 層の配信エンジンとフィルタ一致です。このパッケージに置くのは、
// これが「通知ドメイン」のペイロードだからです。db はシリアライズするだけで、フィールドの意味は知りません。
//
// 脆弱性フィールドを冗長に保存し、描画時に引き直さない理由: 脆弱性は後から名前、深刻度、状態を変えられます。
// 通知の内容は事案当時の結論を反映する必要があります。引き直すと「後から low に変えられた」という
// 危険な誤解を生みます。fan-out と描画も、findings、tasks、assets の 3 表を JOIN せずに済みます。
type Snapshot struct {
	// イベント種別: finding_created / finding_status_changed
	Kind      string  `json:"kind"`
	FindingID int64   `json:"finding_id"`
	TaskID    int64   `json:"task_id"`
	VulnClass string  `json:"vulnclass"`
	Name      string  `json:"name"`
	Severity  string  `json:"severity"`
	Summary   string  `json:"summary"`
	AssetIDs  []int64 `json:"asset_ids"`
	// kind=finding_status_changed のときだけ空ではありません。
	FromStatus string `json:"from_status,omitempty"`
	ToStatus   string `json:"to_status,omitempty"`
}

// Item は配信待ちの脆弱性 1 件で、チャネルの描画に使います。
type Item struct {
	FindingID int64
	Name      string
	VulnClass string
	Severity  string
	Summary   string
	// Assets は解決済みの資産の表示名（ドメインや IP）です。server 層が埋めます。
	// このパッケージはデータベースに触らないので、名前は取れません。
	Assets []string
	// DetailURL は脆弱性詳細への戻りリンクです。空は public_base_url 未設定で、描画時に省きます。
	DetailURL string
	// 状態変更イベント専用です。両方とも空でないとき「未対応 から 修正済み」と描画します。
	FromStatus string
	ToStatus   string
}

// IsStatusChange は、この項目が状態変更イベントかを報告します。
func (i Item) IsStatusChange() bool { return i.FromStatus != "" || i.ToStatus != "" }

// Title は項目の表示タイトルを返します。人が付けた name を優先し、無ければ脆弱性種別 vulnclass に戻ります。
// どちらも空のときはプレースホルダを使い、空のタイトルは出しません。
func (i Item) Title() string {
	if i.Name != "" {
		return i.Name
	}
	if i.VulnClass != "" {
		return i.VulnClass
	}
	return "(名称未設定の脆弱性)"
}

// Message はチャネル送信 1 回分の内容です。
type Message struct {
	// 単発のときは長さ 1、まとめ（digest）のときは 1 バッチです。
	// 空スライスは不正です。呼び出し側は 1 件以上を保証します。
	Items []Item
	// Batch=true のときまとめメッセージとして描画します（タイトルを変え、時間窓と件数を付けます）。
	Batch bool
	// WindowMinutes はまとめの周期（分）です。Batch=true のときの文面「直近 N 分」にだけ使います。
	// 描画時に time.Since を計算せず、設定から明示的に渡します。描画を決定的にしてテストしやすくするためです。
	WindowMinutes int
	// HomeURL はプラットフォーム画面のアドレス（グローバルな public_base_url）です。空なら画面への入口は付けません。
	HomeURL string
}
