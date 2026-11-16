package report

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
)

// 検出事項ページの「エクスポート」用レンダラ。findings の行を、一覧 Markdown、1 件ごとの Markdown、
// または CSV にする。JSON は server 層が DTO を直接直列化するので、ここでは扱わない。

// sortFindingsForExport は深刻度の降順、同じ深刻度では時刻の新しい順に並べる。一覧レポートのグループと同じ順だ。
func sortFindingsForExport(fs []*db.DBFinding) {
	sort.SliceStable(fs, func(i, j int) bool {
		ri, rj := sevRank[fs[i].Severity], sevRank[fs[j].Severity]
		if ri != rj {
			return ri < rj // sevRank が小さいほど深刻
		}
		return fs[i].CreatedAt.After(fs[j].CreatedAt)
	})
}

// findingTitle は脆弱性の表示名を取る。名称、種別、「未分類」の順だ。
func findingTitle(f *db.DBFinding) string {
	return nz(f.Name, nz(f.VulnClass, "未分類"))
}

// FindingsMarkdown は findings を 1 つの一覧レポートにまとめる（要約と深刻度ごとのグループ。
// 各件に種別、状態、所属タスク、証拠、詳細レポートを含む）。
func FindingsMarkdown(fs []*db.DBFinding, generatedAt time.Time) string {
	items := append([]*db.DBFinding(nil), fs...)
	sortFindingsForExport(items)

	var b strings.Builder
	b.WriteString("# 検出事項の一覧レポート\n\n")
	fmt.Fprintf(&b, "- **生成時刻**: %s\n", generatedAt.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "- **検出事項の総数**: %d 件\n\n", len(items))

	// 要約: 深刻度ごとの件数。
	counts := map[string]int{}
	for _, f := range items {
		counts[f.Severity]++
	}
	b.WriteString("## 要約\n\n")
	b.WriteString("| 深刻度 | 件数 |\n| --- | --- |\n")
	for _, s := range []struct{ key, label string }{
		{"critical", "重大"}, {"high", "高"}, {"medium", "中"}, {"low", "低"},
	} {
		fmt.Fprintf(&b, "| %s | %d |\n", s.label, counts[s.key])
	}
	b.WriteString("\n")

	if len(items) == 0 {
		b.WriteString("_該当する脆弱性はありません。_\n")
		return b.String()
	}

	b.WriteString("## 脆弱性の明細\n\n")
	for i, f := range items {
		fmt.Fprintf(&b, "### %d. [%s] %s\n\n", i+1, strings.ToUpper(nz(f.Severity, "info")), findingTitle(f))
		if f.VulnClass != "" {
			fmt.Fprintf(&b, "- **種別**: %s\n", f.VulnClass)
		}
		fmt.Fprintf(&b, "- **状態**: %s\n", nz(f.Status, "pending"))
		if desc := strings.TrimSpace(f.TaskDescription); desc != "" {
			fmt.Fprintf(&b, "- **所属タスク**: %s\n", desc)
		}
		fmt.Fprintf(&b, "- **検出時刻**: %s\n\n", f.CreatedAt.Format("2006-01-02 15:04:05"))
		if s := strings.TrimSpace(f.Summary); s != "" {
			fmt.Fprintf(&b, "%s\n\n", s)
		}
		if e := strings.TrimSpace(f.Evidence); e != "" {
			fmt.Fprintf(&b, "**証拠:**\n\n```\n%s\n```\n\n", e)
		}
		if rep := strings.TrimSpace(f.Report); rep != "" {
			b.WriteString("**詳細レポート:**\n\n")
			b.WriteString(rep)
			b.WriteString("\n\n")
		}
		b.WriteString(findingTrafficMarkdown(f, false))
		b.WriteString("---\n\n")
	}
	return b.String()
}

// SingleFindingMarkdown は脆弱性 1 件を独立した Markdown にする（「1 脆弱性 1 ファイル」のパッケージ用）。
func SingleFindingMarkdown(f *db.DBFinding, generatedAt time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# [%s] %s\n\n", strings.ToUpper(nz(f.Severity, "info")), findingTitle(f))
	if f.VulnClass != "" {
		fmt.Fprintf(&b, "- **種別**: %s\n", f.VulnClass)
	}
	fmt.Fprintf(&b, "- **深刻度**: %s\n", nz(f.Severity, "info"))
	fmt.Fprintf(&b, "- **状態**: %s\n", nz(f.Status, "pending"))
	if desc := strings.TrimSpace(f.TaskDescription); desc != "" {
		fmt.Fprintf(&b, "- **所属タスク**: %s\n", desc)
	}
	fmt.Fprintf(&b, "- **検出時刻**: %s\n", f.CreatedAt.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "- **生成時刻**: %s\n\n", generatedAt.Format("2006-01-02 15:04:05"))
	if s := strings.TrimSpace(f.Summary); s != "" {
		fmt.Fprintf(&b, "## 概要\n\n%s\n\n", s)
	}
	if e := strings.TrimSpace(f.Evidence); e != "" {
		fmt.Fprintf(&b, "## 証拠\n\n```\n%s\n```\n\n", e)
	}
	if rep := strings.TrimSpace(f.Report); rep != "" {
		b.WriteString("## 詳細レポート\n\n")
		b.WriteString(rep)
		b.WriteString("\n")
	}
	b.WriteString(findingTrafficMarkdown(f, true))
	return b.String()
}

var unsafeFilenameChars = regexp.MustCompile(`[^\p{Han}\p{L}\p{N}._-]+`)

// FindingFilename は「1 脆弱性 1 ファイル」用の安全な .md 名を作る。形は
// `critical_SQLインジェクション_#123.md`。パス区切りと制御文字を除き、zip 内の不正なパスを避ける。
func FindingFilename(f *db.DBFinding) string {
	sev := nz(f.Severity, "info")
	title := findingTitle(f)
	name := fmt.Sprintf("%s_%s_#%d", sev, title, f.ID)
	name = unsafeFilenameChars.ReplaceAllString(name, "_")
	name = strings.Trim(name, "._")
	if name == "" {
		name = fmt.Sprintf("finding_%d", f.ID)
	}
	// 防御: パスをもう一段剥がし、zip slip を防ぐ。
	name = path.Base(name)
	if len(name) > 120 {
		name = name[:120]
	}
	return name + ".md"
}

// FindingsCSV は findings を CSV にする（UTF-8 BOM 付き。Excel が日本語を正しく認識するため）。
// 長い report と evidence の全文は入れず、要約のフィールドだけにする。全文が要るときは Markdown か JSON でエクスポートする。
func FindingsCSV(fs []*db.DBFinding) []byte {
	items := append([]*db.DBFinding(nil), fs...)
	sortFindingsForExport(items)

	var buf bytes.Buffer
	buf.WriteString("\xEF\xBB\xBF") // UTF-8 BOM
	w := csv.NewWriter(&buf)
	_ = w.Write([]string{"ID", "名称", "種別", "深刻度", "状態", "所属タスク", "検出時刻", "概要", "トラフィック証拠数", "トラフィック証拠ID"})
	for _, f := range items {
		_ = w.Write([]string{
			fmt.Sprintf("%d", f.ID),
			findingTitle(f),
			f.VulnClass,
			nz(f.Severity, "info"),
			nz(f.Status, "pending"),
			f.TaskDescription,
			f.CreatedAt.Format("2006-01-02 15:04:05"),
			strings.TrimSpace(f.Summary),
			fmt.Sprint(len(f.TrafficBindings)), findingTrafficIDs(f),
		})
	}
	w.Flush()
	return buf.Bytes()
}

func findingTrafficIDs(f *db.DBFinding) string {
	ids := make([]string, 0, len(f.TrafficBindings))
	for _, b := range f.TrafficBindings {
		ids = append(ids, fmt.Sprint(b.ID))
	}
	return strings.Join(ids, ",")
}

func findingTrafficMarkdown(f *db.DBFinding, attachments bool) string {
	stale := f.Report != "" && f.EvidenceVersion != f.ReportEvidenceVersion
	if len(f.TrafficBindings) == 0 && !stale {
		return ""
	}
	var out strings.Builder
	out.WriteString("\n## 関連するトラフィック証拠\n\n")
	fmt.Fprintf(&out, "証拠バージョン: %d。関連付け数: %d。\n\n", f.EvidenceVersion, len(f.TrafficBindings))
	if stale {
		out.WriteString("証拠が変更されたため、詳細レポートは更新待ちです。\n\n")
	}
	for i, b := range f.TrafficBindings {
		fmt.Fprintf(&out, "%d. **証拠 #%d（%s）** `%s %s`、ステータス %d\n", i+1, b.ID, b.Role, b.Snapshot.Method, strings.ReplaceAll(b.Snapshot.URL, "`", "%60"), b.Snapshot.Status)
		if b.Note != "" {
			fmt.Fprintf(&out, "   %s\n", strings.ReplaceAll(b.Note, "\n", "\n   "))
		}
		if attachments {
			fmt.Fprintf(&out, "   [リクエスト本文](evidence/%d/%d/request.http) / [レスポンス本文](evidence/%d/%d/response.http)\n", f.ID, b.ID, f.ID, b.ID)
		}
	}
	out.WriteString("\n")
	return out.String()
}
