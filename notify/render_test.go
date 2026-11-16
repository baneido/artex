package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateBytesKeepsValidUTF8(t *testing.T) {
	// このパッケージで最も重要な不変条件です。企業 WeChat はバイトで長さを限り、
	// 中国語は 1 字 3 バイトです。バイト位置で切る実装は漢字を半分にし、
	// 不正な UTF-8 を出して、プラットフォームに拒否されます。
	// 長さが互いに素な中英混在の入力で、切れる位置をすべて突きます。
	inputs := []string{
		"中文测试内容",
		"混合 mixed 内容 content",
		"a中b文c测d试e",
		"🔴🟠🟡🔵", // 4 バイトの emoji です。切り間違えるとより目立ちます
		strings.Repeat("漏洞", 100),
	}
	for _, in := range inputs {
		for max := 1; max <= len(in)+2; max++ {
			got := TruncateBytes(in, max)
			if !utf8.ValidString(got) {
				t.Fatalf("入力 %q max=%d: 不正な UTF-8 です %q", in, max, got)
			}
			if len(got) > max {
				t.Fatalf("入力 %q max=%d: 結果は %d バイトで上限を超えています", in, max, len(got))
			}
			// 切り詰めていないときは内容を変えてはいけません。
			if len(in) <= max && got != in {
				t.Fatalf("入力 %q max=%d: 上限を超えていないのに内容が変わりました -> %q", in, max, got)
			}
		}
	}
}

func TestTruncateBytesZeroMeansUnlimited(t *testing.T) {
	long := strings.Repeat("x", 10000)
	if got := TruncateBytes(long, 0); got != long {
		t.Fatal("max=0 は制限なしである必要があります")
	}
	if got := TruncateBytes(long, -5); got != long {
		t.Fatal("max<0 は制限なしである必要があります")
	}
}

func TestTruncateBytesEllipsisBudget(t *testing.T) {
	// max が省略記号より短いとき、省略記号を足して上限を超えてはいけません。
	got := TruncateBytes("abcdefgh", 1)
	if len(got) > 1 {
		t.Fatalf("max=1 の結果 %q の長さ %d は上限を超えています", got, len(got))
	}
	// 通常は省略記号が付きます。
	if got := TruncateBytes("abcdefgh", 5); !strings.HasSuffix(got, ellipsis) {
		t.Fatalf("省略記号が必要です。実際は %q です", got)
	}
}

func TestTruncateRunesCountsCharactersNotBytes(t *testing.T) {
	// TruncateBytes との単位の違いは残します。Telegram は文字数で限ります。
	// バイトで切ると、中国語のメッセージはおよそ 3 分の 1 になります。
	s := "一二三四五六七八九十"
	got := TruncateRunes(s, 5)
	if n := utf8.RuneCountInString(got); n != 5 {
		t.Fatalf("5 文字である必要があります。実際は %d 文字です (%q)", n, got)
	}
	// 同じ文字列をバイトで切ると、文字数は明らかに少なくなります。
	if utf8.RuneCountInString(TruncateBytes(s, 5)) >= 5 {
		t.Fatal("バイト単位が、文字単位と同じ文字数を返してはいけません")
	}
}

func TestOneLineCollapsesWhitespace(t *testing.T) {
	got := OneLine("一行目\n\n二行目\tタブあり   空白が続く", 0)
	if strings.ContainsAny(got, "\n\t") {
		t.Fatalf("空白はすべて畳む必要があります。実際は %q です", got)
	}
	if strings.Contains(got, "  ") {
		t.Fatalf("連続する空白を残してはいけません。実際は %q です", got)
	}
	// 切り詰めたあとも読めて、正当な文字列である必要があります。
	got = OneLine("一二三四五六七八九十", 4)
	if n := utf8.RuneCountInString(got); n != 4 {
		t.Fatalf("4 文字である必要があります。実際は %d です (%q)", n, got)
	}
}

func TestTruncateHTMLNeverCutsTagInHalf(t *testing.T) {
	// HTML をそのまま切ると `<a href="htt` のような残骸になり、プラットフォームはメッセージ全体を拒否します。
	s := `<b>标题</b>正文正文正文<a href="https://example.com/very/long/path">查看详情</a>`
	for max := 1; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateHTML(s, max)
		if n := utf8.RuneCountInString(got); max > 0 && n > max {
			t.Fatalf("max=%d: 結果は %d 文字で上限を超えています", max, n)
		}
		// 末尾に閉じていない `<` があってはいけません（最後の区間に `<` があり `>` がない）。
		if lt := strings.LastIndex(got, "<"); lt >= 0 && !strings.Contains(got[lt:], ">") {
			t.Fatalf("max=%d: 末尾のタグが切れています -> %q", max, got)
		}
	}
}

func TestAssetLineOmitsExcess(t *testing.T) {
	if got := assetLine(nil, 3); got != "" {
		t.Fatalf("資産が無いときは空である必要があります。実際は %q です", got)
	}
	if got := assetLine([]string{"a", "b"}, 3); got != "a、b" {
		t.Fatalf("上限以内はすべて列挙する必要があります。実際は %q です", got)
	}
	// 上限を超えたときは総数を書きます。書かないと、読者は載っていない資産の数を知りません。
	got := assetLine([]string{"a", "b", "c", "d", "e"}, 2)
	if !strings.Contains(got, "など 5 件") {
		t.Fatalf("総数 5 を書く必要があります。実際は %q です", got)
	}
}

func TestSeverityAndStatusLabels(t *testing.T) {
	if AtLeast("", "low") {
		t.Fatal("空の深刻度の序数は 0 です。どの下限でも止まる必要があります")
	}
	if !AtLeast("critical", "") {
		t.Fatal("空の下限は通す必要があります")
	}
	if got := StatusLabel("fixed"); got != "修正済み" {
		t.Fatalf("状態の対応が違います。実際は %q です", got)
	}
	// 未知の状態はそのまま返します。ラベルを作りません。
	if got := StatusLabel("weird_status"); got != "weird_status" {
		t.Fatalf("未知の状態はそのまま返す必要があります。実際は %q です", got)
	}
}

// TestTruncateHTMLNeverCutsEntity は、監査で指摘された抜けを覆います。
// 切り詰めは、途中で切れたタグを避けるだけでなく、途中で切れた HTML エンティティも避けます。
//
// `&amp;` が `&amp` になると、エンティティだけを見るパーサはメッセージ全体を拒否することがあります。
// 長いまとめメッセージはもともと多いので、その代償は大きいです。
func TestTruncateHTMLNeverCutsEntity(t *testing.T) {
	s := "aaaa&amp;bbbb&lt;cccc&quot;dddd"
	for max := 1; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateHTML(s, max)
		// 末尾に「& があり、対応する ; がない」エンティティの残骸を残してはいけません。
		if amp := strings.LastIndex(got, "&"); amp >= 0 && !strings.Contains(got[amp:], ";") {
			t.Fatalf("max=%d: 末尾にエンティティの残骸があります %q", max, got[amp:])
		}
		if strings.Contains(got, "&amp\x00") {
			t.Fatalf("max=%d: 壊れたエンティティがあります", max)
		}
	}
}
