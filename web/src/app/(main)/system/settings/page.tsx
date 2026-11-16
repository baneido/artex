"use client";

import * as React from "react";

import { CpuIcon, FlaskConicalIcon, KeyboardIcon, RadioTowerIcon, SearchIcon, ShieldAlertIcon } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { api } from "@/lib/api";
import { CHAT_SEND_MODE_OPTIONS, type ChatSendMode, setChatSendMode, useChatSendMode } from "@/lib/chat-send-mode";
import type { Settings } from "@/lib/types";

import { UpdateCard } from "./_components/update-card";

export default function SystemSettingsPage() {
  const [trafficCapture, setTrafficCapture] = React.useState(false);
  const [agentTrafficBinding, setAgentTrafficBinding] = React.useState(false);
  const [webSearch, setWebSearch] = React.useState(false);
  const [backend, setBackend] = React.useState("ddgs");
  const [braveKeySet, setBraveKeySet] = React.useState(false);
  const [braveKeyInput, setBraveKeyInput] = React.useState("");
  const [tavilyKeySet, setTavilyKeySet] = React.useState(false);
  const [tavilyKeyInput, setTavilyKeyInput] = React.useState("");
  const [savingTavilyKey, setSavingTavilyKey] = React.useState(false);
  const [proxyInput, setProxyInput] = React.useState("");
  const [savingProxy, setSavingProxy] = React.useState(false);
  const [globalProxyInput, setGlobalProxyInput] = React.useState("");
  const [savingGlobalProxy, setSavingGlobalProxy] = React.useState(false);
  const [testing, setTesting] = React.useState(false);
  const [loaded, setLoaded] = React.useState(false);
  const [saving, setSaving] = React.useState(false);
  const [savingKey, setSavingKey] = React.useState(false);
  const [pyInterp, setPyInterp] = React.useState("");
  const [workers, setWorkers] = React.useState("3");
  const [savingWorkers, setSavingWorkers] = React.useState(false);
  // 操作制約を注入する範囲（既定はすべてオン）。
  const [injectPlanner, setInjectPlanner] = React.useState(true);
  const [injectWorker, setInjectWorker] = React.useState(true);
  // 実験機能。noa のコンテキスト圧縮（既定はオフ）。
  const [noaCompaction, setNoaCompaction] = React.useState(false);
  // フロントだけの好み。/api/settings は通さず、localStorage を直接読み書きする。
  const sendMode = useChatSendMode();

  const apply = React.useCallback((s: Settings) => {
    setTrafficCapture(!!s.traffic_capture);
    setAgentTrafficBinding(!!s.agent_traffic_binding);
    setWebSearch(!!s.web_search_enabled);
    setBackend(s.web_search_backend || "ddgs");
    setBraveKeySet(!!s.brave_key_set);
    setTavilyKeySet(!!s.tavily_key_set);
    setProxyInput(s.web_search_proxy ?? "");
    setGlobalProxyInput(s.global_proxy ?? "");
    setPyInterp(s.python_interpreter ?? "");
    setWorkers(String(s.workers ?? 3));
    setInjectPlanner(s.constraints_inject_planner !== false);
    setInjectWorker(s.constraints_inject_worker !== false);
    setNoaCompaction(!!s.noa_compaction);
  }, []);

  const saveWorkers = () => {
    const n = Number(workers);
    if (!Number.isInteger(n) || n <= 0) {
      toast.error("同時実行数は 0 より大きい整数で入力してください");
      return;
    }
    setSavingWorkers(true);
    api
      .setSettings({ workers: n })
      .then((s) => {
        apply(s);
        toast.success("同時実行する作業 agent の数を保存しました（以降に開始するタスクから有効）");
      })
      .catch((e) => toast.error("保存に失敗しました：" + (e as Error).message))
      .finally(() => setSavingWorkers(false));
  };

  const savePython = () => {
    setSaving(true);
    api
      .setSettings({ python_interpreter: pyInterp.trim() })
      .then((s) => {
        apply(s);
        toast.success("Python インタープリターの設定を保存しました");
      })
      .catch((e) => toast.error("保存に失敗しました：" + (e as Error).message))
      .finally(() => setSaving(false));
  };
  const detectPython = () => {
    setSaving(true);
    api
      .detectPython()
      .then((r) => setPyInterp(r.python_interpreter))
      .catch(() => undefined)
      .finally(() => setSaving(false));
  };

  React.useEffect(() => {
    api
      .settings()
      .then(apply)
      .catch(() => undefined)
      .finally(() => setLoaded(true));
  }, [apply]);

  const toggleTraffic = (v: boolean) => {
    setTrafficCapture(v); // optimistic
    setSaving(true);
    api
      .setSettings({ traffic_capture: v })
      .then(apply)
      .catch(() => setTrafficCapture(!v)) // revert on failure
      .finally(() => setSaving(false));
  };

  const toggleInjectPlanner = (v: boolean) => {
    setInjectPlanner(v); // optimistic
    api
      .setSettings({ constraints_inject_planner: v })
      .then(apply)
      .catch(() => setInjectPlanner(!v)); // revert on failure
  };

  const toggleAgentTrafficBinding = (v: boolean) => {
    setAgentTrafficBinding(v);
    setSaving(true);
    api
      .setSettings({ agent_traffic_binding: v })
      .then((s) => {
        apply(s);
        toast.success(v ? "Agent によるトラフィックの自動紐付けを有効にしました" : "Agent によるトラフィックの自動紐付けを無効にしました");
      })
      .catch((e) => {
        setAgentTrafficBinding(!v);
        toast.error(`保存に失敗しました：${(e as Error).message}`);
      })
      .finally(() => setSaving(false));
  };

  const toggleInjectWorker = (v: boolean) => {
    setInjectWorker(v); // optimistic
    api
      .setSettings({ constraints_inject_worker: v })
      .then(apply)
      .catch(() => setInjectWorker(!v)); // revert on failure
  };

  const toggleNoaCompaction = (v: boolean) => {
    setNoaCompaction(v); // optimistic
    api
      .setSettings({ noa_compaction: v })
      .then((s) => {
        apply(s);
        toast.success(v ? "noa コンテキスト圧縮を有効にしました（以降に開始する実行から有効）" : "noa コンテキスト圧縮を無効にしました（組み込みの圧縮に戻ります）");
      })
      .catch((e) => {
        setNoaCompaction(!v); // revert on failure
        toast.error(`保存に失敗しました：${(e as Error).message}`);
      });
  };

  // Persist a web-search patch (enable and/or backend). Optimistic with refetch.
  const saveWebSearch = (patch: Partial<Settings>) => {
    setSaving(true);
    api
      .setSettings(patch)
      .then((s) => {
        apply(s);
        toast.success("Web 検索の設定を保存しました");
      })
      .catch((e) => {
        toast.error("保存に失敗しました：" + (e as Error).message);
        api
          .settings()
          .then(apply)
          .catch(() => undefined);
      })
      .finally(() => setSaving(false));
  };

  const saveBraveKey = () => {
    setSavingKey(true);
    api
      .setSettings({ brave_search_api_key: braveKeyInput })
      .then((s) => {
        apply(s);
        setBraveKeyInput("");
        toast.success("Brave API Key を保存しました");
      })
      .catch((e) => toast.error("保存に失敗しました：" + (e as Error).message))
      .finally(() => setSavingKey(false));
  };

  const saveTavilyKey = () => {
    setSavingTavilyKey(true);
    api
      .setSettings({ tavily_search_api_key: tavilyKeyInput })
      .then((s) => {
        apply(s);
        setTavilyKeyInput("");
        toast.success("Tavily API Key を保存しました");
      })
      .catch((e) => toast.error("保存に失敗しました：" + (e as Error).message))
      .finally(() => setSavingTavilyKey(false));
  };

  const saveProxy = () => {
    setSavingProxy(true);
    api
      .setSettings({ web_search_proxy: proxyInput.trim() })
      .then((s) => {
        apply(s);
        toast.success(proxyInput.trim() ? "出口プロキシを保存しました" : "出口プロキシを解除しました（直接接続に変更）");
      })
      .catch((e) => toast.error("保存に失敗しました：" + (e as Error).message))
      .finally(() => setSavingProxy(false));
  };

  const saveGlobalProxy = () => {
    setSavingGlobalProxy(true);
    api
      .setSettings({ global_proxy: globalProxyInput.trim() })
      .then((s) => {
        apply(s);
        toast.success(globalProxyInput.trim() ? "グローバルプロキシを保存しました" : "グローバルプロキシを解除しました（直接接続に変更）");
      })
      .catch((e) => toast.error("保存に失敗しました：" + (e as Error).message))
      .finally(() => setSavingGlobalProxy(false));
  };

  // Run a real "test" search ("test") against the CURRENT form values (backend +
  // proxy + entered key), falling back to saved values server-side. Toasts result.
  const runTest = () => {
    setTesting(true);
    api
      .testWebSearch({
        web_search_backend: backend,
        web_search_proxy: proxyInput.trim(),
        brave_search_api_key: braveKeyInput,
        tavily_search_api_key: tavilyKeyInput,
      })
      .then((r) => {
        if (r.ok) toast.success(`検索テスト成功 · ${r.backend} が ${r.count} 件の結果を返しました`);
        else toast.error("検索テストに失敗しました：" + (r.error || "不明なエラー"));
      })
      .catch((e) => toast.error("検索テストに失敗しました：" + (e as Error).message))
      .finally(() => setTesting(false));
  };

  // brave-free selected but no key stored and none being entered → tool stays off.
  const braveNeedsKey = webSearch && backend === "brave-free" && !braveKeySet;

  return (
    <div className="flex flex-1 flex-col gap-4 md:gap-6">
      <div>
        <h1 className="text-xl font-semibold tracking-tight">システム設定</h1>
        <p className="text-muted-foreground text-sm">グローバルな実行時スイッチ</p>
      </div>

      {/* grid ではなく複数カラムにする。Web 検索カードは他より数倍高く、選んだバックエンドで高さが変わる（brave/tavily
          の key 入力は条件付きで描画する）。grid は一番高いカードで行全体を伸ばし、隣に大きな空白を残す。
          複数カラムなら内容の高さで自動的に埋める。カード間隔は gap ではなく mb にする。複数カラムでは
          column-gap は列間隔だけを決め、行間隔は子要素自身が付ける。 */}
      <div className="columns-1 gap-4 md:gap-6 lg:columns-2">
        <UpdateCard />

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <RadioTowerIcon className="size-4" />
              トラフィックキャプチャ
            </CardTitle>
            <CardDescription>
              有効にすると、すべての Agent の HTTP トラフィックを記録プロキシ経由で全量保存し、Agent に traffic_search / traffic_get
              ツールとプロキシ設定を注入します（プロンプトにプロキシの説明が入ります）。
              <br />
              無効（デフォルト）のときはトラフィックを一切記録しません。Agent はプロキシ設定とトラフィックツールを
              <b>受け取らず</b>、プロンプトにもプロキシ関連の内容は<b>含まれません</b>。切り替えるとすぐに Agent を再構築して反映します。
            </CardDescription>
          </CardHeader>
          <CardContent className="flex items-center justify-between gap-4">
            <Label htmlFor="traffic-capture" className="text-sm font-normal text-muted-foreground">
              {trafficCapture ? "有効 · トラフィックを記録し、プロキシを注入中" : "無効 · 記録せず、プロキシも注入しません"}
            </Label>
            <Switch
              id="traffic-capture"
              checked={trafficCapture}
              disabled={!loaded || saving}
              onCheckedChange={toggleTraffic}
            />
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <RadioTowerIcon className="size-4" />
              Agent によるトラフィックの自動紐付け
            </CardTitle>
            <CardDescription id="agent-traffic-binding-description">
              デフォルトは無効です。有効にすると、脆弱性の登録時に起動するレポート Agent が既存の HTTP リクエスト/レスポンスを照合し、対応するトラフィックを関連付けてからレポートを作成します。
              <b>パケットの参照と追加のツール呼び出しにより、Token 消費が増えます。</b>
              <br />
              TCP、未キャプチャ、一致するトラフィックがない場合でも、通常どおり報告できます。このスイッチは、トラフィックキャプチャ、手動での紐付け、保存済み証拠の表示には影響しません。 次のターンの Agent
              から有効になり、無効にすると新しい自動紐付けをすぐに拒否します。
            </CardDescription>
          </CardHeader>
          <CardContent className="flex items-center justify-between gap-4">
            <Label htmlFor="agent-traffic-binding" className="text-sm font-normal text-muted-foreground">
              {agentTrafficBinding ? "有効 · Token 消費が増えます" : "無効 · 手動での紐付けは引き続き可能"}
            </Label>
            <Switch
              id="agent-traffic-binding"
              aria-describedby="agent-traffic-binding-description"
              checked={agentTrafficBinding}
              disabled={!loaded || saving}
              onCheckedChange={toggleAgentTrafficBinding}
            />
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <RadioTowerIcon className="size-4" />
              グローバルプロキシ
            </CardTitle>
            <CardDescription>
              すべての Agent の<b>対象向けトラフィック</b>をこのプロキシ経由で外部に送信します（送信元 IP の隠蔽 / 踏み台経由）。<b>http / https / socks5</b> に対応し、{" "}
              <code>user:pass</code> 認証も付けられます。空欄=直接接続。
              <br />
              <b>トラフィックキャプチャ</b>が有効なときは、記録プロキシの<b>上流</b>として動作します（トラフィックは全量保存したうえで、このプロキシ経由で外部に送信します）。キャプチャが無効なときは、
              Agent の bash / WebFetch の外部通信に直接注入します。Web 検索プロキシや LLM プロキシとは独立しています。
              <br />
              <b>ヒント</b>：socks5 は<b>キャプチャが無効</b>のとき、各コマンドラインツールの <code>ALL_PROXY</code> 対応に依存します（curl
              は対応していますが、一部のツールは無視する可能性があります）。主に socks5 を使う場合は、トラフィックキャプチャの有効化を推奨します。この経路では MITM
              が自ら接続するため、ツール側の対応に関係なく安定して動作します。
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-2">
            <Label htmlFor="global-proxy" className="text-sm font-normal text-muted-foreground">
              プロキシアドレス
            </Label>
            <div className="flex items-center gap-2">
              <Input
                id="global-proxy"
                autoComplete="off"
                placeholder="socks5://user:pass@host:1080 または http://host:port（空欄=直接接続）"
                value={globalProxyInput}
                disabled={!loaded || savingGlobalProxy}
                onChange={(e) => setGlobalProxyInput(e.target.value)}
              />
              <Button type="button" onClick={saveGlobalProxy} disabled={!loaded || savingGlobalProxy}>
                保存
              </Button>
            </div>
            <p className="text-muted-foreground text-xs">
              {globalProxyInput.trim() ? "設定済み · 対象向けトラフィックはすべてこのプロキシ経由" : "未設定 · 対象向けトラフィックは直接接続"}
            </p>
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <ShieldAlertIcon className="size-4" />
              操作制約の注入
            </CardTitle>
            <CardDescription>
              有効にすると、各タスクの<b>操作制約</b>（タスク概要の「操作制約」で管理する allow/deny の項目）を対応する Agent
              のシステムプロンプトに組み込み、探索範囲を定めます（例「現在のポートだけをテスト」「ブルートフォース禁止」）。
              <br />
              注入先は <b>プランナー（planner）</b>と <b>実行者（worker）</b>
              で個別に切り替えられます。デフォルトはどちらも有効です。切り替えはすぐに反映され（次のターンで読み込み）、Agent の再構築は不要です。無効にすると、その Agent には制約が見えなくなります。
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-4">
            <div className="flex items-center justify-between gap-4">
              <Label htmlFor="inject-planner" className="text-sm font-normal text-muted-foreground">
                プランナー（planner）に注入{injectPlanner ? " · 有効" : " · 無効"}
              </Label>
              <Switch
                id="inject-planner"
                checked={injectPlanner}
                disabled={!loaded}
                onCheckedChange={toggleInjectPlanner}
              />
            </div>
            <div className="flex items-center justify-between gap-4">
              <Label htmlFor="inject-worker" className="text-sm font-normal text-muted-foreground">
                実行者（worker）に注入{injectWorker ? " · 有効" : " · 無効"}
              </Label>
              <Switch
                id="inject-worker"
                checked={injectWorker}
                disabled={!loaded}
                onCheckedChange={toggleInjectWorker}
              />
            </div>
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <FlaskConicalIcon className="size-4" />
              実験的機能
            </CardTitle>
            <CardDescription>
              検証中の仕組みで、デフォルトは無効です。Agent の動作を変えたり、安定性に影響したりする可能性があります。影響を理解したうえで有効にしてください。
              <br />
              <b>noa コンテキスト圧縮</b>：長いチャット履歴をモデルが自発的に圧縮します（norma v0.4.0）。有効にすると、プラットフォームに接続している 4 種類の Agent（
              <b>プランナー / 実行者 / メイン Agent / チャット</b>）は noa にコンテキストの管理を任せ、組み込みの圧縮を置き換えます。
              圧縮前の原文は、後から追跡できるようタスクの作業ディレクトリにアーカイブされます。切り替えはすぐに反映され（以降に開始する実行から有効）、Agent の再構築は不要です。
              無効にすると、すぐに組み込みの圧縮に戻ります。
            </CardDescription>
          </CardHeader>
          <CardContent className="flex items-center justify-between gap-4">
            <Label htmlFor="noa-compaction" className="text-sm font-normal text-muted-foreground">
              noa コンテキスト圧縮{noaCompaction ? " · 有効" : " · 無効"}
            </Label>
            <Switch
              id="noa-compaction"
              checked={noaCompaction}
              disabled={!loaded}
              onCheckedChange={toggleNoaCompaction}
            />
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <SearchIcon className="size-4" />
              Web 検索
            </CardTitle>
            <CardDescription>
              Web 検索の<b>全体スイッチ + 検索元の設定</b>です。有効にすると、<b>各 Agent の設定</b>で
              <b>web_search</b> を使うかどうかを個別に選べます（タイトル/リンク/概要だけを返し、本文は取得しません。取得は WebFetch が担当します）。Web 検索は記録プロキシを<b>経由せず</b>
              、トラフィックキャプチャとは独立しています。
              <br />
              検索元は <b>DuckDuckGo（ddgs）</b>（Key 不要）、<b>Brave（無料版）</b>（Brave API Key が必要）、{" "}
              <b>Tavily</b>（Tavily API Key が必要）、<b>DeepSeek</b>（現在の LLM 設定を再利用）から選べます。全体スイッチが無効のとき、各
              Agent の Web 検索スイッチは使用できません。
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-4">
            <div className="flex items-center justify-between gap-4">
              <Label htmlFor="web-search" className="text-sm font-normal text-muted-foreground">
                {webSearch ? "全体スイッチは有効 · 各 Agent の設定で個別に有効化できます" : "無効 · 各 Agent は Web 検索を有効にできません"}
              </Label>
              <Switch
                id="web-search"
                checked={webSearch}
                disabled={!loaded || saving}
                onCheckedChange={(v) => {
                  setWebSearch(v); // optimistic
                  saveWebSearch({ web_search_enabled: v });
                }}
              />
            </div>

            {webSearch && (
              <div className="flex items-center justify-between gap-4">
                <Label className="text-sm font-normal text-muted-foreground">検索元</Label>
                <Select
                  value={backend}
                  disabled={!loaded || saving}
                  onValueChange={(v) => {
                    setBackend(v); // optimistic
                    saveWebSearch({ web_search_backend: v });
                  }}
                >
                  <SelectTrigger className="w-48 shrink-0">
                    <SelectValue placeholder="検索元を選択" />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="ddgs">DuckDuckGo（ddgs · 無料、Key 不要）</SelectItem>
                    <SelectItem value="brave-free">Brave（無料版 · Key が必要）</SelectItem>
                    <SelectItem value="tavily">Tavily（Key が必要）</SelectItem>
                    <SelectItem value="deepseek">DeepSeek（公式）</SelectItem>
                  </SelectContent>
                </Select>
              </div>
            )}

            {webSearch && backend === "deepseek" && (
              <div className="border-border/60 bg-muted/30 flex flex-col gap-2 rounded-md border p-3">
                <p className="text-sm font-medium">DeepSeek 公式の Web 検索</p>
                <p className="text-muted-foreground text-xs leading-relaxed">
                  この検索元は<b>現在有効な LLM 設定</b>をそのまま再利用します。そのため
                  <b>DeepSeek 公式モデルにのみ対応</b>し、その設定は<b>anthropic プロトコルを使う必要があります</b>
                  。DeepSeek の OpenAI プロトコルのエンドポイントはサーバー側検索に対応していません。LLM 設定を切り替えると、この検索元は使えなくなる可能性があります。
                </p>
                <p className="text-muted-foreground text-xs leading-relaxed">
                  他の検索元と異なり、検索は <b>DeepSeek のサーバー側で実行</b>されます。検索のたびにモデル呼び出しを 1 回余分に消費します（Token
                  の費用が発生）。検索リクエストは<b>上の出口プロキシを経由せず</b>、<b>トラフィックの記録にも残りません</b>。返される結果は<b>タイトルとリンクだけ</b>
                  で（概要なし）、本文が必要なときは WebFetch で取得します。
                </p>
                <p className="text-muted-foreground text-xs leading-relaxed">
                  上記の条件を満たしているかどうかは自分で確認してください。システムはブロックしません。下の「検索をテスト」ボタンで実際に 1 回実行して確認できます。
                </p>
              </div>
            )}

            {webSearch && backend === "brave-free" && (
              <div className="flex flex-col gap-2">
                <Label htmlFor="brave-key" className="text-sm font-normal text-muted-foreground">
                  Brave Search API Key
                  {braveKeySet && <span className="ml-2 text-xs text-emerald-500">設定済み</span>}
                </Label>
                <div className="flex items-center gap-2">
                  <Input
                    id="brave-key"
                    type="password"
                    autoComplete="off"
                    placeholder={braveKeySet ? "設定済み（空欄の場合は変更しません）" : "Brave API Key を入力"}
                    value={braveKeyInput}
                    disabled={!loaded || savingKey}
                    onChange={(e) => setBraveKeyInput(e.target.value)}
                  />
                  <Button
                    type="button"
                    onClick={saveBraveKey}
                    disabled={!loaded || savingKey || braveKeyInput.trim() === ""}
                  >
                    保存
                  </Button>
                </div>
                {braveNeedsKey && (
                  <p className="text-xs text-amber-500">
                    Brave を選択していますが Key が未設定です —— Key を保存するまで検索ツールは有効になりません。
                  </p>
                )}
                <p className="text-muted-foreground text-xs">
                  無料版のクォータは約 2,000 回/月です。Key は https://brave.com/search/api/ で取得できます。
                </p>
              </div>
            )}

            {webSearch && backend === "tavily" && (
              <div className="flex flex-col gap-2">
                <Label htmlFor="tavily-key" className="text-sm font-normal text-muted-foreground">
                  Tavily Search API Key
                  {tavilyKeySet && <span className="ml-2 text-xs text-emerald-500">設定済み</span>}
                </Label>
                <div className="flex items-center gap-2">
                  <Input
                    id="tavily-key"
                    type="password"
                    autoComplete="off"
                    placeholder={tavilyKeySet ? "設定済み（空欄の場合は変更しません）" : "Tavily API Key を入力（tvly-…）"}
                    value={tavilyKeyInput}
                    disabled={!loaded || savingTavilyKey}
                    onChange={(e) => setTavilyKeyInput(e.target.value)}
                  />
                  <Button
                    type="button"
                    onClick={saveTavilyKey}
                    disabled={!loaded || savingTavilyKey || tavilyKeyInput.trim() === ""}
                  >
                    保存
                  </Button>
                </div>
                {webSearch && backend === "tavily" && !tavilyKeySet && (
                  <p className="text-xs text-amber-500">
                    Tavily を選択していますが Key が未設定です —— Key を保存するまで検索ツールは有効になりません。
                  </p>
                )}
                <p className="text-muted-foreground text-xs">https://tavily.com で登録して API Key を取得できます。</p>
              </div>
            )}

            {webSearch && (
              <div className="flex flex-col gap-2">
                <Label htmlFor="ws-proxy" className="text-sm font-normal text-muted-foreground">
                  出口プロキシ（任意）
                </Label>
                <div className="flex items-center gap-2">
                  <Input
                    id="ws-proxy"
                    autoComplete="off"
                    placeholder="http://host:port または socks5://host:port（空欄=直接接続）"
                    value={proxyInput}
                    disabled={!loaded || savingProxy}
                    onChange={(e) => setProxyInput(e.target.value)}
                  />
                  <Button type="button" onClick={saveProxy} disabled={!loaded || savingProxy}>
                    保存
                  </Button>
                </div>
                <p className="text-muted-foreground text-xs">
                  検索エンドポイントへのアクセスだけに使う独立した出口プロキシです（VPN/SOCKS など）。トラフィックを記録する MITM プロキシとは関係ありません。ネットワークがつながらないときに、このプロキシ経由でアクセスします。
                </p>
              </div>
            )}

            {webSearch && (
              <div className="flex items-center justify-between gap-4 border-t pt-4">
                <p className="text-muted-foreground text-xs">
                  現在の設定（検索元 + プロキシ + Key）で「test」を実際に 1 回検索し、使えるかどうかを確認します。
                </p>
                <Button
                  type="button"
                  variant="outline"
                  onClick={runTest}
                  disabled={!loaded || testing}
                  className="shrink-0"
                >
                  {testing ? "テスト中…" : "検索をテスト"}
                </Button>
              </div>
            )}
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <RadioTowerIcon className="size-4" />
              カスタムスクリプト · Python インタープリター
            </CardTitle>
            <CardDescription>
              カスタム <b>script</b> 種別のツールは、これを使って Python を実行します。起動時に自動検出します（python3 優先）。ここに venv や
              特定バージョンの絶対パスを手入力できます。空欄の場合は実行時に自動検出します。
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-3">
            <div className="flex items-center gap-2">
              <Input
                className="font-mono text-sm"
                placeholder="/usr/bin/python3（空欄=自動検出）"
                value={pyInterp}
                disabled={!loaded || saving}
                onChange={(e) => setPyInterp(e.target.value)}
              />
              <Button variant="outline" onClick={detectPython} disabled={!loaded || saving}>
                再検出
              </Button>
              <Button onClick={savePython} disabled={!loaded || saving}>
                保存
              </Button>
            </div>
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <CpuIcon className="size-4" />
              作業の同時実行 · Work Agent 数
            </CardTitle>
            <CardDescription>
              タスクごとに同時実行する作業 agent の数です（デフォルト 3）。値が大きいほど並行して探索する数が増え、消費も増えます。変更は
              <b>以降に開始するタスクから有効</b>になり、実行中のタスクには影響しません。
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-3">
            <div className="flex items-center gap-2">
              <Input
                type="number"
                min={1}
                className="w-32 font-mono text-sm"
                placeholder="3"
                value={workers}
                disabled={!loaded || savingWorkers}
                onChange={(e) => setWorkers(e.target.value)}
              />
              <Button onClick={saveWorkers} disabled={!loaded || savingWorkers}>
                保存
              </Button>
            </div>
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <KeyboardIcon className="size-4" />
              セッション入力欄の送信キー
            </CardTitle>
            <CardDescription>
              チャットページとタスク詳細のメイン Agent セッションの入力欄は、この設定を共有します。選択するとすぐに有効になり、保存は不要です。
              <br />
              この設定は<b>このブラウザーにだけ保存</b>され、アカウントとは同期されません。ブラウザーを変えたりサイトデータを消去したりした場合は、再設定が必要です。
            </CardDescription>
          </CardHeader>
          <CardContent className="flex items-center justify-between gap-4">
            <Label htmlFor="chat-send-mode" className="text-sm font-normal text-muted-foreground">
              送信方法
            </Label>
            <Select value={sendMode} onValueChange={(v) => setChatSendMode(v as ChatSendMode)}>
              <SelectTrigger id="chat-send-mode" className="w-72">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {CHAT_SEND_MODE_OPTIONS.map((option) => (
                  <SelectItem key={option.value} value={option.value}>
                    {option.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </CardContent>
        </Card>
      </div>
    </div>
  );
}
