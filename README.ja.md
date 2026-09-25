# container-go

English (primary): [README.md](README.md)

[Apple Container](https://github.com/apple/container) と Docker を実行基盤とする、
[testcontainers](https://testcontainers.com/) スタイルの Go ライブラリです。
Go のテストから使い捨てコンテナを起動できます。サードパーティ依存はゼロです。

## バージョンの適用範囲

最新のタグ付きリリースは `v0.2.0`（2026-09-02）です。
このチェックアウトは、そのタグより後の開発版です。
下記のインストールコマンドはリリース版 API のものです。
このコマンドで、後述の開発版 API が入るわけではありません。

現在のチェックアウトには Go 1.23 以降が必要です。
`v0.2.0` モジュールには Go 1.27 以降が必要です。

| API または動作 | `v0.2.0` | 現在の開発チェックアウト |
|---|---|---|
| `Run`、基本オプション、ライフサイクル、エンドポイント、`Exec`、`Logs`、`FollowLogs`、copy、pull policy、Reuse、Cleanup、バックエンド選択、元の wait 戦略 | あり | あり |
| `LogsOptions` と `LogsWithOptions` | なし | あり |
| `wait.ForHTTP` のヘッダー、認証、TLS、独自クライアント設定 | なし | あり |
| 公開型 `wait.AllStrategy` / `AnyStrategy` と合成 strategy の `WithStartupTimeout` | なし | あり |
| root の `CLIError`、`ErrContainerNotFound`、`ErrGenerationReplaced` | なし | あり |
| 世代安全な削除、`v0.2.0` 以降の endpoint 強化、現在のリーパー強化 | `v0.2.0` の契約には含まれない | あり。ただし後述の Docker reaper の前提条件がある |

以下の説明で「現在の開発」と記した節以外は、現行チェックアウトの動作を説明します。
タグ付き `v0.2.0` の API については、バージョン表を確認してください。
コード例は単独ファイルとしてリポジトリのドキュメントテストがコンパイルします。

```go
package docexample

import (
    "context"
    "testing"

    container "github.com/hirokazumiyaji/container-go"
    "github.com/hirokazumiyaji/container-go/wait"
)

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
| macOS | Apple Container | macOS 26+、Apple Silicon、[Apple Container](https://github.com/apple/container) 1.2.x–1.3.x、`container system start` 実行済み |
| Linux | Docker | docker CLI と稼働中のデーモン |
| Windows | Docker | docker CLI と稼働中のデーモン（watchdog リーパーなし。後述） |

macOS で Docker（Docker Desktop など）を使う場合は
`CONTAINERGO_BACKEND=docker` を、Apple Container を明示する場合は
`CONTAINERGO_BACKEND=apple` を設定します。

本ライブラリはバックエンドの CLI（`container` / `docker`）を子プロセスとして
呼び出します。cgo もデーモン API クライアントも使いません。
`DOCKER_HOST`、コンテキスト、レジストリ認証は docker CLI 自身が解決します。
ライブラリが remote Docker endpoint の選択に使うのは
`DOCKER_HOST=tcp://...` だけで、remote Docker context は検出しません。

現在のチェックアウトで確認したバックエンド（ライブラリが照合する CLI stderr
文言と inspect JSON 形状）:

| バックエンド | 確認済みバージョン |
|---|---|
| Apple Container | 1.2.x–1.3.x |
| Docker Engine / CLI | 29.x |

新しい CLI ではエラー文言や JSON フィールドが変わる可能性があります。
`engine_apple.go` / `engine_docker.go` 先頭の stderr マッチャと、
`internal/inspect/testdata/`・`testdata/` のフィクスチャを参照してください。

## インストール

リリース版 `v0.2.0` の API を使う場合:

```text
go get github.com/hirokazumiyaji/container-go@v0.2.0
```

現在の開発チェックアウトはリリースではありません。
`@v0.2.0` にバージョンテーブルの開発版 API が入っていると仮定しないでください。

## API の安定性

1.0 未満では、マイナーリリース（0.x）に破壊的変更が含まれる場合があります。
再現可能なビルドにはモジュールバージョンを明示して pin してください
（`go get ...@v0.2.0`）。

ルートの `container` パッケージ（`Run`、オプション、ライフサイクルヘルパー）が
主な統合面で、マイナーシリーズ内では比較的安定を目指します。
`wait` パッケージも公開 API ですが、カスタム strategy 向けのインターフェース
（特に [`wait.Target`](wait/wait.go)）はバックエンドやプローブ要件の変化に応じて
変更される可能性があります。可能なら組み込み strategy を使ってください。

[CHANGELOG.md](CHANGELOG.md) も参照してください。
`v0.2.0` の節はタグ付きリリースだけ、`Unreleased` の節は現在の開発 API を
説明します。

## 接続エンドポイント

**Apple Container バックエンド**: 既定では各コンテナは `default` vmnet
ネットワーク上に実 IP を持ち、ホストから直接到達できます。既定ではこの IP
を使います。

- `Host` はコンテナ IP、`MappedPort` はコンテナポートそのまま、`Endpoint`
  はその組み合わせを返します。
- ホストポートを消費しないため、並列テストがポートで衝突しません。
- Apple では `WithExposedPorts` は handle 側の endpoint 宣言であり、backend に
  別の `--expose` flag を渡しません。

特定の publish ポートを使うときは `Endpoint`（または既知ホストと
`MappedPort`）を優先してください。複数の publish で host IP が異なるとき、
`Host` は先頭の publish binding のアドレスだけを返します。

**Docker バックエンド**: コンテナ IP にはホストから到達できないことが多いため
（Docker Desktop）、`WithExposedPorts` で宣言したポートはデーモンが割り当てる
ポートへ自動公開されます（testcontainers と同じモデル）。ローカルは
loopback（`-p 127.0.0.1::<port>`）、リモートデーモン
（`DOCKER_HOST=tcp://host`）は全インターフェース（`-p 0.0.0.0::<port>`）へ
束縛します。`Host` は `127.0.0.1`（`tcp://` の `DOCKER_HOST` ならそのホスト）、
`MappedPort` は割り当てられたポートを返します。割り当てをデーモンが起動時に
原子的に行うため、並列テストがポートを奪い合うことはありません。
remote デーモンで loopback（`127.0.0.1:...`、`[::1]:...`）を明示した
`WithPublishedPort` はリモート側でしか待受けられないため拒否します。
`DOCKER_HOST` だけを検出し、remote Docker context は検出しません。

クライアントが `localhost` を要求する場合（または構成上コンテナ IP に
届かない場合）、ポートを明示的に公開します。

```go
package docexample

import (
    "context"
    "testing"

    container "github.com/hirokazumiyaji/container-go"
    "github.com/hirokazumiyaji/container-go/wait"
)

func TestPublishedEndpoint(t *testing.T) {
    ctx := context.Background()
    ctr, err := container.Run(ctx, "nginx:alpine",
        container.WithExposedPorts("80/tcp"),
        container.WithPublishedPort("127.0.0.1:18080:80"),
        container.WithWaitStrategy(wait.ForHTTP("/")),
    )
    container.Cleanup(t, ctr)
    if err != nil {
        t.Fatal(err)
    }

    host, err := ctr.Host(ctx)
    if err != nil {
        t.Fatal(err)
    }
    mapped, err := ctr.MappedPort(ctx, "80/tcp")
    if err != nil {
        t.Fatal(err)
    }
    endpoint, err := ctr.Endpoint(ctx, "80/tcp")
    if err != nil {
        t.Fatal(err)
    }
    _, _, _ = host, mapped, endpoint
}
```

`MappedPort` と `Endpoint` は `WithExposedPorts` で宣言したポートと、明示的に
公開したポートを解決します。`ContainerIP` は別の診断用 API であり、wait
package が使う値ではありません。Docker Desktop ではコンテナ IP に到達できない
ことが多いため、クライアントには `Endpoint` を優先してください。

## 待機戦略

Apple Container にはヘルスチェックも wait コマンドもないため、`wait`
package がクライアント側で readiness を判定します。
次の例は `v0.2.0` で利用できる strategy を使います。

```go
package docexample

import (
    "github.com/hirokazumiyaji/container-go/wait"
)

func ReleasedWaitStrategies() {
    _ = wait.ForLog("Ready to accept connections")
    _ = wait.ForListeningPort("6379/tcp")
    _ = wait.ForExposedPort()
    _ = wait.ForHTTP("/health").WithPort("6379/tcp").
        WithStatusCodeMatcher(func(status int) bool {
            return status >= 200 && status < 300
        })
    _ = wait.ForExec([]string{"pg_isready"})
    _ = wait.ForAll(wait.ForExposedPort())
    _ = wait.ForAny(wait.ForExposedPort())
}
```

基本 strategy の起動タイムアウトは既定 60 秒です。
`ForListeningPort`、`ForExposedPort`、`ForHTTP` は既定 100 ミリ秒、
`ForExec` は既定 250 ミリ秒で poll します。`ForLog` は
`FollowLogs` のストリームを継続して読むため、`WithPollInterval` の
セッターは動作を変更しません。`v0.2.0` の `ForAll` と `ForAny` には
合成全体に設定する timeout setter がありません。接続・HTTP strategy は
最大 1 秒間隔で停止状態を調べます。`ForExec` は poll 中には fail-fast
しません。wait の期限到来時にコンテナ状態を調べます。`ForLog` は
pattern が出る前にログ stream が終了した場合に停止を報告します。

現在の開発チェックアウトには次の API が追加されています。
これらは `v0.2.0` にはありません。

```go
package docexample

import (
    "crypto/tls"
    "net/http"
    "time"

    "github.com/hirokazumiyaji/container-go/wait"
)

func DevelopmentWaitOptions() {
    _ = wait.ForHTTP("/health").
        WithHeaders(map[string]string{"X-Test": "yes"}).
        WithHeader("X-Other", "yes").
        WithBasicAuth("user", "pass").
        WithTLSConfig(&tls.Config{}).
        WithHTTPClient(&http.Client{}).
        WithStartupTimeout(time.Second).
        WithPollInterval(time.Millisecond)
    _ = wait.ForAll(wait.ForExposedPort()).WithStartupTimeout(time.Second)
    _ = wait.ForAny(wait.ForExposedPort()).WithStartupTimeout(time.Second)
}
```

新規作成し再利用していないコンテナで待機に失敗した場合、コンテナをロールバックし、上限付き log の取得が成功した場合だけ 1 MiB 上限の末尾をエラーに付けます。再利用コンテナは、後述の共有ライフタイムの終わりまで残します。

## ログ

`Logs` と `LogsWithOptions` は CLI process が終了するため有限のスナップショットを返しますが、単体で byte 上限は課しません。
`Logs` は利用可能な出力をすべて要求し、`LogsWithOptions{Tail, Since}` は `Tail` または `Since` を指定したときに行数または時間の範囲を指定します。ただし、その範囲の大きさはコンテナ出力に依存します。長時間・大量出力のコンテナでは `Tail` を使ってください。
`FollowLogs` はストリーミングする `io.ReadCloser` を返し、Close するか
context をキャンセルするとバックエンド CLI を停止します。`ForLog` は
`FollowLogs` を使い、`Logs` は新しい出力を追尾しません。
`LogsOptions` と `LogsWithOptions` は開発版 API であり、`v0.2.0` には
ありません。

次の開発版 example は、`v0.2.0` 以降に追加された log stream、exec option、
公開 error symbol も示します。

```go
package docexample

import (
    "context"
    "errors"
    "time"

    container "github.com/hirokazumiyaji/container-go"
)

func DevelopmentLogOptions(ctx context.Context, ctr *container.Container) error {
    logs, err := ctr.LogsWithOptions(ctx, container.LogsOptions{
        Tail:  10,
        Since: time.Now(),
    })
    if err != nil {
        return err
    }
    defer logs.Close()

    stream, err := ctr.FollowLogs(ctx)
    if err != nil {
        return err
    }
    defer stream.Close()

    _, _, err = ctr.Exec(ctx, []string{"true"},
        container.WithExecEnv(map[string]string{"MODE": "test"}),
        container.WithExecUser("test"),
        container.WithExecWorkDir("/tmp"),
    )
    if errors.Is(err, container.ErrContainerNotFound) {
        return err
    }
    if errors.Is(err, container.ErrGenerationReplaced) {
        return err
    }
    var cliErr *container.CLIError
    if errors.As(err, &cliErr) {
        return err
    }
    return err
}
```

## イメージの pull

コンテナを新規作成する必要がある場合、`Run` は起動前に明示的な pull policy
を適用します。

- `PullMissing`（既定値）はローカルストアを検査し、イメージがないときだけ
  明示的に pull します。
- `PullAlways` は新規コンテナ作成の試行ごとに明示的な pull を要求します。
- `PullNever` は検査だけを行い、イメージがない場合は起動前に
  `ErrImageNotFound` を返します。

同じプロセスの並行処理は、バックエンド、イメージ、プラットフォーム、操作の
種類が同じ pull を共有します。待機中の呼び出し元がキャンセルされても、共有
pull は残りの呼び出し元のために続きます。Docker バックエンドは `docker run`
に `--pull=never` を渡すため、CLI が二重に pull することはありません。
Apple Container も同じ明示的な policy 経路を使います。

```go
package docexample

import (
    "context"
    "errors"

    container "github.com/hirokazumiyaji/container-go"
)

func PullPolicy(ctx context.Context) {
    _, err := container.Run(ctx, "redis:7-alpine",
        container.WithPullPolicy(container.PullAlways))
    _ = errors.Is(err, container.ErrImageNotFound)
    _ = container.Pull(ctx, "redis:7-alpine")
}
```

`PullNever` はイメージがない場合、起動前に失敗します。
呼び出し元は `errors.Is(err, container.ErrImageNotFound)` を使えます。

## クリーンアップの契約

通常の終了、作成後の rollback、create 失敗、異常終了では、エラーの見え方が異なります。

1. **通常の cleanup**: `container.Cleanup(t, ctr)` は
   `t.Cleanup` 経由で `TerminateContainer` を登録します。終了エラーは
   テストの log に残ります。defer 形式の
   `container.TerminateContainer(ctr)` はエラーを呼び出し元へ返します。
   どちらの helper も nil 安全です。
2. **作成後の rollback**: 再利用でない `Run` が file copy または readiness
   待ちの途中で失敗すると、`Run` は `Terminate` を呼びます。削除にも失敗
   した場合は、元の失敗とコンテナが残ったことを示すメッセージを返り値に
   含めます。
3. **create 失敗**: backend の `run` 自体が失敗した場合、best-effort の
   `cleanupFailedCreate` は、この process の managed / session label を
   持つコンテナだけを inspect します。creation label がある場合はこの
   run と一致する必要があります。ただし、この best-effort 経路では
   label がないことを不一致とは扱いません。lock、inspect、delete の
   error は `Run` の返り値に連結されません。name conflict はこの経路
   では削除しません。
4. **異常終了**: 親 process が終了して reaper pipe が閉じられると、
   best-effort watchdog が強制削除を試みます。process を終了させる
   panic、`SIGKILL`、`os.Exit` でも pipe は閉じます。recover した panic では
   閉じません。親が生きている間、reaper は何もしません。これはトランザクション的な保証ではなく、登録 error や各 entry の delete error は `Run` へ返されません。現在の base は Apple 形式の名前と creation generation を reaper entry として受け付けます。

reaper は外部 `/bin/sh` child で、Windows では利用できません。
実 CLI コンテナを非 reuse 経路で登録した場合に遅延起動します。
現在の base には重要な Docker の前提条件があります。`docker run` は
完全な 64 桁 hex container ID を返しますが、`reaper.register` は現在は
Apple 形式の名前だけを受け付ける。そのため、通常の Docker `run --detach` 経路では
この checkout の reaper に Docker ID を登録できません。backend が解析可能な
ID を返さない場合だけ、`Run` は name と generation の経路にフォールバックします。
Docker-ID 登録経路には issue #73 を stack する必要があります。#73 を適用すると
Docker entry は immutable ID を使い、`docker rm --force` で削除されます。通常の
Docker `Terminate` と rollback handle は独立して ID を利用できます。

reaper の spawn failure は retry 上限後に一度だけ log へ残ります。
delete failure は shell が無視します。reaper の動作を cleanup の成功確認に
使わないでください。

`CONTAINERGO_KEEP=1` は `Cleanup`、`TerminateContainer`、reaper 登録を
省略し、コンテナを調査用に残します。明示的な `Container.Terminate`、
作成後の失敗に対する rollback、create 失敗後の best-effort cleanup の
動作は変えません。

`container.Prune(ctx)` は過去 session を含め、本 library が作成した停止済み
コンテナ（`com.github.hirokazumiyaji.container-go` label 付き）を削除します。
実行中コンテナは削除しません。

## Reuse（テスト / process 間でのコンテナ共有）

`WithReuse` は安定した `WithName` に対する get-or-create です。
同一 process 内の並列呼び出しや、同じ host の別 process の `go test`
package が 1 つのコンテナを共有します。

```go
package docexample

import (
    "context"
    "testing"

    container "github.com/hirokazumiyaji/container-go"
    "github.com/hirokazumiyaji/container-go/wait"
)

func TestReuse(t *testing.T) {
    ctx := context.Background()
    ctr, err := container.Run(ctx, "redis:7-alpine",
        container.WithName("it-redis"),
        container.WithReuse(),
        container.WithReuseGroup("integration"),
        container.WithExposedPorts("6379/tcp"),
        container.WithWaitStrategy(wait.ForListeningPort("6379/tcp")),
    )
    container.Cleanup(t, ctr) // reused handle では何もしない
    if err != nil {
        t.Fatal(err)
    }
    _ = ctr
}
```

契約:

- `WithName` 必須。readiness strategy は attach 時も必ず再実行する。
- 既存コンテナは `WithReuse` で作成され、互換性のある image である必要が
  ある。create 競合による name conflict は成功として扱い、既存へ attach
  する。
- 停止済み残骸は削除して再作成する。running のまま ready にならない場合は
  削除せず error を返す。
- image 互換性は両 backend で検査する。port の互換性は backend ごとに
  異なる。Docker は自動公開された exposed-port binding と明示的な
  publication を比較する。Apple は明示的な published binding を比較するが、
  library の Apple inspect model に `WithExposedPorts` の宣言が残らないため、
  それを比較できない。Apple では新しい handle ごとに自分の exposed-port
  宣言を共有コンテナ IP へ適用するので、宣言を分離したい場合は名前を変える。
- `env` / `cmd` / `mounts` の差は既存へ黙って attach する。重要な設定は
  別の名前を使う。
- 各作成は generation label を持つ。`Terminate` と stopped 再作成経路は
  置き換わった generation の削除を拒否する。名前ベースの guard は同じ
  host で本 library を使う process 間の協調に限られる。外部 CLI による
  delete / recreate は対象外。Docker handle は利用可能な immutable ID を
  使う。
- `Cleanup` / `TerminateContainer` / watchdog reaper は reused handle を
  削除しない。明示的な `ctr.Terminate` だけが共有コンテナを削除できる。
- `container.PruneReuseGroup(ctx, "integration")` はその group の
  コンテナを強制削除する（CI teardown）。group は再利用 key ではなく
  label である。通常の `Prune` は停止済み管理対象だけを削除する。

ライブラリはテスト間のアプリケーションデータを自動初期化しません。
key prefix、schema 分離、`Exec` による reset（`FLUSHALL` など）を使って
ください。

## セキュリティ上の注意

- すべての CLI 呼び出しは argv 配列で行い、シェルを経由しません。唯一の
  shell script（reaper）は固定文字列で、検証済みの container name と、
  #73 適用後の Docker ID だけを stdin data として受け取ります。
- 環境変数は mode 0600 の一時 env file 経由で渡すため、秘密が process
  一覧（`ps`）に現れません。
- レジストリ認証情報は本 library では扱いません。Apple Container では
  `container registry login`、Docker では `docker login` を使い、認証情報と
  registry context は backend CLI が管理します。

## testcontainers-go との違い

非対応（相当機能が存在しない、またはスコープ外）:

| testcontainers-go | 本 library |
|---|---|
| `wait.ForHealthCheck` | 相当機能なし。`ForLog` / `ForExec` / `ForHTTP` を使用 |
| Dockerfile からの build | スコープ外（`container build` / `docker build` を直接使用） |
| Ryuk reaper container | ローカルの watchdog reaper process で代替。ただし上記の制限がある |
| random host port mapping | Apple はコンテナ IP へ直接接続。Docker はランダム loopback port へ自動公開 |
| network / volume の作成と lifecycle 管理 | 当面スコープ外。`WithNetwork` は既存 network へ接続し、`WithMounts` は mount 指定を受け取る |
| `GenericContainerRequest.Reuse` | `WithReuse` + `WithName`: process 間 get-or-create。再 wait 必須、Cleanup / reaper は所有しない |

## 開発

```text
make test                # backend 不要の unit test
make vet
make integration         # integration test（bench / singleflight を除外）。backend がなければ skip
make integration-docker  # Docker backend の integration test のみ
make bench-integration   # pull が重い bench / singleflight
```

Integration test の image は Docker Hub の匿名 pull 制限を避けるため
`public.ecr.aws/docker/library/...` を使います。`CONTAINERGO_BACKEND=apple`
または `docker` で片方だけを実行できます。

GitHub Actions は `ubuntu-latest` で Go 1.23.0 と安定版 Go を使い、unit
test、race test、lint、`govulncheck` を実行します。同じ runner で Docker
integration test の行列も実行します。Apple Container integration は
GitHub ホストランナーに service がないためローカル専用です。
integration tag のないドキュメント test は、この README の英語版・日本語版
と design document の単独 Go example を抽出してコンパイルします。tag付き
`examples/` の example が実 backend を動かします。

Design document: [docs/design.md](docs/design.md)（日本語版:
[docs/design.ja.md](docs/design.ja.md)）。設計ドキュメントの「実装フェーズ」は
履歴であり、現在の API 契約ではありません。

コントリビューション: [CONTRIBUTING.md](CONTRIBUTING.md)。セキュリティ報告:
[SECURITY.md](SECURITY.md)。

## ライセンス

MIT
