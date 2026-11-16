"use client";

import * as React from "react";

import {
  CheckCircle2Icon,
  DownloadIcon,
  ExternalLinkIcon,
  RefreshCwIcon,
  RotateCcwIcon,
  TriangleAlertIcon,
} from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Progress } from "@/components/ui/progress";
import { api, sseUrl } from "@/lib/api";
import type { UpdateCheck, UpdateProgress } from "@/lib/types";

/** 新しいバージョンが上がるまで待つ最長時間。1回のアップデートはプロセス起動が3回ある（一時保存、入れ替え、新版）。
 *  それぞれ秒単位なので、3分あれば遅いディスクと Docker コンテナの再作成をカバーできる。 */
const RESTART_TIMEOUT_MS = 180_000;

function humanSize(n?: number): string {
  if (!n || n <= 0) return "";
  const units = ["B", "KB", "MB", "GB"];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

export function UpdateCard() {
  const [info, setInfo] = React.useState<UpdateCheck | null>(null);
  const [checking, setChecking] = React.useState(true);
  const [progress, setProgress] = React.useState<UpdateProgress | null>(null);
  // progress とは分ける。一時保存が終わるとプロセスが無くなり、SSE は切れる。このときは /api/health のポーリングへ切り替える。
  const [restarting, setRestarting] = React.useState(false);
  const [busy, setBusy] = React.useState(false);

  // quiet はバックエンドのキャッシュを迂回するかも決める。ページを開いたときの自動確認はキャッシュを使う（トップバーが直前に調べている）。
  // ユーザーが「更新を確認」を押したときは強制的に取り直す。そうしないと、公開したばかりのバージョンはキャッシュ期限まで見えない。
  const check = React.useCallback((quiet = false) => {
    setChecking(true);
    api
      .checkUpdate(!quiet)
      .then((r) => {
        setInfo(r);
        if (!quiet) {
          if (r.error) toast.error("更新の確認に失敗しました：" + r.error);
          else if (r.has_update) toast.success(`新しいバージョン ${r.latest} があります`);
          else if (r.comparable) toast.success("すでに最新バージョンです");
        }
      })
      .catch((e) => {
        if (!quiet) toast.error("更新の確認に失敗しました：" + (e as Error).message);
      })
      .finally(() => setChecking(false));
  }, []);

  React.useEffect(() => {
    check(true);
  }, [check]);

  // バージョン番号が変わるまで /api/health をポーリングする。
  //
  // 判定は「つながった」ではなく「バージョンが変わった」でなければならない。入れ替えの途中で旧バージョンが短時間もう一度起動する
  // （その1回は artex.new を置き換えてすぐ終了するだけ）。接続だけを見ると成功と誤判定する。
  const waitForNewVersion = React.useCallback(async (fromVersion: string) => {
    setRestarting(true);
    const deadline = Date.now() + RESTART_TIMEOUT_MS;
    while (Date.now() < deadline) {
      await sleep(2000);
      try {
        const r = await fetch("/api/health", { cache: "no-store" });
        if (r.ok) {
          const j = (await r.json()) as { version?: string };
          if (j.version && j.version !== fromVersion) {
            toast.success(`${j.version} に更新しました。ページを再読み込みしています`);
            await sleep(800);
            window.location.reload();
            return;
          }
        }
      } catch {
        // 再起動の窓のあいだつながらないのは想定どおり。ポーリングを続ける。
      }
    }
    setRestarting(false);
    toast.error(
      "サービスの再起動待ちがタイムアウトしました。バックエンドのログを確認するか、artex を start.sh / start.bat で起動しているか確認してください。",
    );
  }, []);

  // 更新の進捗を購読する。SSE は Next の /api リライトを通さない（その層はバッファし、イベントを押し出せない）。
  const openStream = React.useCallback(
    (fromVersion: string) => {
      const es = new EventSource(sseUrl("/api/update/stream"));
      es.onmessage = (ev) => {
        let p: UpdateProgress;
        try {
          p = JSON.parse(ev.data) as UpdateProgress;
        } catch {
          return;
        }
        setProgress(p);
        if (p.phase === "failed") {
          es.close();
          setBusy(false);
          toast.error("更新に失敗しました：" + (p.error || p.message));
          return;
        }
        if (p.phase === "staged") {
          es.close();
          void waitForNewVersion(fromVersion);
        }
      };
      es.onerror = () => {
        // プロセス終了時に SSE は必ず切れる。すでに再起動待ちに入っているなら、これは正常である。
        // /api/health のポーリングに判定を続ければよい。
        es.close();
      };
      return es;
    },
    [waitForNewVersion],
  );

  const doUpdate = () => {
    if (!info) return;
    const from = info.current;
    const ok = window.confirm(
      `${info.latest} に更新しますか？\n\n` +
        "更新するとプログラムが再起動し、実行中のタスクは中断されます。\n" +
        (info.mode === "docker"
          ? "\n注意：コンテナ内の更新はプログラム本体だけを置き換え、イメージ内の playwright / nmap などのツールチェーンは更新しません。" +
            "新しいバージョンが新しいツールに依存する場合は、docker compose pull を使ってください。"
          : ""),
    );
    if (!ok) return;

    setBusy(true);
    setProgress({ phase: "downloading", percent: 0, message: "準備中…" });
    const es = openStream(from);
    api.applyUpdate().catch((e) => {
      es.close();
      setBusy(false);
      setProgress(null);
      toast.error("更新の開始に失敗しました：" + (e as Error).message);
    });
  };

  const doRollback = () => {
    if (!info) return;
    if (
      !window.confirm(
        "前のバージョンにロールバックしますか？\n\nプログラムが再起動し、実行中のタスクは中断されます。\n注意：データベースの構造は元に戻りません。古いバージョンは、新しいバージョンが書き込んだデータを認識できない可能性があります。",
      )
    )
      return;
    const from = info.current;
    setBusy(true);
    api
      .rollbackUpdate()
      .then(() => {
        toast.success("前のバージョンに切り替えました。再起動中…");
        void waitForNewVersion(from);
      })
      .catch((e) => {
        setBusy(false);
        toast.error("ロールバックに失敗しました：" + (e as Error).message);
      });
  };

  const phase = progress?.phase;
  const showProgress = busy || restarting;
  // 本当のパーセントが取れるのはダウンロード段階だけである（Content-Length で計算する）。検証、展開、再起動待ちは
  // 所要時間が分からない段階なので、進捗バーを満たしてパルスアニメーションを付け、「作業中だが残り時間は分からない」と示す。
  const downloading = !restarting && phase === "downloading";
  const pct = downloading ? Math.max(progress?.percent ?? 0, 0) : 100;

  return (
    // 設定ページは複数カラムの滝型レイアウトである。カード自身が行間隔を持ち、カラムをまたいで分断されない（page.tsx のコメントを参照）。
    <Card className="mb-4 break-inside-avoid md:mb-6">
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-base">
          <DownloadIcon className="size-4" />
          バージョンと更新
        </CardTitle>
        <CardDescription>
          GitHub
          から新しいバージョンを確認してインストールします。更新するとプログラムが再起動し、実行中のタスクは中断されます。
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="flex flex-wrap items-center gap-2 text-sm">
          <span className="text-muted-foreground">現在のバージョン</span>
          <Badge variant="secondary" className="font-mono">
            {info?.current ?? "…"}
          </Badge>
          {info && (
            <>
              <Badge variant="outline" className="font-mono">
                {info.os}/{info.arch}
              </Badge>
              <Badge variant="outline">{info.mode === "docker" ? "Docker" : "単体プログラム"}</Badge>
            </>
          )}
          {info?.latest && (
            <>
              <span className="text-muted-foreground">最新バージョン</span>
              <Badge variant={info.has_update ? "default" : "secondary"} className="font-mono">
                {info.latest}
              </Badge>
            </>
          )}
          {info?.html_url && (
            <a
              href={info.html_url}
              target="_blank"
              rel="noreferrer"
              className="inline-flex items-center gap-1 text-xs text-muted-foreground underline-offset-4 hover:underline"
            >
              更新履歴 <ExternalLinkIcon className="size-3" />
            </a>
          )}
        </div>

        {info?.boot_notice && (
          <p className="flex items-start gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 p-2 text-xs text-amber-700 dark:text-amber-400">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            {info.boot_notice}
          </p>
        )}

        {info?.error && (
          <p className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/10 p-2 text-xs text-destructive">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            GitHub に接続できません：{info.error}
            {"　"}上でグローバルプロキシを設定してから再試行できます。
          </p>
        )}

        {info && !info.comparable && info.reason && <p className="text-xs text-muted-foreground">{info.reason}</p>}

        {info?.has_update && info.asset_available === false && (
          <p className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/10 p-2 text-xs text-destructive">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            {info.latest} には {info.os}/{info.arch} 向けのリリースパッケージがないため（{info.asset}{" "}
            がありません）、自動更新できません。
          </p>
        )}

        {info?.has_update && info.asset_available !== false && (
          <p className="text-xs text-muted-foreground">
            <span className="font-mono">{info.asset}</span>
            {info.size ? `（${humanSize(info.size)}）` : ""}をダウンロードし、SHA256
            の検証とスモークテストに通った場合だけ置き換えます。失敗した場合は現在のバージョンを自動で保持します。
          </p>
        )}

        {info && !info.has_update && info.comparable && !info.error && (
          <p className="flex items-center gap-2 text-xs text-muted-foreground">
            <CheckCircle2Icon className="size-3.5 text-emerald-600" />
            すでに最新バージョンです。
          </p>
        )}

        {info?.mode === "docker" && info.has_update && (
          <p className="text-xs text-muted-foreground">
            Docker での更新はプログラム本体だけを置き換え、イメージ内の playwright / nmap
            などのツールチェーンは更新しません。また、
            <span className="font-mono"> docker compose up -d </span>
            でコンテナを再作成すると、イメージに含まれるバージョンに戻ります。イメージごと更新するには次を実行してください：
            <span className="font-mono"> docker compose pull artex &amp;&amp; docker compose up -d artex</span>。
          </p>
        )}

        {showProgress && (
          <div className="space-y-1.5">
            <Progress value={pct} className={downloading ? undefined : "animate-pulse"} />
            <p className="text-xs text-muted-foreground">
              {restarting
                ? "再起動して新しいバージョンを適用中です。しばらくお待ちください（ページは自動で更新されます）…"
                : progress?.message}
            </p>
          </div>
        )}

        <div className="flex flex-wrap gap-2">
          <Button variant="outline" size="sm" onClick={() => check(false)} disabled={checking || busy || restarting}>
            <RefreshCwIcon className={checking ? "size-4 animate-spin" : "size-4"} />
            更新を確認
          </Button>
          <Button
            size="sm"
            onClick={doUpdate}
            disabled={busy || restarting || !info?.has_update || info?.asset_available === false}
          >
            <DownloadIcon className="size-4" />
            {info?.has_update ? `${info.latest} に更新` : "今すぐ更新"}
          </Button>
          {info?.has_backup && (
            <Button variant="ghost" size="sm" onClick={doRollback} disabled={busy || restarting}>
              <RotateCcwIcon className="size-4" />
              前のバージョンにロールバック
            </Button>
          )}
        </div>

        <p className="text-xs text-muted-foreground">
          ワンクリック更新は、監視スクリプトによるプログラムの再起動に依存します。
          <span className="font-mono">start.sh</span>（Windows は<span className="font-mono"> start.bat</span>）から
          ARTEX を起動してください。artex 本体を直接実行した場合、プログラムの終了後に自動で再起動されません。
        </p>
      </CardContent>
    </Card>
  );
}
