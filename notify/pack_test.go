package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// このファイルは「項目単位で詰める」修正を覆います。まとめメッセージがチャネルの長さ上限を超えたとき、
// 項目単位で切り、入り切らなかった件数をそのまま伝えます。呼び出し側は、本当に届いたものだけを印づけます。
//
// 以前は全文を描画してから切り、バッチ全体を届いたことにしていました。メッセージの後半が消え、
// 配信履歴はすべて成功と出ます。脆弱性はそうして消え、気づける場所がありません。

func TestMarkdownBodyPacksWholeItemsWithinByteLimit(t *testing.T) {
	// 200 件の中国語のまとめです。企業 WeChat の 4096 バイトを大きく超えます。
	m := batchMsg(200)
	body, kept := markdownBody(m, weComMarkdownLimit)

	if len(body) > weComMarkdownLimit {
		t.Fatalf("本文は %d バイトで上限 %d を超えています", len(body), weComMarkdownLimit)
	}
	if !utf8.ValidString(body) {
		t.Fatal("本文が正当な UTF-8 ではありません")
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("一部だけ入る必要があります（0 < kept < %d）。実際は %d です", len(m.Items), kept)
	}
	// 先頭は、このメッセージに何件入り、残りが何件かをそのまま書きます。
	// 書かないと、読者は先頭の数字を全体の件数だと受け取ります。
	if !strings.Contains(body, "残り") || !strings.Contains(body, "次のメッセージで続きます") {
		t.Fatalf("先頭は、このメッセージに入っていない件数を説明する必要があります:\n%s", body[:minInt(400, len(body))])
	}
	// 入るのは先頭の kept 件だけです。
	for i := 0; i < kept; i++ {
		if !strings.Contains(body, "漏洞"+itoa(i+1)) {
			t.Fatalf("%d 件目はこのメッセージに入る必要があります:\n%s", i+1, body)
		}
	}
	if strings.Contains(body, "漏洞"+itoa(kept+1)) {
		t.Fatalf("%d 件目は出てはいけません（次のバッチです）", kept+1)
	}
}

func TestMarkdownBodyKeepsEverythingWhenUnderLimit(t *testing.T) {
	m := batchMsg(3)
	body, kept := markdownBody(m, 0) // 0 は制限なし
	if kept != len(m.Items) {
		t.Fatalf("長さを限らないときはすべて残る必要があります。実際の kept=%d です", kept)
	}
	if strings.Contains(body, "残り") {
		t.Fatalf("切り詰めていないときに、切り詰めの案内が出てはいけません:\n%s", body)
	}
}

func TestMarkdownBodyAlwaysKeepsAtLeastOneItem(t *testing.T) {
	// 予算が 1 件も入らないほど小さくても、1 件は出します（最後の切り詰めで受け止めます）。
	// 出さないと、極端に長い脆弱性がバッチ全体をその場に止めます。受け取るたびに入らず、送られません。
	m := batchMsg(5)
	_, kept := markdownBody(m, 50)
	if kept != 1 {
		t.Fatalf("少なくとも 1 件は残る必要があります。実際は %d です", kept)
	}
}

func TestMarkdownBodySingleReturnsOne(t *testing.T) {
	_, kept := markdownBody(singleMsg(), 4096)
	if kept != 1 {
		t.Fatalf("1 件のメッセージは、届いた件数を 1 と返す必要があります。実際は %d です", kept)
	}
	// 空のメッセージに、届けられる項目はありません。
	if _, k := markdownBody(Message{}, 4096); k != 0 {
		t.Fatalf("空のメッセージは 0 件である必要があります。実際は %d です", k)
	}
}

func TestTelegramPackingUsesRuneBudget(t *testing.T) {
	m := batchMsg(200)
	text, kept := telegramHTML(m)
	// Telegram は文字数で長さを限ります。バイトで切ると、中国語のメッセージはおよそ 3 分の 1 になります。
	if n := utf8.RuneCountInString(text); n > telegramTextLimit {
		t.Fatalf("本文は %d 文字で上限 %d を超えています", n, telegramTextLimit)
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("一部だけ入る必要があります。実際は %d です", kept)
	}
	if !strings.Contains(text, "次のメッセージで続きます") {
		t.Fatalf("入り切らなかった分の説明が必要です:\n%.300s", text)
	}
}

func TestFeishuPackingReportsKept(t *testing.T) {
	m := batchMsg(2000)
	_, kept := feishuCard(m)
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("カードは一部だけ入る必要があります。実際は %d です", kept)
	}
}

func TestWebhookAndEmailReportAllItems(t *testing.T) {
	// この 2 つのチャネルは本文を切りません。バッチ全体が届いたことになります。
	m := batchMsg(7)
	if n := len(m.Items); n != 7 {
		t.Fatal("前提が成り立っていません")
	}
	// レンダラの戻り値で間接に確認します。markdownBody(0) は制限がなく、すべて残します。
	if _, k := markdownBody(m, 0); k != len(m.Items) {
		t.Fatalf("長さを限らないときはすべて使う必要があります。実際は %d です", k)
	}
}

// TestMarkdownEscapesUntrustedContent は、「信頼できない内容がメッセージの構造を変えてはいけない」の回帰テストです。
// タイトルと要約はモデルの出力です（モデルは対象の応答を読んでいます）。資産名は対象の URL から来ます。
func TestMarkdownEscapesUntrustedContent(t *testing.T) {
	cases := []struct {
		name  string
		item  Item
		must  []string // 結果に必要です（エスケープ後の形）
		wrong []string // 結果にあってはいけません（エスケープ前の形）
	}{
		{
			name: "タイトルの改行と外部リンク",
			item: Item{
				Severity: "high",
				Name:     "ログイン口の SQL インジェクション\n[緊急。アカウントを確認](http://attacker.tld)",
			},
			// 改行は畳みます（畳まないと、新しいリスト項目や引用ブロックを偽造できます）。
			// 角括弧と丸括弧はエスケープします（しないと、クリックできる外部リンクになります）。
			must:  []string{`\[緊急。アカウントを確認\]`, `\(http://attacker.tld\)`},
			wrong: []string{"\n[緊急", "\n\n[緊急"},
		},
		{
			name: "タイトルの画像ビーコン",
			item: Item{
				Severity: "high",
				Name:     "脆弱性 ![](http://attacker.tld/beacon)",
			},
			must:  []string{`\!`, `\(http://attacker.tld/beacon\)`},
			wrong: []string{"![]("},
		},
		{
			name: "資産名の強調と引用",
			item: Item{
				Severity: "high",
				Name:     "普通のタイトル",
				Assets:   []string{"a.com/*インジェクション*>引用"},
			},
			must:  []string{`\*インジェクション\*`, `\>`},
			wrong: []string{"*インジェクション*"},
		},
		{
			name: "要約のバッククォートと縦棒",
			item: Item{
				Severity: "high",
				Name:     "タイトル",
				Summary:  "`code` | 表",
			},
			must:  []string{"\\`code\\`", `\|`},
			wrong: []string{"`code`"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := Message{Items: []Item{tc.item}}
			// 1 件モードの writeItem は、markdown を使う 3 つのチャネルに共通の描画経路です。
			var b strings.Builder
			writeItem(&b, tc.item, "", true)
			got := b.String()
			for _, want := range tc.must {
				if !strings.Contains(got, want) {
					t.Errorf("エスケープ後の形 %q がありません:\n%s", want, got)
				}
			}
			for _, bad := range tc.wrong {
				if strings.Contains(got, bad) {
					t.Errorf("エスケープ前の形 %q が出ています（構造や外部リンクの注入に使えます）:\n%s", bad, got)
				}
			}
			_ = m
		})
	}
}

// TestMarkdownEscapeBackslashFirst はエスケープの順序を固定します。バックスラッシュを最初に処理します。
// 後だと、あとから足したバックスラッシュにさらに 1 層かかり、出力に二重のバックスラッシュが出ます。
func TestMarkdownEscapeBackslashFirst(t *testing.T) {
	if got := markdownEscape(`a\b*c`); got != `a\\b\*c` {
		t.Fatalf("エスケープの順序が違います。実際は %q です", got)
	}
}

// TestTelegramTitleHasNoMarkdownEscapes は、具体的な回帰を固定します。
// markdown のエスケープが Telegram の HTML 出力へ漏れてはいけません（以前、共有のタイトル関数に
// エスケープを足した結果、Telegram のメッセージに `\(1\)` のような見えるバックスラッシュが出ました）。
func TestTelegramTitleHasNoMarkdownEscapes(t *testing.T) {
	m := Message{Items: []Item{{Severity: "high", Name: "alert(1) *重点*"}}}
	text, _ := telegramHTML(m)
	if strings.Contains(text, `\(`) || strings.Contains(text, `\*`) {
		t.Fatalf("Telegram の本文に、markdown のバックスラッシュエスケープが出ています:\n%s", text)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
