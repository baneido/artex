"use client";

// LLM 再試行設定の共通部品。5層の再試行それぞれの「回数 + 間隔」。
//
// 5層は内側から、接続確立（SDK）、空レスポンス（SDK）、同じ provider の安全窓、ローテーション遮断、インテント再実行。
// 前の3層はエンドポイントに付くので、各モデル設定がグローバル既定を上書きできる。後ろの2層はプロセス単位で、グローバルに1つだけ。
//
// 入力はすべて同じ「空のまま = 設定しない」という意味で、バックエンドの db.RetryRule と同じ。
//   回数   空/0 は組み込み既定 | -1 はこの層の再試行をオフ | >0 はこの回数を使う
//   間隔   空/0 はこの層本来の指数バックオフ | >0 はこの固定ミリ秒間隔に変える

import * as React from "react";

import { Loader2Icon, SaveIcon } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api } from "@/lib/api";
import type { LLMRetryOverride, LLMRetryPolicy, LLMRetryRule } from "@/lib/types";

export const ZERO_RULE: LLMRetryRule = { attempts: 0, interval_ms: 0 };
export const ZERO_OVERRIDE: LLMRetryOverride = {
  connect: ZERO_RULE,
  empty: ZERO_RULE,
  stream: ZERO_RULE,
};
const ZERO_POLICY: LLMRetryPolicy = {
  ...ZERO_OVERRIDE,
  breaker: ZERO_RULE,
  intent: ZERO_RULE,
};

type LayerMeta = {
  title: string;
  /** この層の再試行がどこで、誰が実行するか */
  where: string;
  /** どんなエラーがこの層へ来るか。ステータスコードまで書く。推測させない */
  trigger: string;
  /** 似ているがこの層へ【来ない】エラー。記入しても反応が無く、バグだと思わせないため */
  skips?: string;
  desc: string;
  attemptsLabel: string;
  /** 回数を空にしたときの既定値。プレースホルダ用 */
  defAttempts: number;
  /** 間隔を空にしたときの既定方針。プレースホルダ用 */
  defInterval: string;
  /** 回数に -1 を入れたときの意味 */
  offHint: string;
};

export const RETRY_LAYERS = {
  connect: {
    title: "接続確立のリトライ",
    where: "SDK · 200 を受け取る前",
    trigger:
      "接続できない、またはまだ 200 を受け取っていない場合：接続リセット / 読み書きのタイムアウト / DNS 失敗などのネットワーク層のエラー、および HTTP 408、429、500、502、503、504。",
    skips:
      "それ以外のステータスコード（400 / 401 / 403 / 404 / 413 / 422 など）は確定的な拒否で、再送しても同じく失敗するため、そのまま上位に返します。",
    desc: "同じリクエストをそのまま再送します。ストリームが始まったあと（200 を受け取ったあと）に途中で切れた場合は、この層の対象外です。",
    attemptsLabel: "リトライ回数",
    defAttempts: 3,
    defInterval: "0.5s→1s→2s の指数（上限 8s）",
    offHint: "-1 = 一度もリトライせず、失敗したらすぐ上位に返す",
  },
  empty: {
    title: "空レスポンスのリトライ",
    where: "SDK · openai 形式のみ",
    trigger:
      "HTTP 200 で finish_reason も正常な stop なのに、レスポンス全体にコンテンツブロックが 1 つもない場合です。ゲートウェイの空フレーム、思考フィールドのフレーム欠落、サンプリングの不調でこうなります。",
    skips:
      "max_tokens による切り詰めで内容がない場合は対象外です（出力上限を上げて解決するもので、再送しても同じ結果になります）。",
    desc: "prompt 全体を再送するため、長いコンテキストではコストが高くなります。回数は大きくしないほうがよいでしょう。",
    attemptsLabel: "リトライ回数",
    defAttempts: 2,
    defInterval: "0.5s→1s→2s の指数（上限 8s）",
    offHint: "-1 = 空レスポンスをそのまま返す",
  },
  stream: {
    title: "同一 provider の安全ウィンドウでのリトライ",
    where: "本プロジェクト · 出力を渡す前",
    trigger:
      "ストリームが確立したあと（200 を受け取ったあと）に問題が起きた場合：接続の途中切断、プロバイダーの overloaded、ストリーム内の 429 / 5xx エラーイベント。かつ、呼び出し元に token を 1 つも渡していないとき。",
    skips:
      "クォータ切れ（402 / insufficient_quota、ローテーションで設定を切り替え）、コンテキスト超過（413 / context length、圧縮で対応）、400 / 401 / 403 / 404 / 422 の確定的な拒否は、いずれもリトライしません。",
    desc: "同じ設定で同じリクエストを再実行します。出力をまだ何も渡していないため、再実行してもモデル出力やツール実行は重複しません。",
    attemptsLabel: "リトライ回数",
    defAttempts: 2,
    defInterval: "0.5s→1s の指数（上限 4s）",
    offHint: "-1 = ストリーム切断をそのまま外側のインテント再実行に任せる",
  },
  breaker: {
    title: "ローテーションの遮断",
    where: "本プロジェクト · プロセス単位、全体で 1 つ",
    trigger:
      "一時的な失敗（429、5xx、ネットワークエラー）が連続してしきい値に達すると遮断します。残高不足（402）、キー失効（401 / 403）、モデルが存在しない（404）といった確定的な失敗はしきい値に関係なく、1 回目で遮断します。",
    skips:
      "1 回成功すればカウントをリセットするため、たまに不調になるだけの設定が少しずつ遮断に近づくことはありません。",
    desc: "遮断後はクールダウンに入り、その間ローテーションはこの設定をスキップします。状態はデータベースに保存され、再起動しても失われません。",
    attemptsLabel: "連続何回の失敗で遮断するか",
    defAttempts: 3,
    defInterval: "1min→5min→30min の段階",
    offHint: "-1 = 一時的な失敗では遮断しない（確定的な失敗では遮断する）",
  },
  intent: {
    title: "インテント再実行",
    where: "本プロジェクト · プロセス単位、全体で 1 つ",
    trigger:
      "内側の層で救えなかった場合：worker が model_error で終了したとき。内側のリトライをすべて使い切ったか、ストリームが出力を渡し始めたあとに切れた場合です（その時点では再実行が安全でないため、全体をやり直すしかありません）。",
    skips:
      "クォータ切れはローテーションによる設定切り替えで処理済みのため、ここでは再実行しません。タスクが一時停止 / 終了 / ラップアップに入った場合はすぐに譲り、バックオフ時間を消費しません。",
    desc: "インテント全体を最初からもう一度実行します。最も外側の層なので、1 回の再実行で内側の各層の回数がもう一度掛け合わされます。",
    attemptsLabel: "再実行回数",
    defAttempts: 2,
    defInterval: "固定 3s",
    offHint: "-1 = 再実行せず、そのインテントを blocked と判定する",
  },
} satisfies Record<string, LayerMeta>;

type LayerKey = keyof typeof RETRY_LAYERS;

/** ミリ秒を人が読める言い方にする。入力欄の横に出すだけで、ゼロを数えさせない。 */
function humanMs(ms: number) {
  if (!Number.isFinite(ms) || ms <= 0) return "";
  if (ms < 1000) return `${ms}ms`;
  if (ms < 60_000) return `${Number((ms / 1000).toFixed(2))}s`;
  return `${Number((ms / 60_000).toFixed(2))}min`;
}

/** 制御された数値入力。空文字と 0 を対応させる。途中の状態（"-"、"1e"）はそのままローカルに残し、親を邪魔しない。 */
function NumField({
  id,
  value,
  onChange,
  placeholder,
  min,
}: {
  id: string;
  value: number;
  onChange: (n: number) => void;
  placeholder: string;
  min: number;
}) {
  const [text, setText] = React.useState(value === 0 ? "" : String(value));
  // 親が一式の値を変えたとき（方針を読んだ、設定を切り替えた）追従する。自分で打っているときはここへ来ない。
  // そのときは value がすでにローカルテキストを parse した結果と同じだから。
  React.useEffect(() => {
    const incoming = value === 0 ? "" : String(value);
    setText((cur) => (Number(cur || 0) === value ? cur : incoming));
  }, [value]);
  return (
    <Input
      id={id}
      type="number"
      min={min}
      className="w-28 shrink-0"
      value={text}
      placeholder={placeholder}
      onChange={(e) => {
        setText(e.target.value);
        const n = Number(e.target.value);
        onChange(e.target.value.trim() === "" || !Number.isFinite(n) ? 0 : Math.trunc(n));
      }}
    />
  );
}

/** 1層の再試行の2つのつまみ。idPrefix は同じページに複数出るとき、label の htmlFor を保つ。 */
export function RetryRuleFields({
  layer,
  idPrefix,
  value,
  onChange,
  compact,
}: {
  layer: LayerKey;
  idPrefix: string;
  value: LLMRetryRule;
  onChange: (r: LLMRetryRule) => void;
  /** true は設定ドロワーの詰め版。展開の説明を省き、「どんなエラーがこの層へ来るか」の1文だけ残す */
  compact?: boolean;
}) {
  const meta = RETRY_LAYERS[layer];
  const human = humanMs(value.interval_ms);
  return (
    <div className={compact ? "grid gap-2" : "grid gap-3 rounded-lg border p-3"}>
      <div className="grid gap-0.5">
        <div className="flex flex-wrap items-baseline gap-2">
          <Label className="text-sm">{meta.title}</Label>
          <span className="text-muted-foreground text-xs">{meta.where}</span>
        </div>
        {/* どのエラーがこの層へ来るか。ステータスコードまで。つまみを入れても効果が見えないときは、そのエラーがそもそもこの層に来ていないことが多い。 */}
        <p className="text-muted-foreground text-xs">
          <span className="font-medium text-foreground">トリガー</span>：{meta.trigger}
        </p>
        {!compact && meta.skips && (
          <p className="text-muted-foreground text-xs">
            <span className="font-medium text-foreground">この層を通りません</span>：{meta.skips}
          </p>
        )}
        {!compact && <p className="text-muted-foreground text-xs">{meta.desc}</p>}
      </div>
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
        <div className="flex items-center gap-2">
          <Label htmlFor={`${idPrefix}-${layer}-n`} className="text-muted-foreground text-xs">
            {meta.attemptsLabel}
          </Label>
          <NumField
            id={`${idPrefix}-${layer}-n`}
            min={-1}
            value={value.attempts}
            placeholder={`デフォルト ${meta.defAttempts}`}
            onChange={(n) => onChange({ ...value, attempts: n })}
          />
        </div>
        <div className="flex items-center gap-2">
          <Label htmlFor={`${idPrefix}-${layer}-ms`} className="text-muted-foreground text-xs">
            間隔 ms
          </Label>
          <NumField
            id={`${idPrefix}-${layer}-ms`}
            min={0}
            value={value.interval_ms}
            placeholder="デフォルトのバックオフ"
            onChange={(n) => onChange({ ...value, interval_ms: n })}
          />
          <span className="text-muted-foreground text-xs">{human ? `固定 ${human}` : meta.defInterval}</span>
        </div>
      </div>
      {!compact && <p className="text-muted-foreground text-xs">空欄 = デフォルトを使用。{meta.offHint}。</p>}
    </div>
  );
}

/** モデル設定ドロワーの 3 段の上書き（エンドポイントに従う 3 段）。 */
export function ProfileRetryFields({
  value,
  onChange,
}: {
  value: LLMRetryOverride;
  onChange: (o: LLMRetryOverride) => void;
}) {
  return (
    <div className="grid gap-3 rounded-lg border p-3">
      <div className="grid gap-0.5">
        <Label className="text-sm">リトライの上書き</Label>
        <p className="text-muted-foreground text-xs">
          この設定にだけ有効で、「リトライとバックオフ」のグローバルなデフォルトを上書きします。各欄を空欄にするとグローバルに従い、回数を
          -1 にするとその層のリトライを無効にします。
          間隔を入力すると、指数バックオフの代わりに固定間隔を使います。遮断とインテント再実行はプロセス単位のため、グローバルのページでのみ調整できます。
        </p>
      </div>
      {(["connect", "empty", "stream"] as const).map((k) => (
        <div key={k} className="border-t pt-3 first:border-t-0 first:pt-0">
          <RetryRuleFields
            compact
            layer={k}
            idPrefix="pf"
            value={value[k]}
            onChange={(r) => onChange({ ...value, [k]: r })}
          />
        </div>
      ))}
    </div>
  );
}

/** 「再試行とバックオフ」タブ。5層のグローバル既定値。 */
export function RetryPolicyPanel() {
  const [policy, setPolicy] = React.useState<LLMRetryPolicy>(ZERO_POLICY);
  const [loading, setLoading] = React.useState(true);
  const [saving, setSaving] = React.useState(false);

  const load = React.useCallback(async () => {
    setLoading(true);
    try {
      const p = await api.llmRetryPolicy();
      setPolicy({ ...ZERO_POLICY, ...p });
    } catch (e) {
      toast.error(`リトライポリシーの読み込みに失敗しました：${(e as Error).message}`);
    } finally {
      setLoading(false);
    }
  }, []);

  React.useEffect(() => {
    void load();
  }, [load]);

  async function save() {
    if (saving) return;
    setSaving(true);
    try {
      // バックエンドは範囲外の値を区間へ挟んで返し、返した値でそのまま更新する。見えているものが保存されたもの。
      const saved = await api.saveLLMRetryPolicy(policy);
      setPolicy({ ...ZERO_POLICY, ...saved });
      toast.success("保存しました。すぐに有効になります（実行中の呼び出しは古いパラメータのままです）");
    } catch (e) {
      toast.error(`保存に失敗しました：${(e as Error).message}`);
    } finally {
      setSaving(false);
    }
  }

  const set = (k: LayerKey) => (r: LLMRetryRule) => setPolicy((p) => ({ ...p, [k]: r }));

  if (loading) {
    return (
      <div className="flex items-center gap-2 rounded-lg border border-dashed p-10 text-muted-foreground text-sm">
        <Loader2Icon className="size-4 animate-spin" /> リトライポリシーを読み込み中…
      </div>
    );
  }

  return (
    <div className="grid gap-4">
      <div className="rounded-lg border bg-muted/30 p-3 text-muted-foreground text-xs leading-relaxed">
        1 回のモデル呼び出しの失敗は、内側から外側へ 5 層のリトライを順に通ります：
        <span className="text-foreground">
          {" "}
          接続確立 → 空レスポンス → 同一 provider の安全ウィンドウ → ローテーション遮断 → インテント再実行
        </span>
        。内側を使い切ってから外側に進むため、回数は
        <span className="text-foreground">掛け算</span>
        になります。各層を上限まで上げると、1 回の揺らぎで数十回のリクエストを消費します。
        すべて空欄なら現在のデフォルト値になり、このページがない場合とまったく同じ動作です。内側の 3
        層は、モデル設定ごとに個別に上書きできます。
      </div>

      <div className="grid gap-3 md:grid-cols-2 xl:grid-cols-3">
        {(Object.keys(RETRY_LAYERS) as LayerKey[]).map((k) => (
          <RetryRuleFields key={k} layer={k} idPrefix="gl" value={policy[k]} onChange={set(k)} />
        ))}
      </div>

      <div className="flex gap-2">
        <Button onClick={save} disabled={saving}>
          {saving ? <Loader2Icon className="animate-spin" /> : <SaveIcon />}
          保存
        </Button>
        <Button variant="outline" onClick={() => setPolicy(ZERO_POLICY)} disabled={saving}>
          すべてデフォルトに戻す
        </Button>
      </div>
    </div>
  );
}
