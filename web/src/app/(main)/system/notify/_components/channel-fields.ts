// チャネルのフィールド表と、設定値の解析。
//
// ページと分けているのは、これがビューではなく**データ**だからである。チャネルごとにどのフィールドがあるか、
// それぞれどのコントロールを使うか、フォームのテキストと設定値（JSON）の双方向変換を書く。
// ファイルを分けると、チャネルを足すときはここだけを変え、ページ自体は変えなくてよい。
// チャネル種別の表示名と概要。フロントに置くのは文案だけに効き、バックエンドは知る必要がないから。
export const KIND_LABEL: Record<string, string> = {
  dingtalk: "DingTalk",
  feishu: "Feishu",
  wecom: "WeCom",
  webhook: "汎用 Webhook",
  telegram: "Telegram",
  email: "メール",
};

// 各チャネルの設定フィールド定義。
//
// フロントのフィールド表を意図的に残し、バックエンドに schema を配らせない。バックエンドは
// Validate（必須と形式）だけを担う。UI が要るのはレイアウトとコントロール種別で、見ているものは同じではない。
// 唯一の結合点は secret_keys である。どのフィールドをパスワード欄にするかはバックエンドが返す。
// どれが資格情報かはチャネル実装だけが知っている（WeCom は Webhook 全体が資格情報で、
// DingTalk はその中の secret だけ）。チャネルを足したとき、ここに項目が無いとフォームが空白になるだけで、
// 黙って失敗はしない（下の hasFields が知らせる）。
export type FieldKind = "text" | "password" | "number" | "select" | "textarea" | "switch" | "kv" | "list";
export interface FieldDef {
  key: string;
  label: string;
  kind: FieldKind;
  placeholder?: string;
  help?: string;
  options?: { value: string; label: string }[];
}
export const CHANNEL_FIELDS: Record<string, FieldDef[]> = {
  dingtalk: [
    {
      key: "webhook",
      label: "Webhook アドレス",
      kind: "text",
      placeholder: "https://oapi.dingtalk.com/robot/send?access_token=...",
    },
    {
      key: "secret",
      label: "署名用シークレット",
      kind: "password",
      help: "ボットのセキュリティ設定で「署名」を選んだ場合に入力します。「カスタムキーワード」を選んだ場合やセキュリティ設定が無効の場合は空欄にします",
    },
  ],
  feishu: [
    {
      key: "webhook",
      label: "Webhook アドレス",
      kind: "text",
      placeholder: "https://open.feishu.cn/open-apis/bot/v2/hook/...",
    },
    {
      key: "secret",
      label: "署名検証用シークレット",
      kind: "password",
      help: "ボットで「署名検証」を有効にしている場合に入力します。それ以外は空欄にします",
    },
  ],
  wecom: [
    {
      key: "webhook",
      label: "Webhook アドレス",
      kind: "text",
      placeholder: "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=...",
    },
  ],
  webhook: [
    { key: "url", label: "送信先 URL", kind: "text", placeholder: "https://your-endpoint.example.com/hook" },
    {
      key: "method",
      label: "リクエストメソッド",
      kind: "select",
      options: [
        { value: "POST", label: "POST（リクエストボディあり）" },
        { value: "PUT", label: "PUT（リクエストボディあり）" },
        { value: "PATCH", label: "PATCH（リクエストボディあり）" },
        { value: "GET", label: "GET（リクエストボディなし）" },
      ],
    },
    {
      key: "headers",
      label: "カスタムリクエストヘッダー",
      kind: "kv",
      help: "1 行に KEY=VALUE。例 Authorization=Bearer xxx",
    },
    {
      key: "body_template",
      label: "リクエストボディのテンプレート",
      kind: "textarea",
      help:
        "空欄の場合は組み込みのデフォルトテンプレートを使います。変数：{{.Title}} {{.Batch}} {{.Count}} {{.HomeURL}} {{.SentAt}}、" +
        "および range .Items 配下の .Name/.VulnClass/.Severity/.Summary/.Assets/.DetailURL/.StatusLabel。" +
        "文字列を挿入するときは {{.Xxx}} ではなく {{json .Xxx}} を使ってください。そうしないとタイトル内の引用符で JSON が壊れます。",
    },
  ],
  telegram: [
    { key: "bot_token", label: "Bot Token", kind: "password", placeholder: "123456:ABC-DEF..." },
    { key: "chat_id", label: "Chat ID", kind: "text", placeholder: "-1001234567890" },
    {
      key: "base_url",
      label: "API アドレス",
      kind: "text",
      placeholder: "https://api.telegram.org",
      help: "空欄の場合は公式のアドレスを使います。Bot API のリバースプロキシを自前で用意している場合に入力します",
    },
  ],
  email: [
    { key: "host", label: "SMTP サーバー", kind: "text", placeholder: "smtp.example.com" },
    {
      key: "port",
      label: "ポート",
      kind: "number",
      placeholder: "587",
      help: "587 は STARTTLS を使います。465 の場合は「暗黙的 TLS」を有効にしてください",
    },
    { key: "username", label: "アカウント", kind: "text" },
    { key: "password", label: "パスワード / 認証コード", kind: "password" },
    { key: "from", label: "送信者", kind: "text", placeholder: "artex@example.com" },
    { key: "to", label: "受信者", kind: "list", help: "複数のアドレスはカンマで区切ります" },
    {
      key: "tls",
      label: "暗黙的 TLS",
      kind: "switch",
      help: "465 ポートでは有効にします。587 では無効のままにします（自動で STARTTLS を使います）",
    },
  ],
};

export const SEVERITY_OPTIONS = [
  { value: "", label: "制限なし" },
  { value: "low", label: "低以上" },
  { value: "medium", label: "中以上" },
  { value: "high", label: "高以上" },
  { value: "critical", label: "重大のみ" },
];

export type ChannelForm = {
  name: string;
  kind: string;
  mode: "realtime" | "digest";
  enabled: boolean;
  ratePerMin: string;
  config: Record<string, unknown>;
  minSeverity: string;
  includeText: string;
  excludeText: string;
  taskIDsText: string;
  assetIDsText: string;
  onStatusChange: boolean;
};

export const emptyForm = (kind: string): ChannelForm => ({
  name: "",
  kind,
  mode: "realtime",
  enabled: true,
  ratePerMin: "",
  config: {},
  minSeverity: "",
  includeText: "",
  excludeText: "",
  taskIDsText: "",
  assetIDsText: "",
  onStatusChange: false,
});

// parseKV は「1行 KEY=VALUE」のテキスト領域を解析する。
export function parseKV(text: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const line of text.split("\n")) {
    const t = line.trim();
    if (!t) continue;
    const i = t.indexOf("=");
    if (i > 0) out[t.slice(0, i).trim()] = t.slice(i + 1).trim();
  }
  return out;
}
// parseIDs はカンマか空白で区切った id 一覧を解析する。
export function parseIDs(text: string): number[] {
  return text
    .split(/[\s,，]+/)
    .map((s) => s.trim())
    .filter(Boolean)
    .map((s) => Number(s))
    .filter((n) => Number.isFinite(n) && n > 0);
}
// parseKeywords は行かカンマで区切ったキーワード一覧を解析する（脆弱性種別名は空白を含み得るので、行かカンマで切る）。
export function parseKeywords(text: string): string[] {
  return text
    .split(/[\n,，]+/)
    .map((s) => s.trim())
    .filter(Boolean);
}
