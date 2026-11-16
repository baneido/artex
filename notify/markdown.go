package notify

import (
	"fmt"
	"strings"
)

// このファイルは Markdown 系チャネル（DingTalk、WeCom）が共用するメッセージ描画です。
// Feishu はカード JSON、Telegram は HTML、メールは HTML で、それぞれアダプタ側で描画します。

// maxAssetsShown は、メッセージに列挙する資産の上限です。1 件の脆弱性が何十もの資産に
// 紐づくことがあり、全部出すとメッセージが埋まって情報になりません。4 件目以降のドメインを IM で見る人はいません。
const maxAssetsShown = 3

// maxSummaryRunes は、要約を何文字まで縮めるかです。IM のメッセージは「詳細を見に行く合図」であり、
// レポート本体ではありません。全文はプラットフォームにあります。
const maxSummaryRunes = 120

// markdownReservedBytes は、メッセージ先頭（まとめの行、深刻度の分布、切り詰めの案内）と
// 末尾（プラットフォームへのリンク）に残すバイト数です。件数単位で詰めるとき、この分を予算から引き、
// 先頭と末尾が切れないようにします。そこが切れると、読者はどのバッチか、何件が未表示かを知れません。
const markdownReservedBytes = 320

// markdownEscape は Markdown のメタ文字をエスケープします。
//
// 必要な理由: 脆弱性のタイトル、要約、種別、資産の表示名は、すべて信頼できない出所です。
// タイトルと要約はモデルの出力であり、モデルは検証対象のレスポンスを読んでいます。資産の URL は
// スキャンで得た完全な URL で、対象が制御できるクエリ文字列を含みます。エスケープしないと、タイトルが
//
//	ログイン口の SQL インジェクション\n[緊急: ここからアカウントを確認](http://attacker.tld)
//
// のような脆弱性は、セキュリティ担当者の DingTalk や Feishu でクリックできる外部リンクになります。
// `![](http://attacker.tld/beacon)` は描画時にクライアントが取得し、「この脆弱性は既読になった」と相手に知らせ、閲覧者の IP を漏らします。悪意が無くても、注入された太字や
// 引用が、下にある重大な脆弱性を折りたたみの外へ押し出します。
//
// エスケープ対象は、見出し、リンク、強調、リスト、引用、取り消し線のように、構造を変えるか
// クリックできる要素を生む文字です。`\` は最初に処理します。後から足したバックスラッシュを、もう一度エスケープしないためです。
func markdownEscape(s string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		"`", "\\`",
		"*", `\*`,
		"_", `\_`,
		"[", `\[`,
		"]", `\]`,
		"(", `\(`,
		")", `\)`,
		"!", `\!`,
		"#", `\#`,
		">", `\>`,
		"|", `\|`,
		"~", `\~`,
	)
	return replacer.Replace(s)
}

// markdownText は信頼できないテキストを 1 行に畳んでエスケープし、Markdown 本文に使います。
// 1 行化はエスケープのもう半分です。改行そのものが新しいリスト項目や引用を作れ、
// エスケープ文字では止められません。
func markdownText(s string, maxRunes int) string {
	return markdownEscape(OneLine(s, maxRunes))
}

// markdownTitle はメッセージのタイトル（IM のタイトルバーやカードのタイトル）を返します。中身はエスケープしていない原文です。
//
// ここでは意図してエスケープしません。このタイトルは 4 つの描画器が共用します。Markdown 本文、Telegram の
// HTML、Feishu カードの plain_text、汎用 Webhook の JSON とメール件名です。それぞれの
// エスケープ規則は違います（Markdown のエスケープを HTML に入れるとバックスラッシュが見え、JSON に入れると
// データを汚します）。そのためエスケープは各出力側が行います。writeItem、feishuItemLines、
// telegramEscape を参照してください。以前、共用関数に Markdown エスケープを入れたところ、Telegram のメッセージに
// `\(1\)` のような見えるバックスラッシュが出ました。
func markdownTitle(m Message) string {
	if m.Batch {
		return fmt.Sprintf("検出事項のまとめ 全 %d 件", len(m.Items))
	}
	if len(m.Items) == 0 {
		return "脆弱性の通知"
	}
	it := m.Items[0]
	return fmt.Sprintf("[%s] %s", SeverityLabel(it.Severity), OneLine(it.Title(), 0))
}

// markdownBody はメッセージ本文を描画し、本文と実際に書いた件数を返します。
//
// kept は、この配信で本当に届いた件数です。呼び出し側は先頭の kept 件だけを配信済みにします。
// チャネルの長さ上限で外れた件は次のバッチに残し、一緒に成功にしてはいけません。
// これが「黙って消える」原因です。メッセージは切れても、配信記録は全部届いたことになり、
// 後半が一度も出ていないことを示す場所がありません。
//
// maxBytes<=0 は制限なしです。
func markdownBody(m Message, maxBytes int) (string, int) {
	if !m.Batch {
		if len(m.Items) == 0 {
			return "", 0
		}
		var b strings.Builder
		writeItem(&b, m.Items[0], "", true)
		// 1 件のメッセージは長くても送ります（最後の切り詰めで受けます）。脆弱性の一部でも、
		// 1 件も送らないよりましです。
		return TruncateBytes(b.String(), maxBytes), 1
	}

	footer := ""
	if m.HomeURL != "" {
		footer = fmt.Sprintf("\n[プラットフォームですべて見る](%s)\n", m.HomeURL)
	}
	kept := packItemCount(m.Items, maxBytes, markdownReservedBytes, footer, byteSize, func(it Item, idx int) string {
		var b strings.Builder
		writeItem(&b, it, fmt.Sprintf("%d. ", idx+1), false)
		return b.String()
	})

	items := m.Items[:kept]
	var b strings.Builder
	b.WriteString(markdownBatchIntro(m, items, len(m.Items)))
	for i, it := range items {
		writeItem(&b, it, fmt.Sprintf("%d. ", i+1), false)
	}
	b.WriteString(footer)
	return TruncateBytes(b.String(), maxBytes), kept
}

// markdownBatchIntro はまとめメッセージの冒頭を描画します。時間窓、件数、深刻度の分布です。
// これがあれば、まとめを受け取った人はプラットフォームを開かなくても、すぐ対応が要るかを判断できます。
//
// items は実際に入った件、total はこのバッチにある総数です。両者が違うときは、
// 次のメッセージに何件残るかを書きます。書かないと、読者は見出しの数字が全部だと思い、
// 一度も送っていない件は画面のどこにも存在しません。
func markdownBatchIntro(m Message, items []Item, total int) string {
	var b strings.Builder
	if m.WindowMinutes > 0 {
		fmt.Fprintf(&b, "**直近 %d 分の新規脆弱性 %d 件**", m.WindowMinutes, total)
	} else {
		fmt.Fprintf(&b, "**新規の脆弱性 %d 件**", total)
	}
	if extra := total - len(items); extra > 0 {
		fmt.Fprintf(&b, "（このメッセージは先頭 %d 件です。残り %d 件は次のメッセージで続きます）", len(items), extra)
	}
	// 深刻度の分布を出して、重大な件があるかをすぐ分かるようにします。数えるのはこのメッセージに
	// 実際に入った件だけです。「重大 3」と下で数えられる件数が一致します。
	counts := map[string]int{}
	for _, it := range items {
		counts[it.Severity]++
	}
	var parts []string
	for _, sev := range []string{"critical", "high", "medium", "low"} {
		if n := counts[sev]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", SeverityLabel(sev), n))
		}
	}
	if len(parts) > 0 {
		b.WriteString("\n" + strings.Join(parts, " / "))
	}
	b.WriteString("\n\n")
	return b.String()
}

// writeItem は脆弱性 1 件を描画します。
//
// prefix はまとめ一覧の番号です。single が true のときは完全版（要約と戻りリンクを含む）です。
// まとめ一覧は要約を 1 行だけにします。そうしないと 50 件のまとめが長い文書になります。
//
// 外部由来の内容（タイトル、種別、資産、要約）はすべて markdownText を通します。
// 1 行化とエスケープです。戻りリンクは管理者が設定した public_base_url から組み立てたもので、信頼できない内容ではなく、
// クリックできるリンクである必要があるので、そのまま出します。
func writeItem(b *strings.Builder, it Item, prefix string, single bool) {
	line := fmt.Sprintf("%s**%s %s**", prefix, SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if !single {
		// まとめモード: 1 行にし、資産と要約は縮めて後ろに付けます。
		var extras []string
		if a := assetLine(it.Assets, maxAssetsShown); a != "" {
			extras = append(extras, markdownText(a, 0))
		}
		if it.Summary != "" {
			extras = append(extras, markdownText(it.Summary, 60))
		}
		if len(extras) > 0 {
			line += " " + strings.Join(extras, " / ")
		}
		b.WriteString(line + "\n")
		return
	}
	b.WriteString(line + "\n")
	if it.IsStatusChange() {
		fmt.Fprintf(b, "**状態の変更**: %s から %s\n",
			markdownText(StatusLabel(it.FromStatus), 0), markdownText(StatusLabel(it.ToStatus), 0))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		fmt.Fprintf(b, "**種別**: %s\n", markdownText(it.VulnClass, 0))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		fmt.Fprintf(b, "**資産**: %s\n", markdownText(a, 0))
	}
	if it.Summary != "" {
		if s := markdownText(it.Summary, maxSummaryRunes); s != "" {
			fmt.Fprintf(b, "**要約**: %s\n", s)
		}
	}
	if it.DetailURL != "" {
		fmt.Fprintf(b, "[詳細を見る](%s)\n", it.DetailURL)
	}
}
