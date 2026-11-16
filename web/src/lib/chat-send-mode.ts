"use client";

import * as React from "react";

import { getLocalStorageValue, setLocalStorageValue } from "@/lib/local-storage.client";

// セッション入力欄の送信と改行のキー。フロントだけの好みで、localStorage にだけ置き、DB には入れず、アカウント同期もしない。
// そのためブラウザを変えると設定し直す。issue #39 を参照。0.3.2 は Ctrl+Enter 送信を Enter 送信に変えた。
// ここでは旧キーを選択肢として戻している。
export type ChatSendMode = "enter" | "ctrl-enter";

export const CHAT_SEND_MODE_KEY = "artex_chat_send_mode";
export const DEFAULT_CHAT_SEND_MODE: ChatSendMode = "enter";

export const CHAT_SEND_MODE_OPTIONS: { value: ChatSendMode; label: string }[] = [
  { value: "enter", label: "Enter で送信、Shift+Enter で改行" },
  { value: "ctrl-enter", label: "Ctrl+Enter で送信、Enter で改行" },
];

function parseMode(raw: string | null): ChatSendMode {
  return raw === "ctrl-enter" || raw === "enter" ? raw : DEFAULT_CHAT_SEND_MODE;
}

// 同じタブ内の購読者の集合。localStorage の storage イベントは「他の」タブでしか発火しない。
// このページで設定を変えたあとは emit で同じページの入力欄に知らせる。そうしないと再読み込みするまで効かない。
const listeners = new Set<() => void>();

function subscribe(listener: () => void) {
  listeners.add(listener);
  window.addEventListener("storage", listener);
  return () => {
    listeners.delete(listener);
    window.removeEventListener("storage", listener);
  };
}

// 返すのは文字列リテラルである。Object.is は値で比較するので、useSyncExternalStore がループしない。
function getSnapshot(): ChatSendMode {
  return parseMode(getLocalStorageValue(CHAT_SEND_MODE_KEY));
}

// サーバーには localStorage がない。まず既定値を描画し、hydrate 後に getSnapshot が直す。
function getServerSnapshot(): ChatSendMode {
  return DEFAULT_CHAT_SEND_MODE;
}

export function useChatSendMode(): ChatSendMode {
  return React.useSyncExternalStore(subscribe, getSnapshot, getServerSnapshot);
}

export function setChatSendMode(mode: ChatSendMode) {
  setLocalStorageValue(CHAT_SEND_MODE_KEY, mode);
  for (const listener of listeners) listener();
}

// shouldSubmitOnKey は、そのキー入力で送信すべきかを判定する。
// isComposing / keyCode 229 は中国語などの入力メソッドが変換中であることを示す。通す必要がある。通さないと、変換確定の Enter が誤送信になる。
// enter モードは Shift だけを除く。0.3.2 の挙動と一文字ずつ同じにする。設定を変えないユーザーの操作感は変えない。
// ctrl-enter モードは Ctrl と Cmd（macOS）の両方を受け付ける。
export function shouldSubmitOnKey(e: React.KeyboardEvent, mode: ChatSendMode): boolean {
  if (e.key !== "Enter") return false;
  if (e.nativeEvent.isComposing || e.nativeEvent.keyCode === 229) return false;
  if (mode === "ctrl-enter") return e.ctrlKey || e.metaKey;
  return !e.shiftKey;
}
