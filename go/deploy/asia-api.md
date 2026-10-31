# 海外のローカル参照とasiaへのHTTP集約

現在のデプロイ構成。従来構成の履歴はlocal-read-update.mdとread-replicas.mdを参照。

- isu01は従来どおりisu04のPrimaryへ直接SQLを実行する。
- isu02・isu03は通常の表示データをlocalhostの非同期PostgreSQL Replicaから取得する。
- 海外の更新リクエストはasiaへHTTPで一度だけ転送し、Primary付近でトランザクション全体を実行する。
- 認証・現在のブロック/友達関係・公開範囲・画像閲覧権限は、専用の型付きHTTP APIでasiaのPrimaryを参照する。通常の参照に認証等のHTTP往復は残るが、SQLごとの海外往復をまとめる。
- 通常リクエストごとにPrimaryのWAL位置を取得しない。Replicaの利用資格はバックグラウンドで確認し、起動直後・異常時はasiaへフォールバックする。
- 非同期のため表示データには反映遅延がある。権限や無効化セッションの判定にはReplicaを使わない。

内部APIは`/api/internal/primary`。既存の画像共有トークンから用途別の鍵を導出し、HTTPメソッド・URI・元リージョン・時刻・nonce・認証情報・bodyをHMACで認証する。応答も認証し、リプレイを拒否する。元リージョンを信頼するのは署名確認後のみで、サーバーの固定リージョンは変更しない。更新は自動再試行しない。通信切断時には更新結果が不明な可能性がある。

通信先はasiaアプリの`192.168.1.10:80`。VPCルートに従う競技用ルーター経由で、ルーター自身を宛先にしない。PostgreSQLの物理レプリケーションも既存ルートを使用する。Primaryはisu04だけである。

initializeは従来のPrimary初期化後、50秒を既定の上限として両Replicaのrecovery状態・timeline・WAL再生完了を確認する。初回basebackupは事前に行い、initializeでは実行しない。

## デプロイとロールバック

このworktreeでLinux/amd64バイナリをビルドし、`bash go/deploy/asia-api-deploy.sh /path/to/binary`を実行する。asiaを先に配置し、署名付きreadyが通ってから海外の`ISUREAL_PRIMARY_API_URL=http://192.168.1.10`を有効化する。既存の画像複製設定は維持する。

失敗したホストはスクリプトが旧バイナリとprimary-api.confへ戻す。全体を戻す場合は海外2台を先に、最後にasiaを戻し、各ホストの`isureal.before-asia-api-<stamp>`と`primary-api.conf.before-<stamp>`を復元する。設定のバックアップが存在しない場合、そのロールアウトで作ったprimary-api.confだけを削除する。daemon-reload後にisureal-goを再起動する。PostgreSQLの再初期化や昇格は不要。

2026-10-31 06:42 UTCの配置結果と検証の制約は[DEPLOYMENT.md](replica-results/asia-api-merged/DEPLOYMENT.md)を参照。CPU最適化も統合しているため、古い別worktreeからの配布は今回の構成を上書きする。今回の版での公式ベンチマークの成功・性能改善は未確認。
