> 最新の構成は[local-read-update.md](local-read-update.md)を参照。
> 以下は初回の「プロフィールのみReplica」段階の履歴。現在は海外の主要データ読み取りへ拡張し、追加トランザクションを撤去している。

# PostgreSQL読み取りReplica：実装と稼働記録

2026-10-31。**最終方針はユーザーの追加指示による非同期＋限定利用**。
`remote_apply`を試験した後、書き込み遅延を避けるため非同期に変更した。
全SELECTのローカル化は採用していない。

## 現在の役割と読み取り方針

| サーバー | 役割 | アプリの接続 |
|---|---|---|
| isu01 / asia | Go | Primaryのみ |
| isu02 / europe | Go＋PostgreSQL 16 physical standby | 自分のプロフィール本体だけlocalhost、他はPrimary |
| isu03 / america | Go＋PostgreSQL 16 physical standby | 自分のプロフィール本体だけlocalhost、他はPrimary |
| isu04 / asia | 唯一の書き込み可能なPostgreSQL 16.15 | Primary |

Primaryは`wal_level=replica`、`max_wal_senders=10`、`max_replication_slots=10`。
最終設定は`synchronous_standby_names=''`、`synchronous_commit=on`。
`on`はPrimary自身のWAL永続化を待つ設定であり、Replicaへの反映は待たない。
isu02/isu03のapplication_nameと物理スロット名は、それぞれ`isu02`/`isu03`。
Primaryを再初期化せず、許可された停止中の既存ローカルDBだけを削除して事前seedした。

Replicaが利用する接続先は`192.168.4.10:5432`。
アプリからReplicaへの接続先は`127.0.0.1:5432`。
GoとPostgreSQLはsystemdで自動起動する。
既存の画像ファイル保存・非同期画像複製・nginx設定は維持している。

## Goの分離

`HandlerContext.PrimaryDB`と`ReplicaDB`を明示し、`WriteDB()`を共通のPrimary選択口とした。
旧`DB`フィールドは既存テストfixtureとの互換用のPrimary alias。
SQL文字列で読み書きを判別していない。

Replicaへ渡せるのは`GET /api/me`のプロフィール本体取得だけ。
表示名・bio・プロフィール画像URLなどには短い反映遅延を許す。
地域はPrimaryで検証したセッションの値を返す。
新規ユーザーがReplicaにまだ存在しない場合や読取エラー時はPrimaryへ再取得する。

以下は常にPrimary：

- 認証、ログインセッション、退会状態、IP banの取得
- 書き込みとその事前SELECT、書き込みトランザクション、行ロック
- 友達関係、ブロック、公開範囲、投稿・コメント・RealMojiの閲覧判定
- ユーザー検索・他人のプロフィール、Memories・export
- 画像の署名・閲覧許可・遅い画像取得後の再認可、通知と通知hub

複数SQLの一貫性が必要なGETは、read-only / repeatable-readの共通リクエストトランザクションを使う。
認証はReplicaトランザクションとは別にPrimaryで検証する。
したがって**プロフィールAPIにも認証のリージョン間RTTは残る**。
通常のHTTP読み取りでPrimaryへWAL位置を問い合わせる処理はない。

## 起動・障害・復旧

Replicaは起動直後には利用しない。1秒ごとのバックグラウンド確認で、次を照合する。

- Primaryがrecovery中でないこと、system identifierとtimeline
- 対象application_name・接続元IP・物理スロット・walsenderの組み合わせ
- ローカルDBがrecovery中で、replayがpauseされていないこと
- walreceiverのsender_host、slot_name、received_tli、streaming状態
- チェックで取得したPrimary flush LSNまでローカルreplayが進んだこと

各チェックの期限は900ms、利用資格の有効期間は2秒。
失敗・期限切れ・ローカル接続失敗時はPrimaryを使用し、追従確認後にのみ復帰する。
チェックの途中で切断された場合も古い認証・閲覧判定をReplicaで行うことはない。
許可したプロフィール情報には非同期反映の競合があり、書込み直後の最新値は保証しない。

同期quorumは最終構成では存在せず、Replica停止によりPrimaryのCOMMITは停止しない。
Goの変更系リクエストには10秒の期限を設けている。
接続切断や期限切れのCOMMITは結果不明としてエラーにし、自動再試行しない。
Primaryの昇格先を増やしたり、Replicaを自動昇格させる処理はない。
同期試験に使用したCOMMIT待ち監視timerは最終構成から削除した。

`local`/`fallback`は認証成功後のプロフィール本体取得の試行数。
`policy_primary`はReplica対象外のGET数。10秒ごとにjournaldへ出力する。
これらは全SQL数を数えるメトリクスではない。

## initialize

通常の配置では全アプリに`ISUREAL_WAIT_REPLICAS=true`を設定する。
既存`sql/init.sh`をPrimaryに実行し、psqlの正常終了＝reset.sqlのCOMMIT完了を確認する。
その後flush LSN、system identifier、timelineを記録し、Primary上の
`pg_stat_replication.replay_lsn`（各standbyが通知した再生位置）を25ms間隔で確認する。
期待する両IP・application_name・physical slotの組み合わせとstreaming状態も確認する。
両方のreplay位置が対象LSN以上になり、Primaryのidentity/timelineが変わっていない場合だけ成功する。
物理ストリーミングの同一system/timeline接続と、各ホストのrecovery状態は上記バックグラウンド確認でも検証する。
毎回のReplica再作成はしない。

`ISUREAL_REPLICA_INIT_TIMEOUT`は既定50秒、最大55秒。
DB初期化と待機を合わせた期限で、60秒制限に余裕を残す。
psqlはexecで起動し、初期化中のstatement_timeoutは45秒。
タイムアウト時に成功を返さず、停止・pause中のReplicaを利用可能にしない。
待機時間と総時間はjournaldに記録する。

## 実測

公式スコアは取得していない。Chromeの利用が承認されずポータルを操作できなかった。
既存の公式ベンチ相当ログ（13:22〜13:23 JST、11,127 requests、5xx 0、スコア未取得）はローカルの
`~/isucon-measurement/results/20261031T042148Z-ccaee3/`にある。
以下は独立した低負荷の診断であり、スコア比較や高負荷性能の代わりにはならない。
導入前には初回backupも並行していたため、特にisu02の比較にはその負荷が混ざる。

### COMMITのみ（Primary localhost、30回、ms）

| 構成 | P50 | P95 | P99 |
|---|---:|---:|---:|
| 導入前 | 1.172 | 1.319 | 1.367 |
| remote_apply＋ANY 2試験 | 103.218 | 103.992 | 104.273 |
| 最終：非同期 | 1.104 | 1.215 | 1.219 |

### API（各アプリのlocalhostから測定、ms）

| API / host | 導入前 P50/P95/P99 | 最終非同期 P50/P95/P99 |
|---|---|---|
| GET /api/me / isu02 | 398.433 / 445.187 / 453.316 | 202.922 / 203.517 / 204.077 |
| GET /api/me / isu03 | 304.375 / 305.075 / 305.785 | 203.162 / 203.712 / 204.357 |
| POST /api/auth/login / isu02 | 804.553 / 836.280 / 836.280 | 607.617 / 608.513 / 608.513 |
| POST /api/auth/login / isu03 | 608.721 / 609.542 / 609.542 | 608.049 / 608.890 / 608.890 |
| PATCH /api/me / isu02 | 795.038 / 835.989 / 835.989 | 506.546 / 607.523 / 607.523 |
| PATCH /api/me / isu03 | 608.510 / 609.460 / 609.460 | 507.105 / 607.871 / 607.871 |

GETの標本は61件、login/PATCHは各10件。小さい標本のP99は最大値に近い。
同期試験ではプロフィールGETがP50約0.97msだったが、認証もReplicaで読んでいた時の値であり、
**最終構成の性能ではない**。安全性と書き込み遅延の方針変更に伴い採用を取りやめた。

### 初期化と障害試験

- isu03のreplayを意図的にpause。Primaryの初期化COMMITが終了しisu02が追従しても、initializeは応答しないことを確認。
- resume後に200。総時間11.090秒、うちReplica待機6.582秒。
- 成功直後に3リージョンすべてで初期化前のセッションが401。
- 全DBでlogin_sessions=0、initial_usersとのdisplay_name/status差分=0、Replicaはrecovery=true。
- 最終非同期構成でisu02のPostgreSQLを停止。GETはPrimaryへfallbackして200、PATCHも200（約0.810秒）。
- 再開後は通常WALで追従し、バックグラウンド確認後にReplica読み取りへ復帰。
- 同期試験では同じ停止に対して書込みが約10.203秒で500、結果不明として扱った。最終構成ではこの同期待ちはない。
- 4台のOSを順に再起動し、boot ID変更とサービス自動復旧を確認。再起動後、Primaryと両Replicaで診断テーブル90行・作成テストユーザー6行の一致を確認。診断テーブルは確認後に削除。
- 全4台のenvcheckが「全ての追試環境チェックをパスしました」。
- go test ./...、go test -race ./...が成功。ローカルHTTPテストに必要な権限で実施。

最終バイナリの低負荷診断では各Replicaでプロフィール本体SELECTが60回、fallback 0回。
対象となるプロフィール本体は120/120回（100%）ローカルで取得した。
認証・IP banは各リクエストでPrimary経路を使うため、これは全SQLのローカル率ではない。
実負荷での全APIローカル率は未測定。対象外GETは設計どおりPrimaryである。
Replica停止試験の2回のGETは両方Primaryへfallbackした。

通常の書込みを含む20回ずつのWALサンプルで、replay_lagはisu02が101.846〜102.142ms、
isu03が101.886〜102.203ms。20回の瞬間サンプルで両方の未再生差分は0〜32バイトだった。
これは非同期の実測であり、反映完了の保証や遅延の上限ではない。

追加の期限試験では、isu03をpauseし、isu01だけ一時的に期限を10秒へ短縮した。
initializeは10.001秒で500、待機8.980秒。isu03はhealthy=falseのままで、成功扱いしなかった。
試験後にreplayを再開し、一時設定を削除して通常の50秒へ戻した。
最後の通常initializeは200、総時間2.621秒、Replica待機1.605秒。診断ユーザーは初期化で削除した。

WAL遅延・資源・ローカル取得率のスナップショットは`replica-results/`の結果ファイルを参照。
`sar`のCPU・メモリ・disk I/Oとpostgres各プロセスのCPU/RSSを取得したが、低負荷・待機時間を含む。
高負荷時のReplica CPU/メモリ/I/Oや全APIのP50/P95/P99は未評価。

## 規約の根拠

`go/manual.md`の固定リージョン、ルーター経由、迂回禁止を確認した。
ユーザーからのClarification #20・#55（Private IPは既存VPC経路なら可、ルーター自身への接続は禁止）と
#32（initializeは60秒、basebackupは事前実施）を適用した。
AWSのread-only APIでサブネットに対応するルートテーブルを確認：

- europe: `rtb-047ffb9f9c6a01b34`
- america: `rtb-06b34c9fd65e50add`
- asia DB: `rtb-0a2606702962f4733`
- asia app: `rtb-0fb13c51d2f583fea`

各相手サブネットの/24ルートは競技ルーターinstance `i-07c25404259417e3e`を指す。
isu02のtracepathもルーターを経てisu04へ到達し、約100.9msを示した。
ルーターへのログイン・宛先接続・設定変更、VPCルートやSG変更、追加サーバー利用は行っていない。
PostgreSQLの物理レプリケーションもこの既存TCP/5432経路で実稼働を確認した。

## 再構築・保守・バックアップ

- `replica-primary.sh`＋`.sql`：Primaryの前提を検査、専用role、限定したpg_hba、スロット、WAL保持設定。
- `replica-enable.sql`：最終の非同期設定。
- `replica-seed.sh isu02|isu03`：空のデータディレクトリだけにbasebackup。既存データがあれば拒否する。自動削除しない。
- `replica-deploy.sh BINARY`：既存バイナリ・init.shを保存して3アプリへ順次配置。画像設定は変更しない。
- `replica-mode.sh primary-only|async-profile`：全アプリの読み取り切り替え。通常はasync-profile。
- `api-probe.py`、`replica-consistency.py`、`replica-failure.py`、`commit-probe.go`：診断用。テストユーザー等を作るため、本番の負荷走行と同時実行しない。

Primaryの`max_slot_wal_keep_size=4GB`。停止ReplicaのためにWALを無制限保持しない。
これはcheckpoint時に適用される上限で、一時的に超過することがある。
`pg_replication_slots`の`wal_status`/`restart_lsn`、`pg_wal_lsn_diff`、ディスク空きを監視する。
スロットがlostになった場合は手動で該当アプリをPrimaryへ戻し、停止Replicaのデータ保存・削除許可と容量を確認し、
inactiveスロットを再作成してseedする。初期化のための再seedは不要。

認証情報は既存envから設定し、Gitや結果ファイルには保存していない。
standbyのpassfileはpostgres所有・0600。診断用の生成資格情報は各サーバーの
`/home/isucon/.replica-validation/`（0700、各ファイル0600）に保存する。
initializeで診断ユーザーは消えるため、次の診断は新規fixtureから開始する。

Replicaは独立したバックアップではない。この作業では新規の独立DBバックアップは取得していない。
独立backupを追加する場合は容量・別保存先を確認し、[公式pg_basebackup](https://www.postgresql.org/docs/16/app-pgbasebackup.html)によるバックアップ／復元検証を別途行う。
Primaryの既存データディレクトリは削除・作り直ししていない。

## ロールバック

1. ローカルで`bash go/deploy/replica-mode.sh primary-only`。全アプリがPrimaryを使い、同期quorumは無効のまま。
2. 必要なら各アプリの`/home/isucon/webapp/go/isureal.before-replicas-20261031T045638Z`を`isureal`へ復元し、対応する`sql/init.before-replicas-20261031T045638Z.sh`も復元してGoを再起動。
3. 旧版を使う場合はReplica用systemd drop-inを外す。初期化待機も旧動作に戻るため、Replicaは読み取りに使用しない。
4. Replicaを廃止するならPostgreSQLを停止・自動起動を無効化してから、Primaryのinactive物理スロットを削除する。Primaryは停止・削除しない。
5. 保持上限を元へ戻すなら`ALTER SYSTEM RESET max_slot_wal_keep_size`＋reload。追加したreplication用pg_hbaの2行と専用roleも、不要と確認してから削除する。

残作業は公式ベンチマークによるスコア・全API分位点・高負荷資源の比較と、プロフィールの反映遅延が競技の検証に許容されるかの確認。
非同期でも厳密な最新値が必要なら、プロフィールもPrimaryへ戻す。
