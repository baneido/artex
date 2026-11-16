// Mock のスイッチ。ビルド時に注入する公開変数（NEXT_PUBLIC_ プレフィックスだけがブラウザから読める）。
// Vercel で NEXT_PUBLIC_MOCK=1 を設定するとサイト全体が mock になり、バックエンドは不要。
export const MOCK = process.env.NEXT_PUBLIC_MOCK === "1";
