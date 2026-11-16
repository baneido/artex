#!/bin/sh
# ARTEX 常駐起動スクリプト（Linux / macOS / Docker ENTRYPOINT）
#
# 使い方:
#   ./start.sh                       フォアグラウンドで実行する（Ctrl-C で停止）
#   nohup ./start.sh >artex.log 2>&1 &   バックグラウンドで常駐する
#   ./start.sh -addr :9000           追加の引数はそのまま artex に渡す
#
# やることは一つだけである。artex を起動し、終了コードで再起動するかどうかを決める。
#
#   0      ユーザーが正常に停止した   → ループを抜ける
#   75     プログラムが再起動を要求した → すぐに再実行する（画面で「ワンクリックアップデート」または「ロールバック」を押した）
#   その他 クラッシュした             → 待ってから再実行する（1→2→4…最大 60 秒）
#
# ダウンロード、SHA256 の検証、バイナリの入れ替えは、ここでは意図的にしない。
# その処理を sh と bat の両方に書くと二系統になり、しかもそこが最も間違えてはいけない箇所である。
# 起動できないバイナリに入れ替わると、このスクリプトは律儀にそれを繰り返し起動し、ユーザーはマシンに入って手で直すしかない。
# そのため検証と入れ替えはすべて Go（selfupdate パッケージ）に残し、artex 自身が起動時に完了する。スクリプトは単純なままにする。
set -u

cd "$(dirname "$0")" || exit 1

BIN=./artex
[ -x "$BIN" ] || { echo "[artex] 実行ファイル $BIN が見つかりません" >&2; exit 1; }

RESTART_CODE=75
MAX_DELAY=60

child=0
stopping=0

# 停止シグナルを artex 本体へ転送する。
#
# Docker ではこれが必須である。docker stop は SIGTERM を PID 1（このスクリプト）にだけ送り、
# 子プロセスには送らない。転送しないと artex はシグナルを受け取れず、猶予のある終了ができない。
# 10 秒後に SIGKILL で強制終了され、実行中のタスクが途中で切れる。
forward() {
	stopping=1
	if [ "$child" -ne 0 ]; then
		kill -TERM "$child" 2>/dev/null || true
	fi
}
trap forward INT TERM

# Wait for either the application or its restart delay. A stop that arrived
# between starting the child and recording its PID must reach that child too.
wait_for_child() {
	if [ "$stopping" -eq 1 ]; then
		kill -TERM "$child" 2>/dev/null || true
	fi
	wait "$child"
	code=$?
	if [ "$code" -gt 128 ]; then
		wait "$child"
		code=$?
	fi
	child=0
}

delay=1
while [ "$stopping" -eq 0 ]; do
	"$BIN" "$@" &
	child=$!

	# シグナルは wait を中断し、128 より大きい値を返させる。このとき子プロセスはまだ猶予のある終了の途中なので、
	# 本当の終了コードを得るにはもう一度 wait する必要がある。
	wait_for_child

	if [ "$stopping" -eq 1 ]; then
		echo "[artex] 停止しました"
		exit 0
	fi

	case "$code" in
		0)
			echo "[artex] 正常に終了しました"
			exit 0
			;;
		"$RESTART_CODE")
			# アップデートまたはロールバックの準備ができている。再実行後、artex は起動時に入れ替えを完了する（selfupdate.Bootstrap を参照）。
			echo "[artex] 再起動を要求しました（新しいバージョンを適用します）…"
			delay=1
			;;
		*)
			echo "[artex] 異常終了しました (code=$code)。${delay} 秒後に再起動します" >&2
			sleep "$delay" &
			child=$!
			wait_for_child
			delay=$((delay * 2))
			[ "$delay" -gt "$MAX_DELAY" ] && delay=$MAX_DELAY
			;;
	esac
done
echo "[artex] 停止しました"
exit 0
