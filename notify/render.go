package notify

import (
	"strings"
	"unicode/utf8"
)

const ellipsis = "…"

// TruncateBytes は s を max バイト以下に切り、結果が正当な UTF-8 になり、文字の途中で切らないようにします。
//
// 文字境界で切る理由: 企業 WeChat グループロボットの markdown には 4096 バイトの上限があります（文字数ではありません）。
// 中国語は 1 字 3 バイトです。バイト位置で切ると漢字が半分になり、不正な
// UTF-8 になります。プラットフォームはメッセージ全体を拒否するか、文字化けの四角を出します。ここでは予算の位置から
// 直前の rune 開始バイトまで戻します（utf8.RuneStart が継続バイト 0b10xxxxxx を判定します）。
//
// max<=0 は制限なしです。切ったあとに省略記号を足します。max が省略記号より短いときは足しません。
func TruncateBytes(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	budget := max - len(ellipsis)
	suffix := ellipsis
	if budget < 0 {
		// max が省略記号より短いときは、省略記号を捨てて切るだけにします。結果が max を超えないようにします。
		budget = max
		suffix = ""
	}
	cut := budget
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + suffix
}

// OneLine は複数行のテキストを 1 行にします。空白をすべて畳み、文字数で切ります。
// IM のタイトル行に使います。要約には改行が多く、そのまま表やタイトルへ入れるとレイアウトが壊れます。
// max<=0 は長さの制限なしです。
func OneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	return TruncateRunes(s, max)
}

// TruncateRunes は s を max 文字以下に切ります（バイトではありません）。超えたときは省略記号を足します。
// max<=0 は制限なしです。
//
// TruncateBytes との違いはプラットフォームの単位です。企業 WeChat はバイト、Telegram は文字数です。
// 単位を間違えてもエラーにはなりません。想定よりずっと短く切れるだけです（中国語は 1 字 = 3 バイトで、
// 4096 バイトで切ると約 1365 字になります）。そのため両方の関数を残し、チャネルごとに選びます。
func TruncateRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 1 {
		return string(runes[:max])
	}
	return string(runes[:max-1]) + ellipsis
}

// TruncateHTML は HTML 断片を文字数で切り、タグの途中を残しません。
//
// HTML を文字数でそのまま切ると `<a href="htt` のような欠けたタグになり、プラットフォームのパーサは
// エラーでメッセージ全体を拒否するか、その後の本文を属性値として飲み込みます。ここでは先に文字数で切り、
// 末尾に閉じていない `<` があれば、その前まで戻します。
//
// タグの対応は取りません（</b> の補完などはしません）。Telegram の HTML パーサは閉じていないタグを自動で閉じます。
// 自分で対応を取るには、属性内の引用符、コメント、自己閉じタグを扱う必要があり、複雑さに対して得るものが小さいです。
func TruncateHTML(s string, max int) string {
	if max <= 0 || len([]rune(s)) <= max {
		return s
	}
	cut := TruncateRunes(s, max)
	// 末尾が `<` で始まる断片（最後の `<` の後に `>` が無い）なら、`<` の前まで戻します。
	if lt := strings.LastIndex(cut, "<"); lt >= 0 && !strings.Contains(cut[lt:], ">") {
		cut = cut[:lt]
	}
	// 末尾が切れた HTML エンティティ（`&amp;` が `&amp` になるなど）も、同じように戻します。
	// エンティティだけを認識するパーサでは、断片がメッセージ全体の拒否につながります。長さ上限を超える
	// まとめメッセージは普通にあり、そのために通知全体を失う価値はありません。
	if amp := strings.LastIndex(cut, "&"); amp >= 0 && !strings.Contains(cut[amp:], ";") {
		cut = cut[:amp]
	}
	return cut
}

// packItemCount は、予算内に完全な形で何件入るかを計算します。まとめメッセージを件単位で詰めるためです。
//
// 件単位にする理由: 全文を描画してから切ると、後半の項目が消えます。
// それでも配信記録は届いたことになります。メッセージを見ても配信履歴を見ても分からず、
// 脆弱性が消えます。件単位で詰めると、入らなかった項目は保存されたまま次のバッチになり、
// 呼び出し側が受け取る kept が、このメッセージで本当に届いた件数になります。
//
// 引数: maxSize<=0 は制限なしです。reserve はメッセージの先頭と末尾に残す量です。
// size が計量します（プラットフォームごとに単位が違います。企業 WeChat と DingTalk はバイト、Telegram は文字数です。
// 単位を間違えてもエラーにはなりません。中国語のメッセージが上限よりずっと小さく圧縮されるだけです）。
// render は idx 番目を実際のテキストにします。長さは内容で変わるので、見積もりでは足りません。
//
// 項目が残っていれば、少なくとも 1 を返します。1 件が極端に長くてもその 1 件は送り、呼び出し側の
// 最終切り詰めで受けます。そうしないと、長い 1 件の脆弱性がバッチ全体をその場に止めます。
func packItemCount(items []Item, maxSize, reserve int, footer string, size func(string) int, render func(Item, int) string) int {
	if maxSize <= 0 {
		return len(items)
	}
	budget := maxSize - reserve - size(footer)
	if budget < 0 {
		budget = 0
	}
	used := 0
	for i, it := range items {
		used += size(render(it, i))
		if used > budget && i > 0 {
			return i
		}
	}
	return len(items)
}

// byteSize / runeSize は packItemCount の 2 つの計量単位です。名前を付けると、呼び出し側に
// 裸の func(s string) int クロージャが出ず、どちらの単位かが一目で分かります。
func byteSize(s string) int { return len(s) }
func runeSize(s string) int { return utf8.RuneCountInString(s) }

// assetLine は資産の一覧を 1 行の表示テキストにします。limit を超えた分は省き、総数を書きます。
// 1 件の脆弱性が何十もの資産に紐づくことがあり、全部出すとメッセージが埋まります。
func assetLine(assets []string, limit int) string {
	if len(assets) == 0 {
		return ""
	}
	if limit <= 0 || len(assets) <= limit {
		return strings.Join(assets, "、")
	}
	return strings.Join(assets[:limit], "、") + " など " + itoa(len(assets)) + " 件"
}

// itoa は strconv.Itoa の短い別名で、表示テキストの連結だけに使います。あちこちに strconv を import しないためです。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
