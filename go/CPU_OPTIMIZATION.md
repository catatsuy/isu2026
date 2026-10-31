# Go CPU最適化の検証

2026-10-31、Go 1.26.5 / darwin arm64 / Apple M1。
DBスキーマ・HTTP仕様・他言語実装は変更していない。

## 実装

- 画像署名: 呼び出しごとにHMACを1個生成し、Resetと32バイトのSumバッファを再利用。1000回の計算は維持。共有状態は追加していない。
- レプリケーション: 起動時は256シャードを走査して永続マーカーを回収。その後はSave/完了通知で変更されたシャード、または最短リトライ期限を迎えたシャードだけを走査。空キューでは通知待ちとなり、1秒の定期走査を廃止。ディスクのキュー、4ワーカー、fsync、再試行、終了時のキャンセルを維持。メモリの管理情報は256シャードと実行中ワーカー分に制限。ディレクトリ読み取り失敗は1秒後に再試行し、リトライ日時の保存失敗時にもキャンセル可能な待機でビジーループを防止。
- 推薦: 100候補以下は従来の安定ソート。それより多い場合はカーソル適用後の上位51件を固定サイズのヒープで選択。同点は元のSQL候補順を維持。スコア計算では友達の反応数の中間mapを省略。返却しない51件目のレスポンスと画像署名も生成しない。post.goの友達・友達の友達の集合、SQL認可条件は既存処理を維持。
- JPEG: 3エンドポイントすべて完全デコードを維持。DecodeConfigで通る切断JPEGがDecodeで拒否される回帰テストを追加。post/realmojiのmultipartはパーサーが確定したサイズで1回だけ確保して読み込み、ReadAllの成長コピーを省略。profile-imageはリクエストBodyを従来どおり読み込み、未検証のContent-Lengthに基づく確保は追加していない。既存実装に重複デコードはない。
- 検索: ASCIIの正規化をToLowerに短絡し、それ以外は従来のNFC→fold→NFCを維持。ブロック済みユーザーは正規化前に除外。クエリの正規化は既存どおり1回。更新・初期化の無効化が必要な永続キャッシュは導入していない。
- IP Ban: 200msのDB確認間隔とatomic snapshotを維持。TryLock獲得後にsnapshotを再確認し、直前に完了したリロードを重複実行しない。一覧が不変なら既存の不変mapを共有。更新時は新しいmapを公開し、読み取り中のmapを書き換えない。定常状態でのDB取得頻度そのものは維持。
- ログ: デフォルトerror。正常時は既存の早期returnでログJSONを生成しない。5xxの詳細をアクションログに集約し、ミドルウェア外ではAPIErrorHandlerのエラーログを維持。想定内の認証ヘッダー不正Warnを削除。レプリケーションのpeer別エラーを集約し、最初とバックオフ上限到達後の試行で詳細を出力。
- SSE: 500msの共有ポーリング、購読者の有効性確認、通知・終了処理を維持。ループ専用のsubscriber snapshot、ID slice、重複排除mapを再利用。SQL中はロックを保持せず、接続ごとのポーリングやgoroutineは追加していない。

## ベンチマーク

変更前の処理をテスト内に保持して同じ入力で比較。各3回の中央値。ns/opはマイクロベンチマークの処理時間で、HTTPレイテンシではない。

| 処理 | ns/op 前→後 | B/op 前→後 | allocs/op 前→後 |
|---|---:|---:|---:|
| 画像署名 | 328,665 → 97,526 | 約512,042 → 776 | 6,002 → 10 |
| 推薦選択・50候補 | 17,704 → 16,919 | 424 → 424 | 3 → 3 |
| 推薦選択・1,000候補 | 739,033 → 67,907 | 424 → 18,904 | 3 → 4 |
| 推薦選択・10,000候補 | 11,260,836 → 378,941 | 424 → 18,904 | 3 → 4 |
| 検索正規化・ASCII | 203.0 → 42.90 | 272 → 16 | 2 → 1 |
| 検索正規化・非ASCII | 205.2 → 204.2 | 272 → 272 | 2 → 2 |
| 1MiB multipart読み込み | 199,412 → 80,605 | 2,228,003 → 1,048,608 | 25 → 2 |
| 不変のIP Ban map・1,000件 | 18,562 → 11,304 | 98,432 → 0 | 6 → 0 |

推薦選択の計測は両方式で同じ候補sliceのコピーを含む。ヒープ方式では結果51件を保持するため約18KBの追加確保があるが、候補数には比例しない。IP Banはmap構築部分のみでSQL取得を含まない。multipartの計測は読み込み部分のみでJPEGデコードを含まない。

512×512のJPEG完全デコードは中央値1,244,799 ns/op、約407,107 B/op、6 allocs/op。デコード方式は変更していないため改善率は算出しない。

署名単体のCPUプロファイルも取得。変更後はSHA256 block処理がflat約35%、HMAC.Sumが累積約70%を占め、暗号計算が中心となる。これはアプリケーション全体のCPUプロファイルではない。診断用HTTPエンドポイントは追加していない。

再現コマンド（goディレクトリから）:

```sh
GOCACHE=/private/tmp/isu2026-go-cache go test ./...
GOCACHE=/private/tmp/isu2026-go-cache go test -race ./...
GOCACHE=/private/tmp/isu2026-go-cache go test -run '^$' -bench 'Benchmark(ImageSignature|FeedRanking|SearchNormalization|JPEGDecode|ImagePartRead|IPBanMap)$' -benchmem -count=3
GOCACHE=/private/tmp/isu2026-go-cache go test -run '^$' -bench 'BenchmarkImageSignature/after$' -benchtime=3s -cpuprofile=/private/tmp/isu2026-signature.cpu -o /private/tmp/isu2026-profile.test
GOCACHE=/private/tmp/isu2026-go-cache go tool pprof -top /private/tmp/isu2026-profile.test /private/tmp/isu2026-signature.cpu
```

## テストと計測の制限

`go test ./...`、`go test -race ./...`は成功。既存の画像認可・レプリケーションの再試行/再起動/複数ワーカー/並行Save/タイムアウト/Close、SSEテストと、追加した署名互換性・推薦順序/カーソル・Unicode正規化・JPEG切断・IP Ban snapshot・ログのテストを含む。

`ISUREAL_TEST_DATABASE_URL`未設定のため以下のPostgreSQL実DBテストはスキップ。ローカルDockerデーモンにも接続できなかった。

- TestFeedSQLPostgres
- TestImageAccessSQLPostgres
- TestPostingWindowSharedLocksPostgres
- TestConcurrentRealMojiReplacementPostgres
- TestConcurrentRegistrationPostgres
- TestRegistrationPostingWindowBoundaryPostgres

本番相当のDB・データ・ISUCONベンチマーカーがないため、ISUCONスコア、HTTPスループット、p50/p95/p99、アプリ全体のCPU使用率、アイドル時の実測システムコール数は未測定。これらの改善率は推測していない。
