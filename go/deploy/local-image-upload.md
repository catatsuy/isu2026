# 海外での画像アップロード

対象は POST /api/posts、PUT /api/me/profile-image、POST /api/posts/:postId/realmojis。
それ以外の海外 /api/ は引き続き asia に転送する。asia への直接リクエストは従来の JPEG 検証・保存を行う。

## 処理と認証

海外では既存の署名付き read_auth でセッション・IP Ban・リージョンを事前確認する。未認証では JPEG の永続保存に進まない。通常時のリージョン間通信は事前認証と更新の2往復。認証をリクエスト間でキャッシュしない。

既存の外部 handler が完全な jpeg.Decode、寸法・フィールド検証を行い、新しい UUID で ImageFileStore.Save を呼ぶ。Save はローカル画像と複製ジョブを fsync してから戻る。画像データは image_upload RPC に含めない。

内部 payload:

- 共通: kind、image_id
- post: caption、visibility、locationKind、latitude、longitude、locality
- profile: 追加なし
- realmoji: post_id、reaction_type

既存の HMAC・時刻・nonce・リプレイ拒否・署名付き応答を再利用する。Authorization・元リージョン・クライアントIPも署名対象。image_upload は認証済み海外リクエストのコンテキストを必須にする。外部 handler はクライアントの image_id を受け入れず、検証後に生成した UUID だけを使う。

asia は再認証後、commitPost / commitProfileImage / commitRealMoji で従来のトランザクションを実行する。これらは直接アップロードとも共通。actor の行ロック取得後にもセッション有効性・active状態・リージョン・IP Ban を再確認する。投稿枠・Window・友達/ブロック・RealMoji閲覧権限・削除との競合・ロック順序は既存処理を使う。投稿時刻・リアクション作成時刻は asia の更新時に決まる。

## 投稿直後の画像

DB COMMIT は画像 PUT 完了を待たない。asia の Ensure はローカル不在時に両 peer へ署名付き GET を並行実行し、本文の SHA256 に対応する応答 HMAC を検証してローカルへ fsync する。singleflight で同じ画像の fill を共有し、最初の呼び出しのキャンセルが他の待機者を失敗させない。共有 fill は独立した15秒制限を持つ。ローカル保存後は通常の nginx 配信になる。

画像の公開可否・削除・アカウント状態・URL署名/期限は既存の Primary 参照で判定する。Ensure が1秒以上かかった場合の権限再確認も維持する。画像の存在だけでは公開されない。

## 失敗

JPEG・寸法・multipart・フィールドのエラーは元の外部 handler と同じ。ファイル/fsync/ジョブ保存の失敗時は metadata RPC を呼ばない。DB失敗・HTTP失敗・COMMIT不明ではファイル/ジョブを削除せず、元の multipart も metadata 更新も自動再送しない。複製だけは既存の永続キューで再試行する。

## 配置と復帰

`asia-api-deploy.sh` は asia を先に配置し、署名付き ready と upload_ready を確認してから海外を配置する。旧海外版は新しい asia へ従来の multipart を送れるため混在期間も互換性がある。PostgreSQL Replica・nginx・スキーマは変更しない。海外の画像複製先は既存設定どおり asia が先頭。

ロールバックは isu02・isu03 を先に旧バイナリへ戻し、最後に isu01 を戻す。`/home/isucon/webapp/go/isureal.before-asia-api-<stamp>` と `primary-api.conf.before-<stamp>` を使い、daemon-reload とサービス再起動を行う。画像ファイルと .replication は削除しない。PostgreSQL Replica は停止・mask のままにする。

## 計測

`local-upload-probe.py before|after europe|america 20` は各海外ホストで実行する。投稿用 JPEG は 2048×2048/3,759,501 bytes、profile/RealMoji用は 512×512/235,755 bytes の固定入力。既存の active Window を使い、新規検証アカウントは終了時に退会する。initialize・Window変更・公式ベンチマーク・Slack通知は行わない。

各 phase の journal ログ（image_upload）: jpeg の完全デコード時間、ローカル fsync、ジョブ fsync、事前認証、metadata HTTP、asia DB処理、metadata本文サイズ、全peer複製完了時間。replicated.duration_us はジョブの mtime からの経過で、再試行後は再試行予定時刻基準になるため失敗を含む画像の総時間とは区別する。

署名付き diagnostics は asia の image_ensure_fallback / image_ensure_failures を返す。CPU/ネットワークはプローブ中の /proc 15秒差分を保存する。小標本の P95/P99 と低負荷の比較であり、公式ベンチマークのスコア比較とは区別する。
