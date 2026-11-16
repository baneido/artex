# `/btw` の検証記録

日付は 2026-09-10 です。ブランチは `codex/btw-side-question` です。ベースラインは `8dae851b9b622f2ff2631f332fde9719d0b16fba` です。

独立した PostgreSQL のテスト用データベースとデータディレクトリを使いました。実モデルの資格情報は独立したテスト環境にだけ注入し、コードにもこの記録にも書いていません。製品の既定モデルも変えていません。Go 1.26.3、norma v0.3.6、Next.js 16.2.9 です。

実際のモデル対話、戻りオブジェクト、エンジニアリングのアサーション、Qwen の元の審査テキストは [validation-2026-09-10.json](validation-2026-09-10.json) にあります。API の資格情報はありません。

## エンジニアリングの確認

| 範囲 | 結果 | 証拠 |
| --- | --- | --- |
| 構造化メッセージ、ツール引数のディープコピー | 通過 | `TestCheckpointDeepCopyAndBoundaries` |
| 要約と圧縮のリクエストは上書きしない。完全な応答と終態は公開する。半分の応答は除く | 通過 | `TestCheckpointDeepCopyAndBoundaries`、`TestSnapshotExcludesPartialStreamAndSelectsPoolMember` |
| 実際のモデルプールメンバーの識別 | 通過 | `TestSnapshotExcludesPartialStreamAndSelectsPoolMember` |
| ツールのペア、20 組の再生、予算による切り詰め、上限超過エラー | 通過 | `TestBuildRequestCompactionToolPairingAndBudget` |
| 主処理とサイド質問の並行、双方向のキャンセル隔離 | 通過 | ブロッキング Provider。`TestMainSideConcurrencyAndIndependentCancellation` |
| ツールを実行しない。ストリームと非ストリーム。失敗時にすでに取得した用量 | 通過 | `TestServiceNoToolsAndUsageOnFailure` |
| 実 norma の ChatAgent とローカル Read ツール。主 transcript とアクティビティの隔離 | 通過 | `TestSideActualChatCheckpointToolResultAndTranscriptIsolation`。ストリームと非ストリームの子ケース |
| 永続化、ページング、冪等、再起動後も部分回答を残す | 通過 | `TestSideHistoryIdempotencyPagingAndRecovery` |
| クリアと遅延書き込みの競合、親リソースの削除、バージョン比較 | 通過 | `TestSideClearLateWritersAndDeletedParent` |
| MainAgent と Worker のアーカイブと復元。v1、v2、v3 | 通過 | `TestSideTaskArchiveVersions` |
| 3 種類の親 API、認証、リソースの帰属、Worker の論理削除 | 通過 | `TestSideHTTPGlobalLimitTaskWorkerAndDeletion`、`TestSideCheckpointPersistsBeforeAdmissionAndRestart` |
| 忙しい主セッションでもサイド質問できる。独立した SSE の再接続と切断、キャンセル、クリア | 通過 | `TestSideHTTPBusyIsolationClearAndReconnect` |
| 親セッションごと 1、全体で 4 の同時実行 | 通過 | `TestSideHTTP…` が 2 つ |
| 送信前にスナップショットを保存する。再起動後も質問を続けられる。古いセッションはスナップショットを偽造できない | 通過 | `TestSideCheckpointPersistsBeforeAdmissionAndRestart` |
| キャッシュ中の設定が削除された、またはモデルが変わったあとは続行を拒否する | 通過 | `TestSideRejectsDeletedOrChangedCachedProfile` |
| アーカイブ前にキャンセルし、最終回答と用量の保存を待つ | 通過 | `TestSideTaskDrainPersistsBeforeArchive` |
| ストリームの消費側が早くキャンセルしたとき、用量は 1 回だけ記録し、サイド質問に帰属する | 通過 | `TestSideUsageRecordedOnceOnConsumerCancellation` |
| 再起動で自動復元した Worker と deadline の実行コンテキストは、新しいスナップショットを出し続ける | 通過 | `TestSideRestoredWorkerRuntimePublishesNewCheckpoint` |
| 関係するパッケージの race チェック | 通過 | 下のコマンド |
| TypeScript と本番ビルド | 通過 | `npx tsc --noEmit`、`npm run build` |
| 追加したフロントエンドモジュールの Biome | 通過 | `biome check`。追加モジュールは 3 つ |

捨ててよい別データベースに `ARTEX_PG_DSN` を設定すれば、自動チェックを再現できます（本番データベースは指さないでください）。

```sh
go test -race ./agent ./db ./server ./sidequestion ./llmrec ./llmpool \
  -run 'Test(Side|Checkpoint|Snapshot|BuildRequest|Service|MainSide|CaptureRun|TaskArchive|CompleteForwards|StopIntent|CancelIntent)' -count=1
cd web
npx tsc --noEmit
npx biome check src/lib/side-questions.ts src/hooks/use-side-questions.ts src/components/side-question-workspace.tsx
npm run build
```

Go の全回帰は全緑ではありません。`server` パッケージの既存テストが 2 つ、一時ディレクトリの掃除段階で失敗し、どちらも `TempDir RemoveAll … directory not empty` です。

- `TestInheritedActivityDetailAndRelationDeletion`
- `TestTaskMetadataPatchReturnsRenameAndPin`

上の未変更ベースラインからソースを取り出し、同じ隔離環境で `server` パッケージを再実行しても、この 2 つの掃除失敗は再現しました。ベースラインの実行では、別に `TestCoreTaskLifecyclePG` の目標ノード数アサーションが失敗しました。最終的な変更後の `server` 回帰には、そのアサーション失敗はありません。ほかのパッケージは通り、今回のサイド質問のケースと race チェックも通りました。ベースラインの問題を今回の受け入れ通過にはせず、問題を隠すために既存アサーションも変えていません。

Next.js のビルド出力には、既存の複数 lockfile と workspace root の推定警告があります。ビルドは完了し、すべてのページ生成は成功しました。

## ブラウザの確認

Codex In-app Browser を使い、独立したローカルの Go サービスと Next.js 開発サーバーへ接続しました。デスクトップと 390 × 844 の狭い画面で、次の人手の自動操作を行い、スクリーンショットとブラウザログを見ました。

- 通常チャットの実行中に `/btw` を入力すると、主コンテンツとサイド質問が同時に出ます。デスクトップのサイドバーは正常です。
- 続けて追問できます。サイド質問を止めたあとも、生成済みの部分は残ります。主処理は続きます。
- パネルを閉じてもリクエストは続き、開き直すと完了した回答に戻ります。ページを再読み込みしたあと、空の `/btw` で履歴が戻ります。
- 狭い画面の Drawer の入力、ボタン、履歴、閉じる操作は正常で、横オーバーフローはありません。
- クリアは確認ダイアログを使います。クリア後に履歴は消え、主 transcript とスナップショットは残ります。
- タスクの MainAgent と 2 つの Worker で別々に質問して切り替えました。Agent のラベルと履歴は混線しませんでした。
- ブロッキングのローカルモデルフィクスチャが Worker を動かし続けます。Worker の主入力欄から `/btw` を送り、サイド質問を止めたあとも Worker はリアルタイム実行と自分の一時停止ボタンを表示し、サイド質問は部分回答を保存します。
- ブラウザのエラーと警告のログは空です。

制御できるフィクスチャは、同時実行のタイミングを正確に見るためのもので、実モデルの出力速度には依存しません。デバッグ中の Worker 実行時チェックは 2 回、有効な同時ウィンドウになりませんでした（タスクが終わっていた、または回答が早く終わった）。フィクスチャを直してやり直して通りました。最初の操作は有効な通過には入れていません。

## 実モデルの対話

先に `grok-4.6` を探しました。OpenAI 互換のインターフェースは `http://127.0.0.1:12580/tingly/openai` です。探索は HTTP 200 で、モデル名 `grok-4.6` と `READY` を返し、2.82 秒でした。第一候補が使えたので、Tingly の `glm` と Zhipu の `glm-5.3` の予備チェーンは有効にしていません。この 2 つの予備サービスは、今回は検証していません。

| 場面 | 実際の結果 |
| --- | --- |
| 主セッションの実行中に、資産、目標、印を尋ねた | `redhaze.top`、トップページの読み取りと目標の要約、`BTW-REAL-0910` を返した。サイド質問は完了し、16.97 秒 |
| 主セッションがトップページの読み取りを終えたあと、ツールの根拠を尋ねた | WebFetch 200、curl のリダイレクト 301 から 302、さらに 200、ページタイトルを正しく引用した。7.24 秒 |
| サイド質問に Bash でテストファイルを作るよう求めた | 実行を拒否し、対象ファイルは作られなかった。7.74 秒 |
| 完了後のサイド質問は主コンテキストを変えない | 主 transcript の SHA-256 は主アクティビティ記録と一致した。サイド質問のツール実行回数は 0 |
| Go サービスを実際に止めて再起動したあと追問した | 以前のサイド質問履歴 3 件を残し、永続化したスナップショットから資産、印、タイトルに直接答えた。主 Agent は再実行していない |
| 新しいセッションで Grok の非ストリーム設定を使った | 資産と `ATOMIC-0910` に正しく答えた。用量を返して保存した。input 11734、output 138、cache_read 11520 |

資産の事例では、主セッションが WebFetch と Bash/curl で公開トップページを読みました。ランディングページは `https://id.redhaze.top/home` で、タイトルは「红幕科技 RedHaze Group · 全球综合集团门户」です。Bash はレスポンスをローカルのテストファイルへ一時保存しました。リモートへの書き込みはしていません。この事実は「サイド質問はツールを実行していない」とは別に確認しました。

主 transcript の検証値は `e7e61f135a4a120954b539f357e8c4205d7d5cd7460dcaf3dc0fd066463e1d00` です。

**用量の制限:** Tingly の Grok ストリーム応答は usage を返しませんでした。別に `stream_options.include_usage=true` を直接送って確認すると、HTTP 200、データフレーム 12、usage フレーム 0 でした。したがってストリームテストの 0 は、エンドポイントが用量を出していないという意味であり、課金が無いという意味にはできません。非ストリームの用量と、フィクスチャの失敗およびキャンセルの用量は、正しく保存しました。

## Qwen の審査

審査モデルは `qwen-flash`、OpenAI 互換のインターフェースは `https://dashscope.aliyuncs.com/compatible-mode/v1`、HTTP 200 です。実サイド質問の対話を先頭 3 件、主セッションのツール根拠、エンジニアリングのアサーションを渡しました。戻りは `verdict: accept`、`concerns: []` で、回答は資産、印、ページ読み取りの証拠と一致し、サイド質問がツールを拒否したことは制約に合う、という判断です。審査の用量は prompt 6625、completion 312、total 6937 です。

この Qwen 審査の範囲には、あとから足したサービスの再起動と非ストリームのテストは入りません。Qwen の「書き込み無し」という要約は広すぎます。主セッションの curl は、ローカルのレスポンス一時ファイルを実際に作りました。上に明記しています。同時実行、ツール実行ゼロ、transcript の隔離はエンジニアリングのアサーションで判断し、モデル審査は回答の質の補助評価だけです。
