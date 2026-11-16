"use client";

// LLM 重试配置的共用件：五层重试各自的「次数 + 间隔」。
//
// 五层从内到外：建连(SDK) → 空响应(SDK) → 同 provider 安全窗口 → 轮询熔断 → 意图重跑。
// 前三层跟着端点走，所以每个模型配置都能覆盖全局默认；后两层是进程级的，只有全局一份。
//
// 所有输入都遵循同一套「留空 = 不配置」语义，与后端 db.RetryRule 一致：
//   次数   空/0 = 用内置默认 | -1 = 关闭这层重试 | >0 = 用这个次数
//   间隔   空/0 = 用这层原本的指数退避 | >0 = 改用这个固定毫秒间隔

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
  /** 这层重试发生在哪、由谁执行 */
  where: string;
  /** 什么样的错误会走到这层——具体到状态码，别让人猜 */
  trigger: string;
  /** 长得像但【不】走这层的错误，省得填了没反应还以为是 bug */
  skips?: string;
  desc: string;
  attemptsLabel: string;
  /** 次数留空时的默认值，用于占位符 */
  defAttempts: number;
  /** 间隔留空时的默认策略，用于占位符 */
  defInterval: string;
  /** 次数填 -1 的含义 */
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

/** 毫秒的人话，只用于在输入框旁边回显，免得数零。 */
function humanMs(ms: number) {
  if (!Number.isFinite(ms) || ms <= 0) return "";
  if (ms < 1000) return `${ms}ms`;
  if (ms < 60_000) return `${Number((ms / 1000).toFixed(2))}s`;
  return `${Number((ms / 60_000).toFixed(2))}min`;
}

/** 受控数字输入：空串 ↔ 0，中间态（"-"、"1e"）原样留在本地，不打扰父级。 */
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
  // 父级换了一整套值（读取到策略、切换配置）时跟上；自己敲字时不会走到这里，
  // 因为那时 value 已经等于本地文本 parse 后的结果。
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

/** 一层重试的两个旋钮。idPrefix 用来在同一页出现多次时保住 label 的 htmlFor。 */
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
  /** true = 配置抽屉里的紧凑版：省掉展开说明，只留「什么错误会走到这层」这一句 */
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
        {/* 哪些错误会走到这层，具体到状态码——填了旋钮却看不到效果，多半是错误压根不落在这层。 */}
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

/** 模型配置抽屉里的三层覆盖（跟着端点走的那三层）。 */
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

/** 「重试与退避」tab：五层的全局默认值。 */
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
      // 后端会把越界值夹回区间并回传，直接用回传值刷新，所见即所存。
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
