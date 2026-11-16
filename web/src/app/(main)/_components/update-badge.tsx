"use client";

import * as React from "react";

import Link from "next/link";

import { ArrowUpCircleIcon } from "lucide-react";

import { api } from "@/lib/api";

/**
 * トップバーの「新しいバージョンがある」表示。ページ全体の読み込み時に1回調べ、更新があればバージョン番号の横に出す。
 * クリックするとシステム設定ページの「バージョンと更新」カードへ直行する。
 *
 * バックエンドの GitHub 照会結果は 30 分キャッシュされる。だからここでマウントのたびに調べても安全である。
 * 未認証の GitHub API は 60 回/時/IP しかない。そのキャッシュが無いと、タブを複数開くだけで
 * 枠を使い切り、そのあと本当に更新したくても調べられなくなる。
 *
 * 照会の失敗はすべて黙る。トップバーはエラーを出す場所ではない。ユーザーが設定ページで「更新を確認」を押すと理由が見える。
 */
export function UpdateBadge() {
  const [latest, setLatest] = React.useState("");

  React.useEffect(() => {
    let alive = true;
    api
      .checkUpdate()
      .then((r) => {
        // has_update はすでに「バージョン番号を比較できる」判定を含む。開発ビルドではこの表示は点かない。
        if (alive && r.has_update && r.latest) setLatest(r.latest.replace(/^v(?=\d)/, ""));
      })
      .catch(() => {
        // 黙る。圏外でも GitHub のレート制限でも、トップバーにエラーを出さない。
      });
    return () => {
      alive = false;
    };
  }, []);

  if (!latest) return null;

  return (
    <Link
      href="/system/settings"
      title={`新しいバージョン ${latest} があります。クリックして更新へ進みます`}
      className="inline-flex items-center gap-1.5 rounded-full bg-primary px-2.5 py-1 font-medium text-primary-foreground text-xs transition-opacity hover:opacity-90"
    >
      {/* 呼吸する点。トップバーの要素は多く、文字だけだと見落としやすい。動きで一目で分かるようにする。 */}
      <span className="relative flex size-1.5">
        <span className="absolute inline-flex size-full animate-ping rounded-full bg-primary-foreground opacity-75" />
        <span className="relative inline-flex size-1.5 rounded-full bg-primary-foreground" />
      </span>
      <ArrowUpCircleIcon className="size-3.5" />
      <span className="hidden sm:inline">新バージョン {latest}</span>
      <span className="sm:hidden">新バージョン</span>
    </Link>
  );
}
