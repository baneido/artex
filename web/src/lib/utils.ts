import { type ClassValue, clsx } from "clsx";
import { twMerge } from "tailwind-merge";

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

// ドロワーやダイアログ（Sheet/Dialog）の onInteractOutside で閉じるかどうかの補助。
//
// 背景。ドロワー内の Radix ポップアップ（Select のドロップダウン、DropdownMenu、Popover など）はドロワーの
// 外へ portal される。ポップアップが開いているときにマスクやドロワーの外をクリックして畳もうとすると、その pointerdown を Select と Sheet の
// DismissableLayer が同時に処理する。Select が先に閉じ、discrete イベントなので React は同期 flush する。そのため
// Sheet の処理番になるころにはポップアップの data-state はすでに closed になっている。「いま」ポップアップが開いているかを見ても
// 当てにならない（実測で確認済み）。
//
// 正しいやり方。Radix の pointerdown 監視はバブリング段階にある。それより前の capture 段階で
// 「この時点でポップアップが開いていたか」を記録し、onInteractOutside はその記録を読んで閉じるかを決める。
function isRadixOverlayOpenNow(): boolean {
  if (typeof document === "undefined") return false;
  return !!document.querySelector(
    [
      "[data-slot='select-trigger'][data-state='open']",
      "[data-slot='select-content'][data-state='open']",
      "[role='listbox'][data-state='open']",
      "[data-radix-popper-content-wrapper]",
      "[aria-expanded='true'][data-state='open']",
    ].join(","),
  );
}

let overlayOpenAtLastPointerDown = false;
if (typeof document !== "undefined") {
  document.addEventListener(
    "pointerdown",
    () => {
      overlayOpenAtLastPointerDown = isRadixOverlayOpenNow();
    },
    true, // capture。Radix のバブリング段階の pointerdown 処理より前に記録する
  );
}

// radixOverlayWasOpenAtPointerDown は、直近の pointerdown のとき Radix ポップアップが
// 開いていたかを返す。ドロワーとダイアログはこれで判断する。ポップアップが開いているときにマスクをクリックしたら、ポップアップだけ畳み、自分は閉じない。
export function radixOverlayWasOpenAtPointerDown(): boolean {
  return overlayOpenAtLastPointerDown;
}

// copyText はテキストをクリップボードへ書き、成功したかを返す。
// 背景。navigator.clipboard はセキュアコンテキスト（HTTPS / localhost）でしか使えない。IP + HTTP で
// アクセスすると undefined になる。そのときは execCommand("copy") にフォールバックする。
export async function copyText(text: string): Promise<boolean> {
  if (navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      // フォールバックへ進む
    }
  }
  try {
    const textarea = document.createElement("textarea");
    textarea.value = text;
    textarea.style.position = "fixed";
    textarea.style.left = "-9999px";
    textarea.style.top = "0";
    document.body.appendChild(textarea);
    textarea.focus();
    textarea.select();
    const ok = document.execCommand("copy");
    document.body.removeChild(textarea);
    return ok;
  } catch {
    return false;
  }
}

export const getInitials = (str: string): string => {
  if (typeof str !== "string" || !str.trim()) return "?";

  return (
    str
      .trim()
      .split(/\s+/)
      .filter(Boolean)
      .map((word) => word[0])
      .join("")
      .toUpperCase() || "?"
  );
};

export function formatCurrency(
  amount: number,
  opts?: {
    currency?: string;
    locale?: string;
    minimumFractionDigits?: number;
    maximumFractionDigits?: number;
    noDecimals?: boolean;
  },
) {
  const { currency = "USD", locale = "en-US", minimumFractionDigits, maximumFractionDigits, noDecimals } = opts ?? {};

  const formatOptions: Intl.NumberFormatOptions = {
    style: "currency",
    currency,
    minimumFractionDigits: noDecimals ? 0 : minimumFractionDigits,
    maximumFractionDigits: noDecimals ? 0 : maximumFractionDigits,
  };

  return new Intl.NumberFormat(locale, formatOptions).format(amount);
}
