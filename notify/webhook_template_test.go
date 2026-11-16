package notify

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// このファイルは、汎用 Webhook テンプレートの能力の境界を固定します。
//
// このパッケージで、ユーザーが渡した文字列をコードとして評価するのはここだけです。
// 何ができて、何ができないかをテストで固定します。テンプレートの文脈にメソッドを足したり、
// FuncMap に readFile を足したりすると、能力が静かに広がり、差分は無害な小さな関数に見えます。

// TestTemplateContextHasNoMethods が最も重要です。
//
// text/template はエクスポートされたメソッドを呼びます（{{.Foo}} はフィールドもメソッドも取れます）。
// テンプレートの文脈が、エクスポートされたメソッドを持つ型に触れると、そのメソッドがテンプレート作者に開きます。
// この機能の文脈は、意図して純粋なデータだけです（エクスポートされたフィールドだけ、メソッドはゼロ）。
//
// これが失敗したら、webhookTemplateData か webhookItem にメソッドが足されています。
// 許す前に、そのメソッドで、出したくないものをテンプレートが読めるかを先に考えます。
func TestTemplateContextHasNoMethods(t *testing.T) {
	for _, v := range []any{webhookTemplateData{}, webhookItem{}} {
		typ := reflect.TypeOf(v)
		if n := typ.NumMethod(); n != 0 {
			var names []string
			for i := 0; i < n; i++ {
				names = append(names, typ.Method(i).Name)
			}
			t.Fatalf("%s が %d 個のメソッドを公開しています（%s）。text/template はこれらを呼べます。"+
				"テンプレート作者に、これらのメソッドの能力が開きます", typ.Name(), n, strings.Join(names, ", "))
		}
	}
}

// TestTemplateFuncsAreMinimal は、テンプレートに公開する関数の集合を固定します。
//
// FuncMap に関数が 1 つ増えると、能力が 1 つ増えます。今あるのは json と jsons だけです。
// 値を JSON の断片にします。ファイルは読めず、リクエストは送れず、コマンドは実行できません。
func TestTemplateFuncsAreMinimal(t *testing.T) {
	var got []string
	for name := range webhookTemplateFuncs {
		got = append(got, name)
	}
	sort.Strings(got)
	want := []string{"json", "jsons"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("テンプレート関数の集合が変わりました。実際は %v、期待は %v です。関数を足す前に、能力が広がらないことを確認してください"+
			"（ファイルの読み書き、ネットワークリクエスト、コマンド実行はできません）", got, want)
	}
}

// TestTemplateCannotReachUnknownData は、テンプレートからの範囲外アクセスを覆います。
// 無いものへのアクセスは失敗する必要があります。何かが表示されてはいけません。失敗情報に内部データを出してもいけません。
func TestTemplateCannotReachUnknownData(t *testing.T) {
	_, err := renderWebhookBody(`{"x": {{.Environment}}, "y": {{.Env}}}`, singleMsg())
	if err == nil {
		t.Fatal("存在しないフィールドへのアクセスはエラーになる必要があります")
	}
	// エラーに、テンプレート文脈の実内容（脆弱性のタイトルと要約）が出てはいけません。
	for _, leak := range []string{"SQLインジェクション", "パラメータ id"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("テンプレートのエラーがメッセージ内容 %q を漏らしています: %v", leak, err)
		}
	}
}

// TestTemplateRenderFailsPermanently では、壊れたテンプレートは設定の誤りです。再試行では直りません。
// 再試行可能と判定すると、壊れたテンプレートは配信のたびに 3 回のバックオフを空振りします。
func TestTemplateRenderFailsPermanently(t *testing.T) {
	cfg := map[string]any{
		"url":           "https://example.com/hook",
		"body_template": `{{.Items.`,
	}
	if err := (webhookChannel{}).Validate(cfg); err == nil {
		t.Fatal("テンプレートの文法エラーは、保存時に止まる必要があります")
	}
	// 検証を迂回して直接配信しても、永久失敗です。繰り返して再試行してはいけません。
	_, err := (webhookChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("壊れたテンプレートは永久失敗である必要があります。実際は %v です", err)
	}
}

// TestTemplateCanOnlyProduceJSON は、「テンプレートの描画結果は正当な JSON である」という制約を覆います。
// ついでに、「テンプレートでプレーンテキストを作り、別のプロトコルを起こす」使い方を止めます。
func TestTemplateCanOnlyProduceJSON(t *testing.T) {
	// 正当なテンプレートは通ります。
	ok := map[string]any{"url": "https://example.com/hook", "body_template": `{"t":{{json .Title}}}`}
	if err := (webhookChannel{}).Validate(ok); err != nil {
		t.Fatalf("正当なテンプレートは検証を通る必要があります: %v", err)
	}
	// JSON でない描画結果は拒否します。そのまま送ってはいけません。
	bad := map[string]any{"url": "http://127.0.0.1:1/hook", "body_template": `not json {{.Count}}`}
	_, err := (webhookChannel{}).Send(context.Background(), bad, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("JSON でない描画結果は永久失敗である必要があります。実際は %v です", err)
	}
	if !strings.Contains(err.Error(), "正当な JSON") {
		t.Errorf("エラー情報は JSON の問題だと説明する必要があります。実際は %v です", err)
	}
}
