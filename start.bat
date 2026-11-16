@echo off
rem コンソールを UTF-8 にする。そうしないと、このファイルの日本語が GBK 端末では文字化けする。
chcp 65001 >nul 2>&1
rem ARTEX 常駐起動スクリプト（Windows）
rem
rem 使い方:
rem   start.bat                  フォアグラウンドで実行する（Ctrl-C で停止）
rem   start.bat -addr :9000      追加の引数はそのまま artex に渡す
rem
rem やることは一つだけである。artex.exe を起動し、終了コードで再起動するかどうかを決める。
rem
rem   0      ユーザーが正常に停止した -> ループを抜ける
rem   75     プログラムが再起動を要求した -> すぐに再実行する（画面で「ワンクリックアップデート」または「ロールバック」を押した）
rem   その他 クラッシュした            -> 待ってから再実行する（1->2->4…最大 60 秒）
rem
rem ダウンロード、SHA256 の検証、入れ替えはここでは行わない。すべて artex 自身が起動時に完了する
rem （selfupdate パッケージ）。スクリプトは単純なままにする。詳しくは start.sh 先頭の説明を参照する。

setlocal enabledelayedexpansion
cd /d "%~dp0"

set "BIN=artex.exe"
if not exist "%BIN%" (
	echo [artex] 実行ファイル %BIN% が見つかりません 1>&2
	exit /b 1
)

set "RESTART_CODE=75"
set "MAX_DELAY=60"
set /a delay=1

:loop
"%BIN%" %*
set "code=!ERRORLEVEL!"

if "!code!"=="0" (
	echo [artex] 正常に終了しました
	exit /b 0
)

if "!code!"=="%RESTART_CODE%" (
	rem アップデートまたはロールバックの準備ができている。再実行後、artex は起動時に入れ替えを完了する。
	echo [artex] 再起動を要求しました（新しいバージョンを適用します）…
	set /a delay=1
	goto loop
)

echo [artex] 異常終了しました ^(code=!code!^)。!delay! 秒後に再起動します 1>&2
rem リダイレクトされたコンソールでは timeout が失敗する。ping で代替する（N 秒待つには N+1 回必要である）。
set /a pings=!delay!+1
ping -n !pings! 127.0.0.1 >nul 2>&1
set /a delay=!delay!*2
if !delay! gtr %MAX_DELAY% set /a delay=%MAX_DELAY%
goto loop
