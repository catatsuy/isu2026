# 2026-10-31 ベンチマークに対する変更

## 計測の解釈

画像アクセス制御の遅いSQLは合計約554秒、Posting Window取得は約244秒であり、優先して改善した。SQL時間にはロック待ちが含まれるため、CPU処理時間とは解釈していない。SSEの18秒というrequest_timeも処理遅延とは解釈していない。本番への接続・デプロイ、Go以外の実装の参照、DBスキーマの変更は行っていない。このチェックアウトに `go/AGENTS.md` は存在しなかった。

## 画像取得

対象: `image_api.go`, `image_api_test.go`, `image_access_sql_test.go`

従来は次の2往復を行っていた。

1. `image_objects JOIN users` で有効な所有者の署名鍵を取得。
2. 同じJOINでmedia_typeと、プロフィール・投稿・RealMojiのいずれかが有効かを確認。

これを `imageAccessQuery` の1往復にまとめた。`OR EXISTS (...) OR EXISTS (...)` は `CASE WHEN profile THEN true WHEN EXISTS(post) THEN true ELSE EXISTS(realmoji) END` に変更し、プロフィール画像や投稿画像で不要な後続条件を評価しない構造にした。各検索は引き続き画像IDと所有者IDの両方を条件にする。新たなキャッシュやアクセス制御の省略はない。

署名・期限、所有者のactive状態、投稿の削除、RealMojiの置換・削除、対象投稿の削除と投稿者のactive状態を維持する。ファイル取得が遅い場合のDB再確認、配信直前の期限確認も維持する。通常のキャッシュ済み画像ではSQL数が2から1になることをfixtureテストで確認した。実行計画やCPU時間の改善率は未測定。

## プロフィール画像の5xxと保存処理

対象: `profile_image_api.go`, `profile_image_api_test.go`

コード上の5xx経路は、リクエストbody読込失敗、画像保存失敗、Begin、ユーザー行ロック取得、image_objects INSERT、users UPDATE、旧画像メタデータDELETE、Commitのエラー。ユーザー不在は404、JPEG不正・寸法違反は400、Content-Type違反は415。

画像保存には、サイズ上限、ディレクトリ・ファイル作成、書込・同期・link、既存内容不一致、複製先未設定、全複製先の通信/応答失敗、リクエストキャンセルという失敗経路がある。複製は最低1台の永続化確認を要求し、通信には15秒のタイムアウトがある。最低1台への複製確認という条件は変更していない。

従来はユーザー行をロックしてから、ローカル永続化・複製を待っていた。保存をDBトランザクションより前へ移動し、その間のDB接続占有とユーザーロック保持をなくした。投稿画像も同様に変更した。遅い画像保存による他のユーザー操作への待ち時間の波及を抑えられる。ただし、12件の5xxの原因を再現・確定したわけではなく、5xxが解消したとは主張しない。発生時のアプリログ、PostgreSQLのエラーコード、複製先ログ・ディスク状態との突合が必要。

PUT/PUT、PUT/DELETEは従来どおり `users FOR NO KEY UPDATE` で直列化する。保存後にロックを取り、その時点の旧画像IDを読み直すため、保存中に別の更新・削除があっても古いIDを前提に削除しない。新画像の登録・参照切替・旧画像メタデータ削除は同一トランザクション。ロールバックしても旧画像参照は維持される。

ファイルはUUIDごとの不変オブジェクトであり、DBロールバック時も消去しない。以前から失敗後の未参照ファイルは残り得たが、今回の移動により投稿枠不足・ユーザー不在でも保存済みファイルが残る機会は増える。公開GETでは必ずDB認可を行うため未参照ファイルは配信されない。コミット結果が不明な場合や並行複製を考慮し、安易なファイル削除は追加していない。長期運用の未参照ファイル回収は別途検討が必要。

## Posting Window

対象: `post_api.go`, `posting_window_lock_test.go`

```sql
-- 変更前
SELECT id, local_date, starts_at FROM posting_windows
WHERE region = $1 AND ended_at IS NULL FOR UPDATE;
-- 変更後
SELECT id, local_date, starts_at FROM posting_windows
WHERE region = $1 AND ended_at IS NULL FOR SHARE;
```

投稿はWindow行を変更しないため、異なるユーザーの投稿同士は共有可能。終了処理の `UPDATE posting_windows SET ended_at=...` はFOR SHAREと競合するため、投稿コミットまで終了を待つ保証は残る。終了処理が先行した場合はロック待ち後にended_at条件が再評価される。既存の1回の再検索も維持した。

ユーザー行→Window行のロック順序は変更していない。ユーザー単位の投稿・削除の排他を維持しているため、投稿数・削除数の確認と挿入/削除の間に同じユーザーの操作が割り込まない。投稿側にはWindow行の更新・排他ロックへの昇格も追加していない。終了処理には新たなユーザー行の排他取得を追加していない。この変更による新しい逆順ロック取得はない。

## SSE

対象: `notification_hub.go`, `notification_api.go`, `notification_hub_test.go`

共有pollのエラー時に全購読者をremoveしていた。エラー時は購読者・最後の成功時のWindow情報を保持し、次の共有pollで再試行する。復旧時には失敗期間中のWindow変化を最後の成功情報と比較する。

個別通知SELECT失敗時も即切断せず、購読者にretryフラグを付け、次の成功した共有pollから再通知する。接続別のタイマーや定期ポーリングは追加していない。ユーザーの無効化・リージョン変更は成功したpollで確認した場合のみ切断する。フラグや購読状態は同じmutexで保護し、終了済み購読への操作と旧poll世代のキャンセル処理も維持した。長時間のDB障害では接続が残るが、クライアントキャンセルで終了する。新規接続の最初の時刻取得失敗は引き続きエラーになる。

## 今回変更しなかった処理

- `users FOR NO KEY UPDATE`: 投稿数・削除数、画像更新、関係変更などの整合性に必要。削除していない。画像保存を外へ移して保持時間を短縮した。
- `LOCK TABLE users`: Window開始とユーザー登録/リージョン変更などの整合性、開始処理同士の直列化にも関係する。単純に削除すると通知対象の整合性を壊すため維持。ロック範囲の縮小は、これらの処理を合わせて別途検証する必要がある。
- Friendshipの複数ID検索: `post.go` の友人の友人・共通友人数と `feed_api.go` の推薦スコアに使用。既にまとめて2列を取得しておりN+1ではない。両端が対象集合に入る辺を二重に数えない必要があるため、ORを単純なUNION ALLへ変更していない。実行計画、集合サイズ、既存インデックスを確認してから検討する。
- RealMoji画像保存は関係する2ユーザーのロック内に残る。今回の優先範囲を超えるため変更していない。

## インデックス候補（未適用）

既存インデックスと実行計画をDBで確認できていないため、追加が必要とは断定しない。画像検索が全走査になっている場合の候補は以下。同等の既存インデックスがあれば不要。

```sql
CREATE INDEX ON posts (image_id, author_id) WHERE deleted_at IS NULL;
CREATE INDEX ON realmojis (image_id, reactor_id)
  WHERE replaced_at IS NULL AND deleted_at IS NULL;
```

まず変更後SQLを代表的な画像種類ごとに `EXPLAIN (ANALYZE, BUFFERS)` で確認し、不要な走査とロック待ちを区別する。Friendshipも両端列それぞれを先頭にするインデックスの有無を確認する。

## 検証

- 画像HTTPテスト: 署名/期限、キャッシュ済み画像の再認可、削除反映、遅い複製取得中の削除、DB画像bytesへのフォールバック禁止。
- プロフィール画像fixtureテスト: 複製中のDB接続占有なし、複製失敗時Beginなし、INSERT失敗時ロールバック。
- Hubテスト: pollエラーから復旧、失敗中の新規購読、Window変化の通知、個別SELECT再試行のwake。既存の購読終了・同時購読テストも実行。
- PostgreSQL用テスト: 画像認可のプロフィール/投稿/RealMoji・削除・置換・所有者不一致・無効ユーザーの条件、および共有ロック同士の並行取得と終了UPDATEとの競合。`ISUREAL_TEST_DATABASE_URL` が未指定の場合はskip。使い捨てのローカルDBを指定すること。画像テストは一時テーブル、ロックテストは一意なテストスキーマを作成し終了時に削除する。
- ローカルPostgreSQL環境がないため、実DB用テスト・実行計画・スループット改善・実DB上のPUT/DELETE競合やデッドロック再現は未検証。

最終状態で `go test ./...` と `go test -race ./...` はともに成功。実DB用の2テストはDB未設定によりskip。最初のサンドボックス内実行はキャッシュ・ローカルlisten制限で失敗したため、実行許可を得て再実行した。`git diff --check` も成功。作業開始時から存在した `session.go`, `user_api.go`, `login_test.go` のログイン関連変更は変更していない。
