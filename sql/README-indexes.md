# クエリ用インデックス

新規DBでは `001-schema.sql` に5個のインデックスを定義している。既存DBへはスキーマファイルを再実行せず、以下を使う。接続先は `PGHOST`、`PGPORT`、`PGUSER`、`PGDATABASE` 等で指定する。

```sh
# 読み取りのみ。deletedを含む全ユーザーの重複を確認する。
psql -X -v ON_ERROR_STOP=1 -f sql/check-username-duplicates.sql
# 本番適用は運用者が実行する。-1 / --single-transaction は指定しない。
psql -X -v ON_ERROR_STOP=1 -f sql/add-query-indexes.sql
```

適用SQLはpsql専用で、autocommitで各 `CREATE INDEX CONCURRENTLY IF NOT EXISTS` を実行する。既存トランザクションの中から読み込まない。username重複があれば一覧を出して終了コード1で停止し、データは変更しない。事前確認と作成の間の競合はUNIQUE作成自体が検出する。登録時のテーブルロック削除など、アプリケーションの変更は今回含まない。

適用前後に以下で既存定義と有効性を確認する。`IF NOT EXISTS` は同名オブジェクトの定義・有効性を保証しない。他名の同等インデックスが既にある環境では、不要な重複を作らないよう定義を照合して適用対象を調整する。

```sql
SELECT t.relname AS table_name, c.relname AS index_name,
       i.indisvalid, i.indisready, pg_get_indexdef(c.oid) AS definition
FROM pg_index i
JOIN pg_class c ON c.oid = i.indexrelid
JOIN pg_class t ON t.oid = i.indrelid
WHERE i.indrelid IN ('users'::regclass, 'posts'::regclass,
                    'realmojis'::regclass, 'friendships'::regclass)
ORDER BY t.relname, c.relname;
```

CONCURRENTLYの中断・重複エラー後には無効なインデックスが残り得る。同名の無効インデックスを `IF NOT EXISTS` で飛ばして成功扱いにしないこと。原因を確認し、対象が今回の無効インデックスであることを確認した運用者が、トランザクション外で `DROP INDEX CONCURRENTLY 対象名;` を実行してから再適用する。データの修正は別途判断する。

| インデックス | 用途 |
| --- | --- |
| `posts_active_image_author` | 未削除投稿をimage_idとauthor_idで探索 |
| `realmojis_active_image_reactor` | 有効なリアクションをimage_idとreactor_idで探索。post_idをINCLUDEして投稿への結合を補助 |
| `friendships_high_low` | OR条件のuser_high_id側。user_low_id側は既存UNIQUE(user_low_id,user_high_id)を利用 |
| `users_username_unique` | 全statusでusernameの一意性を保証し、存在確認・完全一致検索を補助 |
| `users_active_username_id` | activeユーザーの(username,id)順の走査・カーソル条件を補助 |

`imageAccessQuery` の投稿・リアクション両方の所有者条件と部分インデックスの条件は一致している。既存の投稿author_id、リアクションpost_id等のインデックスは別用途のため維持する。INCLUDEによるIndex Only Scanにはvisibility mapの状態も影響する。

## 実測手順

この変更では実DBへ接続しておらず、重複の有無・実行計画・改善率は未検証。添付ログ自体もリポジトリ内では確認していない。適用前後で同じデータとパラメーターを使い、Execution Time、Buffers、実行行数、走査方式を比較する。画像はプロフィール画像・投稿画像・RealMoji画像をそれぞれ選ぶ。CASEで先の分岐が成立すると後続は実行されないため、RealMoji分岐が実行された計画も確認する。

インデックス作成後や大量データ投入・initialize後、統計が古い場合は次を運用者が実行する（initializeごとの自動実行は追加していない）。

```sql
ANALYZE posts;
ANALYZE realmojis;
ANALYZE friendships;
ANALYZE users;
```

```sh
psql -X -v ON_ERROR_STOP=1 \
  -v image_id='実在する画像ID' \
  -v friend_id_1='ユーザーID1' -v friend_id_2='ユーザーID2' \
  -v viewer_id='閲覧者ID' \
  -v before_username='' -v before_id='' \
  -v username='実在するusername' \
  -f sql/explain-query-indexes.sql
```

ユーザー検索は初回（空カーソル）と途中の実在するusername/idの両方で確認する。検証SQLはGoのSELECT列・条件・ORDER BYを維持し、画像可視性SQLも `go/image_api.go` と同じ条件を使う。友人関係は実際のINリスト長・対象ユーザーの偏りも再現する。小規模データや選択性が低い場合はSeq Scanが選ばれても異常とは限らない。インデックス利用を強制する設定は行わない。

ユーザー検索はLIMITなしでactiveユーザーを取得し、正規化したusername/display_nameの部分一致をGo側で判定する。今回のインデックスだけではその取得量や部分一致処理は削減できず、プランによって改善が限定的になる。画像のimage_objects探索や他の結合も今回の追加だけで全て改善するとは限らない。

## initializeとの関係

`go/initialize.go` → `sql/init.sh` → `sql/reset.sql` を確認済み。resetはTRUNCATE / DELETE / UPDATE / INSERTによるデータ復元で、今回のインデックスをDROP / CREATEしない。TRUNCATE後も定義は保持される。新規構築用 `001-schema.sql` はテーブルを作り直すが、ベンチマークのinitializeからは呼ばれない。
