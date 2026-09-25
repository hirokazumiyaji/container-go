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

    endpoint, _ := ctr.Endpoint(ctx, "6379/tcp") // 例: "192.168.64.3:6379"
    // ... endpoint へクライアントを接続
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
wait.ForHTTP("/health")                      // .WithPort、.WithMethod、.WithStatusCodeMatcher、.WithHeaders、.WithBasicAuth、.WithTLS/.WithHTTPClient
wait.ForExec([]string{"pg_isready"})         // .WithExitCodeMatcher
wait.ForAll(...), wait.ForAny(...)           // 合成; .WithStartupTimeout
```

すべての戦略は `WithStartupTimeout`(既定 60 秒)と `WithPollInterval`
(既定 100 ミリ秒)を持ちます(`ForAll` / `ForAny` は `WithStartupTimeout` で合成全体のタイムアウトを設定可)。待機中にコンテナが停止すると即座に失敗し、
待機に失敗した場合はロールバック削除のうえ、エラーにログ末尾が添付されます。

## クリーンアップの契約

コンテナがテストより長生きしないよう、3 層の仕組みがあります。

1. `container.Cleanup(t, ctr)` は `t.Cleanup` 経由で削除を登録します。
   defer 派には `container.TerminateContainer(ctr)` があります。どちらも
   nil 安全なので、`Run` のエラーチェックより前に呼べます。
2. `Run` が途中で失敗した場合は、安全だと確認できる場合は `Run` 自身が
   作成済みリソースを削除してから返ります。削除に失敗した場合は、返された
   `*CleanupError` から元の error と cleanup error の両方を取得できます。
   failed create が既に running の reuse generation を残している場合、
   peer が採用している可能性があるため自動削除せず、保持された generation を
   CleanupError として報告します。`CONTAINERGO_KEEP=1` では、所有者を確認
   できた partial handle を error とともに返すので、明示的に確認・終了できます。
3. watchdog リーパー(外部の `/bin/sh` 子プロセス)が、テストプロセスが
   どのように死んでも(SIGKILL やパニックを含む)登録済みコンテナを強制
   削除します。Docker の full 64-hex ID と Apple の name のどちらも登録
   対象です。リーパーは `/bin/sh` を必要とするため Windows では動かず、
   Windows では前 2 層のみでクリーンアップします。Apple の name lock を
   取得できない場合、reaper は協調しない unlocked delete を行わず entry を
   skip します。

補足:

- `CONTAINERGO_KEEP=1` でコンテナを残せます(デバッグ用)。確認済みの
  failed create には partial handle policy も適用されます。
- `container.Prune(ctx)` は過去セッションを含め、本ライブラリが作成した
  停止済みコンテナ(`com.github.hirokazumiyaji.container-go` ラベルと有効な
  creation generation 付き)を削除します。Apple backend では stable な
  name lock 上で list candidate を再確認してから削除します。

backend probe も失敗した場合、返された error は
`errors.Is(err, container.ErrSystemNotRunning)` と、元の command failure
に対する `errors.As(err, *container.CLIError)` の両方を満たします。

## Reuse(テスト / プロセス間でのコンテナ共有)

`WithReuse` は安定した `WithName` に対する get-or-create です。同一
プロセス内の並列呼び出しや、別プロセスの `go test` パッケージが 1 つの
コンテナを共有します。

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
- 競合する create の名前衝突は成功として扱い、既存へ attach する。
- stopped generation は managed、reuse、有効な generation label を
  確認できた場合だけ削除・再作成する。failed create が running の
  generation を残した場合、peer が採用している可能性があるため自動削除
  せず、`CleanupError` として保持中の generation を報告する。
  `CONTAINERGO_KEEP=1` では所有者を確認した partial handle を返す。
  running のまま ready にならない場合も削除せずエラーになる。
- image / port が既存と不一致なら分かりやすいエラーを返す。
  既存の reuse container には managed、reuse、有効な creation generation label が必要で、欠ける場合は fail closed する。
  `PullAlways` は reuse を解決する前にローカル image を更新し、container が既に running でも pull failure を隠さない。
  pull 後に running container の image が一致しなければ、共有 container を再作成せずエラーにする。digest を指定している場合は digest 不一致も含む。
  mutable tag はローカル store を更新するが、既に running の共有 container は置き換えない。厳密な image identity が必要なら digest を固定する。
  `PullMissing` と `PullNever` は attach 専用に image を fetch しない。
  `WithPlatform` は OS・architecture・variant を field ごとに比較する。
  Apple inspect の OCI descriptor digest も image identity として保持する。
  `WithReuseGroup` は新規作成・再作成時の tag 付けのみで、reuse の compatibility key には含めない。
- `WithFiles` は shared ensure flight の後、create leader も含めた reuse の
  全 caller で copy する。leader の copy failure が成功した waiter を
  poison することはない。Docker は inspect した immutable UID を copy
  target に使い、Apple は stable な per-name lock 内で generation を確認
  する。identity 確認から copy の間に replacement があれば fail closed に
  する。copy failure は shared container を削除せずにエラーにする。
  同じ path への copy は shared state の変更なので、並行 caller は競合する
  target を避ける。
  `WithEnv` / `WithCmd` / `WithEntrypoint` / `WithLabels` / `WithMounts` /
  `WithCPUs` / `WithMemory` / `WithUser` / `WithWorkingDir` / `WithNetwork` は
  作成専用 option として attach 時に意図的に無視する。必要な場合は別 name
  を使う。
- Apple の create、generation-checked delete、prune は、協力する host 上の
  process 間で stable な per-name lock を使う。prune candidate は lock 内で
  再 inspect してから削除する。直接の `container` CLI 呼び出しや別の
  implementation は lock を使わないため、この保証の対象外。
- 各作成は世代ラベルを持ち、`Terminate`、stopped reuse、prune、watchdog
  リーパーは置き換わった世代の削除を拒否する。
- `Cleanup` / `TerminateContainer` / watchdog リーパーは reused ハンドルを
  削除しない。明示的な `ctr.Terminate` だけが共有コンテナを消し得る。
- `container.PruneReuseGroup(ctx, "integration")` はそのグループの
  コンテナを強制削除する(CI 終了時)。通常の `Prune` は Apple では
  lock 上で candidate を再確認しつつ stopped のみを削除する。

ライブラリはテスト間のアプリケーションデータを自動初期化しません。
キー接頭辞、スキーマ分離、`Exec` による reset(`FLUSHALL` 等)を使って
ください。

## セキュリティ上の注意

- すべての CLI 呼び出しは argv 配列で行い、シェルを経由しません。唯一の
  シェルスクリプト(リーパー)は固定文字列で、コンテナ ID は検証済みの
  Apple name または Docker の full 64-hex ID として stdin に渡します。
  Apple の name-addressed reaper エントリは library cleanup と同じ
  stable lock を使います。
- 環境変数はパーミッション 0600 の一時 env ファイル経由で渡すため、秘密が
  プロセス一覧(`ps`)に現れません。
- レジストリ認証情報は本ライブラリでは扱いません。`container registry
  login`(macOS Keychain 保存)を使ってください。

## testcontainers-go との違い

非対応(相当機能が存在しない、またはスコープ外):

| testcontainers-go | 本ライブラリ |
|---|---|
| `wait.ForHealthCheck` | 相当機能なし。`ForLog`/`ForExec`/`ForHTTP` を使用 |
| Dockerfile からのビルド | スコープ外(`container build` / `docker build` を直接使用) |
| Ryuk リーパーコンテナ | ローカルの watchdog リーパープロセスで代替 |
| ランダムホストポートマッピング | Apple バックエンドはコンテナ IP へ直接接続。Docker バックエンドはループバックのランダムポートへ自動公開 |
| ネットワーク / ボリューム管理 API | 当面スコープ外 |
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

設計ドキュメント: [docs/design.md](docs/design.md)(日本語版:
[docs/design.ja.md](docs/design.ja.md))

コントリビューション: [CONTRIBUTING.md](CONTRIBUTING.md)。セキュリティ報告:
[SECURITY.md](SECURITY.md)。

## ライセンス

MIT
