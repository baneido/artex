#!/usr/bin/env bash
# 開発モード。バックエンド (:8787) とトラフィックプロキシ (:8788)、フロントの next dev (:5173) を同時に起動する。
# フロントの /api はバックエンドへプロキシする。Ctrl-C でまとめて終了する。
#
# 単一バイナリ（フロント埋め込み）は README の「単一バイナリ」を参照する。このスクリプトでは行わない。
set -euo pipefail
cd "$(dirname "$0")"

# 終了時に、このプロセスグループの子プロセス（バックエンドとフロント）をすべて止める。
cleanup() { kill 0 2>/dev/null || true; }
trap cleanup EXIT INT TERM

# バックエンド（通常の go run。フロントは埋め込まない）。同時に動かす work agent の数は「システム設定」で決める。
go run ./cmd/artex -addr :8787 -proxy 127.0.0.1:8788 &

# フロントのホットリロード（Next の dev server。/api は :8787 へプロキシする）。
( cd web && npm run dev ) &

echo "[dev] バックエンド :8787 / プロキシ :8788 / フロント http://localhost:5173  (Ctrl-C で終了)"
wait
