import { fileURLToPath } from "node:url";

// 静的エクスポート。`NEXT_EXPORT=1 next build` は純静的ディレクトリを web/out に出し、
// nginx の web ルートへそのまま置いて動かせる。開発（next dev）ではこの変数を付けず、/api のリバースプロキシとホットリロードを残す。
const isExport = process.env.NEXT_EXPORT === "1";
// Vercel demo。サイト全体を mock にし、バックエンドは無く、/api のリバースプロキシも要らない。
const isMock = process.env.NEXT_PUBLIC_MOCK === "1";

/** @type {import('next').NextConfig} */
const nextConfig = {
  // 親ディレクトリの lockfile がルート判定とリソースパス生成に影響しないようにする。
  turbopack: { root: fileURLToPath(new URL(".", import.meta.url)) },
  reactCompiler: true,
  // ローカルネットワークの IP から dev リソース（HMR）へアクセスできるようにする。必要に応じて増減する。
  // dev では任意の IPv4 からの /_next/* と HMR を許可する（ローカルネットワークの IP が変わっても影響しない）。
  // 注意。Next は安全のため裸の "*" を禁じており、セグメント単位のワイルドカードが要る。"*.*.*.*" は任意の IPv4 に一致する。
  allowedDevOrigins: ["*.*.*.*"],
  compiler: {
    removeConsole: process.env.NODE_ENV === "production",
  },
  ...(isExport
    ? {
        // 純静的エクスポート。Node ランタイムは無い。画像は最適化しない。各ルートは <route>/index.html を出す。
        output: "export",
        images: { unoptimized: true },
        trailingSlash: true,
      }
    : isMock
      ? {
          // Vercel mock demo。バックエンドは無く、/api のリバースプロキシは要らない。
          images: { unoptimized: true },
        }
      : {
          // 開発。/api/* を Go バックエンドへプロキシする（既定 :8787。AUTOPENTEST_API で上書きできる）。
          async rewrites() {
            const backend = process.env.AUTOPENTEST_API ?? "http://localhost:8787";
            return [{ source: "/api/:path*", destination: `${backend}/api/:path*` }];
          },
        }),
};

export default nextConfig;
