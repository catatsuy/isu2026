# 全 API を asia に集約

- isu01 (asia) だけが isu04 の Primary へ SQL を実行する。
- isu02・isu03 は GET・画像・通知ストリームを含む `/api/` リクエストを asia へ転送する。画像付き更新3 APIは海外で検証・永続保存し、メタデータだけを asia へ送る（[詳細](local-image-upload.md)）。海外では DB 接続プールも Replica 監視も起動しない。
- 海外では `ISUREAL_PRIMARY_API_URL=http://192.168.1.10` が必須。未設定なら起動を拒否する。
- Replica 環境変数が残っていても使用しない。initialize は Primary だけを初期化し、Replica の再生完了を待たない。
- `/internal/image-files/` のファイル複製は独立して継続する。

内部APIは`/api/internal/primary`。既存の画像共有トークンから用途別の鍵を導出し、HTTPメソッド・URI・元リージョン・時刻・nonce・認証情報・bodyをHMACで認証する。応答も認証し、リプレイを拒否する。元リージョンを信頼するのは署名確認後のみで、サーバーの固定リージョンは変更しない。更新は自動再試行しない。通信切断時には更新結果が不明な可能性がある。

通信先はasiaアプリの`192.168.1.10:80`。VPCルートに従う競技用ルーター経由で、ルーター自身を宛先にしない。Primaryはisu04だけである。

## デプロイとロールバック

このworktreeでLinux/amd64バイナリをビルドし、`bash go/deploy/asia-api-deploy.sh /path/to/binary`を実行する。asiaを先に配置し、署名付きreadyが通ってから海外の`ISUREAL_PRIMARY_API_URL=http://192.168.1.10`を有効化する。既存の画像複製設定は維持する。

失敗したホストはスクリプトが旧バイナリとprimary-api.confへ戻す。全体を戻す場合は海外2台を先に、最後にasiaを戻し、各ホストの`isureal.before-asia-api-<stamp>`と`primary-api.conf.before-<stamp>`を復元する。設定のバックアップが存在しない場合、そのロールアウトで作ったprimary-api.confだけを削除する。daemon-reload後にisureal-goを再起動する。PostgreSQL の Replica 停止・slot 削除後は、旧構成へのロールバックに Replica の再構築と mask 解除も必要。

旧 Replica 構成での 2026-10-31 06:42 UTC の配置結果と検証の制約は[DEPLOYMENT.md](replica-results/asia-api-merged/DEPLOYMENT.md)を参照。CPU最適化も統合しているため、古い別worktreeからの配布は今回の構成を上書きする。今回の版での公式ベンチマークの成功・性能改善は未確認。

## PostgreSQL Replica の停止

アプリ配置と海外の参照確認後、isu02・isu03 の専用 standby を `sudo systemctl disable --now postgresql@16-main` と `sudo systemctl mask postgresql@16-main` で停止する。isu04 は停止しない。事前に Primary の `synchronous_standby_names` が空であることを確認する。停止後、isu04 で不要な WAL 保持を解除する：

```sql
SELECT pg_drop_replication_slot(slot_name)
FROM pg_replication_slots
WHERE slot_name IN ('isu02', 'isu03')
  AND slot_type = 'physical' AND NOT active;
```

slot 削除後に旧 Replica 構成へ戻す場合は再構築が必要。

## 2026-10-31 06:58 UTC 配置結果

- isu01 → isu02 → isu03 の順で配置・再起動し、全台の画像 API 未署名リクエストが 403 になることを確認。
- 旧バイナリ: `/home/isucon/webapp/go/isureal.before-asia-api-20261031T065824Z`。設定バックアップも同じ suffix。
- 海外 PostgreSQL は停止・mask 済み。Primary の physical slot `isu02`・`isu03` は削除済み。旧 Replica 構成への復帰には再構築が必要。
- PostgreSQL 停止中の europe・america で検証アカウント登録、ログイン、プロフィール更新と即時参照、friends フィード、友達一覧を確認。検証アカウントは削除済み。
- Primary の replication 接続と該当スロットは各0件。isureal DB 接続元は asia (`192.168.1.10`) のみ。
- 公式ベンチマークと、Slack 通知を含む計測準備スクリプトは未実行。
