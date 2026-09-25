# container-go

English (primary): [README.md](README.md)

[Apple Container](https://github.com/apple/container) と Docker を実行基盤とする、
[testcontainers](https://testcontainers.com/) スタイルの Go ライブラリです。
Go のテストから使い捨てコンテナを起動できます。サードパーティ依存はゼロです。

```go
func TestRedis(t *testing.T) {
    ctx := context.Background()

    ctr, err := container.Run(ctx, "redis:7-alpine",
        container.WithExposedPorts("6379/tcp"),
        container.WithWaitStrategy(wait.ForListeningPort("6379/tcp")),
    )
    container.Cleanup(t, ctr) // nil 安全。テスト終了時にコンテナを削除
    if err != nil {
        t.Fatal(err)
    }

    endpoint, err := ctr.Endpoint(ctx, "6379/tcp") // 例: "192.168.64.3:6379"
    if err != nil {
        t.Fatal(err)
    }
    _ = endpoint // endpoint へクライアントを接続
}
```

## バックエンド

| OS | 既定バックエンド | 要件 |
|---|---|---|
| macOS | Apple Container | macOS 26+、Apple Silicon、[Apple Container](https://github.com/apple/container) 1.2.x(`container system start` 実行済み) |
| Linux | Docker | docker CLI + 稼働中のデーモン |
| Windows | Docker | docker CLI + 稼働中のデーモン(watchdog リーパーなし。後述) |

macOS で Docker(Docker Desktop など)を使う場合は
`CONTAINERGO_BACKEND=docker` を、Apple Container を明示する場合は
`CONTAINERGO_BACKEND=apple` を設定します。

本ライブラリはバックエンドの CLI(`container` / `docker`)を子プロセスとして
呼び出します。cgo 不要、デーモン API クライアント不要です。`DOCKER_HOST`、
コンテキスト、レジストリ認証は docker CLI 自身が解決します。

Go 1.23 以上が必要です。

動作確認済みバックエンド(本ライブラリがマッチする CLI stderr 文言と
inspect JSON 形状):

| バックエンド | 確認済みバージョン |
|---|---|
| Apple Container | 1.2.x–1.3.x |
| Docker Engine / CLI | 29.x |

新しい CLI ではエラー文言や JSON フィールドが変わる可能性があります。
`engine_apple.go` / `engine_docker.go` 先頭の stderr マッチャと、
`internal/inspect/testdata/`・`testdata/` のフィクスチャを参照してください。

## インストール

```
go get github.com/hirokazumiyaji/container-go@v0.2.0
```

## API の安定性

1.0 未満では、マイナーリリース(0.x)に破壊的変更が含まれる場合があります。
再現可能なビルドにはモジュールバージョンを明示的に pin してください
(`go get ...@v0.2.0`)。

ルートの `container` パッケージ(`Run`、オプション、ライフサイクルヘルパー)が
主な統合面で、マイナーシリーズ内では比較的安定を目指します。`wait`
パッケージも公開 API ですが、カスタム戦略向けのインターフェース —
特に [`wait.Target`](wait/wait.go) — はバックエンドやプローブ要件の変化に
応じて変更される可能性があります。可能なら組み込み戦略を使ってください。

リリースノートは [CHANGELOG.md](CHANGELOG.md) を参照してください。

## 接続エンドポイント

**Apple Container バックエンド**: 各コンテナは vmnet の `default`
ネットワーク上に実 IP を持ち、ホストから直接到達できます。既定ではこの IP
を使います。

- `Host` はコンテナ IP、`MappedPort` はコンテナポートそのまま、`Endpoint`
  はその組み合わせを返します。
- ホストポートを消費しないため、並列テストがポールで衝突しません。

特定の publish ポートを使うときは `Endpoint`(または既知ホスト +
`MappedPort`)を優先してください。複数 publish の host-IP が異なる場合、
`Host` は先頭の publish のアドレスだけを返します。

**Docker バックエンド**: コンテナ IP にはホストから届かないことが多いため
(Docker Desktop)、`WithExposedPorts` で宣言したポートはデーモンが割り当てる
ランダムポートへ自動公開されます(testcontainers と同じモデル)。ローカルはループバック(`-p 127.0.0.1::<port>`)、リモートデーモン(`DOCKER_HOST=tcp://host`)では全IF(`-p 0.0.0.0::<port>`)に束縛します。`Host` は `127.0.0.1`(`tcp://` の `DOCKER_HOST` 設定時はそのホスト)、`MappedPort` は割り当てられたポートを返します。リモートデーモンでは、ループバック(`127.0.0.1:...`、`[::1]:...`)を明示した `WithPublishedPort` はリモート側でしか待ち受けられないため拒否します。`docker context` 経由のリモート指定は検知しません。割り当ては
デーモンが起動時に原子的に行うため、こちらでも並列テストがポートを
奪い合うことはありません。

クライアントが `localhost` を要求する場合(または構成上コンテナ IP に
届かない場合)は、明示的に公開します。

```go
ctr, err := container.Run(ctx, "nginx:alpine",
    container.WithExposedPorts("80/tcp"),
    container.WithPublishedPort("127.0.0.1:18080:80"),
    container.WithWaitStrategy(wait.ForHTTP("/")),
)
// Host → "127.0.0.1"、MappedPort("80/tcp") → 18080
```

`MappedPort` と `Endpoint` が解決できるのは `WithExposedPorts` で宣言した
ポート(または公開済みポート)だけです。

## 待機戦略

Apple Container にはヘルスチェックも wait コマンドもないため、`wait`
パッケージがクライアント側で readiness を判定します。

```go
wait.ForLog("Ready to accept connections")   // 部分一致。.AsRegexp()、.WithOccurrence(n)
wait.ForListeningPort("6379/tcp")            // TCP 接続成功まで
wait.ForExposedPort()                        // 最初に宣言したポート
wait.ForHTTP("/health")                      // .WithPort、.WithMethod、.WithStatusCodeMatcher、.WithHeaders/.WithHeader、.WithBasicAuth、.WithTLS/.WithTLSConfig/.WithHTTPClient
wait.ForExec([]string{"pg_isready"})         // .WithExitCodeMatcher
wait.ForAll(wait.ForExposedPort()), wait.ForAny(wait.ForExposedPort()) // 合成。.WithStartupTimeout
```

基本戦略の起動タイムアウトは既定 60 秒です。
`ForListeningPort`、`ForExposedPort`、`ForHTTP` は既定 100 ミリ秒、
`ForExec` は既定 250 ミリ秒でポーリングします。`ForLog` は
`FollowLogs` のストリームを継続して読むため、`WithPollInterval` の
セッターは動作を変更しません。`ForAll` と `ForAny` には
`WithPollInterval` がなく、`WithStartupTimeout` で合成全体に位置を
設定します。子戦略の設定はそのまま使われます。待機中にコンテナが
停止すると即座に失敗します。新規作成し再利用していないコンテナで
待機が失敗した場合はロールバック削除し、エラーにログ末尾を添付します。
再利用コンテナは、後述の共有ライフタイムの終わりまで残します。

## ログ

`Logs` と `LogsWithOptions` は有限のスナップショットを返します。
`LogsWithOptions` は `LogsOptions{Tail, Since}` でスナップショットを
制限できます。`FollowLogs` はストリーミングする `io.ReadCloser` を
返し、`Close` するか context をキャンセルするとバックエンド CLI を
停止します。`ForLog` は `FollowLogs` を使い、`Logs` は新しい出力を
追尾しません。

## イメージの pull

コンテナを新規作成する必要がある場合、`Run` は起動前に明示的な
pull policy を適用します。

- `PullMissing`（既定値）はローカルストアを検査し、イメージがない
  ときだけ明示的に pull します。
- `PullAlways` は `Run` の呼び出しごとに明示的な pull を要求します。
- `PullNever` は検査だけを行い、イメージがない場合は起動前に
  `ErrImageNotFound` を返します。

同じプロセスの並行処理は、バックエンド、イメージ、プラットフォーム、
操作の種類が同じ pull を共有します。待機中の呼び出し元がキャンセル
されても、共有 pull は残りの呼び出し元のために続きます。Docker
バックエンドは `docker run` に `--pull=never` を渡すため、CLI が
二重に pull することはありません。Apple Container でも、このライブラリ
も同じ明示的な policy 経路を使う。

```go
container.Run(ctx, "redis:7-alpine",
    container.WithPullPolicy(container.PullAlways))
// container.PullNever: イメージがない場合は起動前に失敗
// (errors.Is(err, container.ErrImageNotFound))

container.Pull(ctx, "redis:7-alpine") // 明示的な取得。Run と同様に共有
```

## クリーンアップの契約

コンテナがテストより長生きしないよう、3 層の仕組みがあります。

1. `container.Cleanup(t, ctr)` は `t.Cleanup` 経由で削除を登録します。
   defer 派には `container.TerminateContainer(ctr)` があります。どちらも
   nil 安全なので、`Run` のエラーチェックより前に呼べます。
2. 再利用でない経路で `Run` が途中で失敗した場合は、作成済みリソースの
   削除を試してから返し、削除に失敗した場合はそのエラーを報告します。
3. 実際の CLI コンテナを登録すると、外部 `/bin/sh` 子プロセスである
   best-effort の watchdog リーパーを遅延起動します。親プロセスの
   パイプが閉じられると（パニックや SIGKILL を含む）、登録済みコンテナ
   の強制削除を試みます。Windows では利用できず、起動や削除の失敗を
   成功したクリーンアップとして扱いません。

`CONTAINERGO_KEEP=1` は `Cleanup`、`TerminateContainer`、reaper の
登録を省略します。明示的な `ctr.Terminate` と、失敗した `Run` の
ロールバックはコンテナを削除することがあります。

`container.Prune(ctx)` は過去セッションを含め、本ライブラリが作成した
停止済みコンテナ（`com.github.hirokazumiyaji.container-go` ラベル付き）
を削除します。実行中コンテナは削除しません。

## Reuse(テスト / プロセス間でのコンテナ共有)

`WithReuse` は安定した `WithName` に対する get-or-create です。同一
プロセス内の並列呼び出しや、同じホストの別プロセスの `go test`
パッケージが 1 つのコンテナを共有します。

```go
ctr, err := container.Run(ctx, "redis:7-alpine",
    container.WithName("it-redis"),
    container.WithReuse(),
    container.WithReuseGroup("integration"),
    container.WithExposedPorts("6379/tcp"),
    container.WithWaitStrategy(wait.ForListeningPort("6379/tcp")),
)
container.Cleanup(t, ctr) // reused ハンドルでは何もしない
```

契約:

- `WithName` 必須。待機戦略は attach 時も必ず再実行する。
- 既存コンテナは `WithReuse` で作成され、互換性のあるイメージである
  必要がある。競合する create の名前衝突は成功として扱い、既存へ
  attach する。
- stopped の残骸は削除して再作成する。running のまま ready にならない
  場合は削除せずエラーを返す。
- image / port が既存と不一致なら分かりやすいエラーを返す。互換性
  チェックは image と port のみが対象。`env` / `cmd` / `mounts` の差は
  既存へ黙って attach する仕様。
- 各作成は世代ラベルを持ち、`Terminate` と stopped 再作成経路は
  置き換わった世代の削除を拒否する。watchdog リーパーも同様に
  ガードする。名前ベースのガード保証は、同じホストで本ライブラリを
 使うプロセス間の協調に限られる。外部 CLI による delete / 再作成は
  この保証の対象外。
- `Cleanup` / `TerminateContainer` / watchdog リーパーは reused ハンドルを
  削除しない。明示的な `ctr.Terminate` だけが共有コンテナを消し得る。
- `container.PruneReuseGroup(ctx, "integration")` はそのグループの
  コンテナを強制削除する(CI 終了時)。グループは再利用キーではなく
  ラベルである。通常の `Prune` は停止済み管理対象だけを削除する。

ライブラリはテスト間のアプリケーションデータを自動初期化しません。
キー接頭辞、スキーマ分離、`Exec` による reset(`FLUSHALL` 等)を使って
ください。

## セキュリティ上の注意

- すべての CLI 呼び出しは argv 配列で行い、シェルを経由しません。唯一の
  シェルスクリプト(リーパー)は固定文字列で、コンテナ ID は検証済みの
  stdin データとしてのみ渡ります。
- 環境変数はパーミッション 0600 の一時 env ファイル経由で渡すため、秘密が
  プロセス一覧(`ps`)に現れません。
- レジストリ認証情報は本ライブラリでは扱いません。Apple Container では
  `container registry login`、Docker では `docker login` を使い、
  認証情報とレジストリコンテキストはバックエンド CLI が管理します。

## testcontainers-go との違い

非対応(相当機能が存在しない、またはスコープ外):

| testcontainers-go | 本ライブラリ |
|---|---|
| `wait.ForHealthCheck` | 相当機能なし。`ForLog`/`ForExec`/`ForHTTP` を使用 |
| Dockerfile からのビルド | スコープ外(`container build` / `docker build` を直接使用) |
| Ryuk リーパーコンテナ | ローカルの watchdog リーパープロセスで代替 |
| ランダムホストポートマッピング | Apple バックエンドはコンテナ IP へ直接接続。Docker バックエンドはループバックのランダムポートへ自動公開 |
| ネットワーク / ボリュームの作成とライフサイクル管理 | 当面スコープ外。`WithNetwork` は既存ネットワークへ接続し、`WithMounts` はマウント指定を受け取る |
| `GenericContainerRequest.Reuse` | `WithReuse` + `WithName`: プロセス間 get-or-create。再待機必須、Cleanup/リーパーは所有しない |

## 開発

```
make test                # ユニットテスト(バックエンド不要)
make vet
make integration         # 統合テスト(bench/singleflight 除外)。バックエンドがなければ各自 skip
make integration-docker  # Docker バックエンドの統合テストのみ
make bench-integration   # pull が多い bench / singleflight
```

統合テストのイメージは Docker Hub 匿名 pull 制限を避けるため
`public.ecr.aws/docker/library/...` を使います。
`CONTAINERGO_BACKEND=apple` または `docker` で片方だけ実行できます。

GitHub Actions は `ubuntu-latest` で Go 1.23.0 と安定版 Go を使い、
ユニットテスト、race テスト、lint、`govulncheck` を実行します。
同じ runner で Docker 統合テストの行列も実行します。Apple Container
統合テストは、GitHub ホストランナーにサービスがないためローカルで
実行します。integration タグなしの `examples/compile_test.go` は
ドキュメント中の API 呼び出しを型検査し、タグ付き example が実
バックエンドで動作する役割を担います。

設計ドキュメント: [docs/design.md](docs/design.md)(日本語版:
[docs/design.ja.md](docs/design.ja.md))。
設計ドキュメントの「実装フェーズ」は履歴であり、現在の API 契約では
ありません。

コントリビューション: [CONTRIBUTING.md](CONTRIBUTING.md)。セキュリティ報告:
[SECURITY.md](SECURITY.md)。

## ライセンス

MIT
