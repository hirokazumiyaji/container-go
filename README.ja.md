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

root ライブラリのチェックアウトには Go 1.23 以降が必要です。
`make bench-integration` が使う nested `bench/` モジュールには
Go 1.25 以降が必要です。`v0.2.0` モジュールには Go 1.27 以降が必要です。

| API または動作 | `v0.2.0` | 現在の開発チェックアウト |
|---|---|---|
| `Run`、基本オプション、ライフサイクル、エンドポイント、`Exec`、`Logs`、`FollowLogs`、copy、pull policy、Reuse、Cleanup、バックエンド選択、元の wait 戦略 | あり | あり |
| `LogsOptions` と `LogsWithOptions` | なし | あり |
| `wait.ForHTTP` のヘッダー、認証、TLS、独自クライアント設定 | なし | あり |
| 公開型 `wait.AllStrategy` / `AnyStrategy` と合成 strategy の `WithStartupTimeout` | なし | あり |
| root の `CLIError`、`ErrContainerNotFound`、`ErrGenerationReplaced` | なし | あり |
| 世代安全な削除、`v0.2.0` 以降の endpoint 強化、現在のリーパー強化 | `v0.2.0` の契約には含まれない | 一部実装済み。以下の後続 issue の制約を参照 |

以下の説明で「現在の開発」と記した節以外は、現行チェックアウトの動作を説明します。
タグ付き `v0.2.0` の API については、バージョン表を確認してください。
コード例は単独ファイルとしてリポジトリのドキュメントテストがコンパイルします。

このチェックアウトは、バックエンド固有の動作を改修する後続 issue の
修正を統合した状態ではありません。特に次の制限があります。

- Apple の `LogsWithOptions` は現在 Docker 形式の `--tail` と
  `--since` を渡します。Apple Container は tail に `-n` を使い、
  `--since` を提供しません。Apple の機能を文書化する前に #82 を
  適用する必要があります。
- endpoint 解決は最初の inspect をキャッシュし、IP アドレスと host
  binding も含めます。この値は変わることがあるため、古い値を返す
  ことがあります。動的な endpoint データの更新は #85 が担当します。
- reuse は既存コンテナですべての ownership / generation label を
  要求せず、readiness 後に generation を再確認しません。generation が
  ない場合は name ベースの delete に到達する可能性があります。#83 と
  #84 がこれらの fail-open 経路を扱います。
- Docker の handle は delete では immutable ID を使いますが、他の backend
  operation は現在 logical name を対象にします。そのため stale handle が
  同じ名前の置き換えを inspect または変更する可能性があります。#74 が
  operation target の修正を担当します。
- Docker の `Prune` は現在 exited コンテナだけを選び、dead 状態は
  選びません。dead 状態の対応は #113 が担当します。
- Apple の `Prune` と `PruneReuseGroup` は現在、fresh candidate validation や
  per-name lock なしの list-to-delete path を使う。#98 が Apple cleanup race を
  担当する。
- 現在の `WithReuse` attach caller は `WithFiles` と `PullAlways` を無視する。
  #94 がこれらの creation-only side effect を担当する。
- Windows Docker の bind source と remote Docker の bind source semantics は
  現在の validation path では扱えていません。#76 が host path と remote mount を
  担当します。
- `ForListeningPort` と `ForExposedPort` は TCP 専用 probe です。UDP は endpoint
  設定には宣言できますが、UDP readiness request は現在 TCP dial に渡され、timeout
  または別の TCP listener に接続する可能性があります。#77 が protocol validation を
  担当します。
- `Stop` の非 nil timeout は whole second へ truncation し、negative や極端な値を
  reject しません。#89 が timeout validation を担当します。
- Apple の `PullNever` は現在 best-effort な backend precheck であり、no-fetch
  保証ではない。#112 が strict な Apple capability handling を担当する。
- wait の timeout/cancellation error は error chain に一様に保持されません。#92 が
  built-in strategy の error contract を担当します。
- liveness probe failure は元の `*CLIError` を `ErrSystemNotRunning` の message に
  flatten する。#104 が error chain の保持を担当する。
- target に一致する entry がない successful inspect response は
  `ErrContainerNotFound` ではなく generic error を返す場合がある。#103 がその
  classification gap を担当する。
- public option の validation は部分的です。negative log tail、zero memory、unknown
  mount type、reuse-group grammar は一様に reject されません。#102 が typed validation を
  担当します。
- reaper は full inspect output を一時 file に staging するため、環境 data が残る
  可能性があります。#111 が staging exposure と cleanup を担当します。

以下の節は、後続ブランチの動作ではなく、現在の上限を説明します。

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
検出した non-loopback `DOCKER_HOST=tcp://...` デーモンで loopback
（`127.0.0.1:...`、`[::1]:...`）を明示した `WithPublishedPort` は
リモート側でしか待受けられないため拒否します。`DOCKER_HOST` だけを
検出し、remote Docker context は検出しません。

ローカルの Docker daemon でクライアントが `localhost` を要求する場合
（または構成上コンテナ IP に届かない場合）、ポートを明示的に公開します。
以下の loopback example は local-only です。non-loopback の
`DOCKER_HOST` では loopback binding を reject します。

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
公開したポートを解決します。ポートが未宣言、または宣言済みポートに利用できる
host binding がない場合は `ErrPortNotExposed` を返します。`ContainerIP` は別の
診断用 API であり、wait package が使う値ではありません。Docker Desktop では
コンテナ IP に到達できないことが多いため、クライアントには `Endpoint` を優先
してください。

現在のチェックアウトでは、endpoint 関連メソッドが最初の inspect を
キャッシュします。キャッシュにはコンテナ IP と host 側 binding が
含まれますが、どちらも container の lifecycle 中に変更されます。`State`
は新しい inspect を実行しますが、endpoint の結果には古い値が
含まれることがあります。この値は immutable な事実ではなく snapshot
として扱い、動的データの更新は #85 が担当します。Docker が複数 network を
報告し top-level address がない場合、現在の network selection は
決定的な first-network contract ではなく unspecified です。

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

`ForListeningPort` と `ForExposedPort` は TCP 専用の readiness probe です。
UDP は endpoint 設定には宣言できますが、現在の実装は probe 前に `/udp` を
reject せず TCP dial に渡します。そのため timeout したりする別の TCP listener に
接続する可能性があります。不正な port specification も wait の終了まで
retry されます。#77 を参照してください。

| API | TCP | UDP |
|---|---|---|
| `WithExposedPorts` / `WithPublishedPort` | 現在の option parser が受理 | endpoint / publish 設定では受理 |
| `wait.ForListeningPort` | `PORT` または `PORT/tcp` | UDP probe ではない。UDP declaration を TCP dial する場合がある |
| `wait.ForExposedPort` | 最初の TCP declaration を使う | 解決する場合、最初の UDP declaration も TCP dial に渡される |

### Stop の timeout

非 nil の `Container.Stop` timeout は、両 backend が fractional value を
truncate して whole seconds に変換します。negative や極端な duration は reject
されず、timeout を saturation なしで query budget に加算するため、library 側の
maximum も強制されません。backend 固有の limit が適用されます。#89 を適用する
までは sub-second、negative、maximum 付近の duration に依存しないでください。

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

`Logs` と、成功する `LogsWithOptions` 呼び出しは CLI process が終了するため
有限のスナップショットを返しますが、単体で byte 上限は課しません。
`Logs` は利用可能な出力をすべて要求します。`LogsOptions` は backend
固有です。Docker は `Tail` と `Since` の両方を受け付けます。Apple
Container 1.2.x–1.3.x は tail に `-n` を使い、`--since` は
実装していません。このチェックアウトは Apple にも Docker 形式の
`--tail` と `--since` を渡すため、`LogsWithOptions` はここでは
backend 中立ではありません。#82 を適用するまで Apple では
`Logs` を使ってください。#82 の変更は `Tail` を Apple の `-n` に
対応させ、`Since` を unsupported として拒否する予定です。範囲を指定
しても大きさはコンテナ出力に依存し、`LogsWithOptions` は byte 上限を
追加しません。
`FollowLogs` はストリーミングする `io.ReadCloser` を返し、Close するか
context をキャンセルするとバックエンド CLI を停止します。`ForLog` は
`FollowLogs` を使い、`Logs` は新しい出力を追尾しません。
`LogsOptions` と `LogsWithOptions` は開発版 API であり、`v0.2.0` には
ありません。

現在のチェックアウトでは validation は一様ではありません（#102）。
negative な `LogsOptions.Tail` は 0/all として扱われ、`WithMemory("0")` は現在の
parser に受理され、unknown な `MountType` は `WithMounts` で reject されず、
`PruneReuseGroup` は `WithReuseGroup` より弱い grammar を使います。これらは
現在の動作であり、backend がその設定を受け付ける保証ではありません。

次の例は Docker 固有です。`v0.2.0` 以降に追加された log stream、exec
option、公開 error symbol も示しますが、Apple の
`LogsWithOptions` の例ではありません。

```go
package docexample

import (
    "context"
    "errors"
    "time"

    container "github.com/hirokazumiyaji/container-go"
)

func DockerLogOptions(ctx context.Context, ctr *container.Container) error {
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

これらの error check は path ごとに異なる。liveness 分類では元の
`*CLIError` が text に flatten される場合があり（#104）、target に一致する
entry がない successful inspect response は `ErrContainerNotFound` を返さない
ことがある（#103）。

## イメージの pull

コンテナを新規作成する必要がある場合、`Run` は起動前に明示的な pull policy
を適用します。

- `PullMissing`（既定値）はローカルストアを検査し、イメージがないときだけ
  明示的に pull します。
- `PullAlways` は新規コンテナ作成の試行ごとに明示的な pull を要求します。
  現在の `WithReuse` attach ではこの side effect を実行しません（#94）。
- `PullNever` は backend 固有で、現在のチェックアウトでは best-effort precheck
  を行い、image がない場合は `ErrImageNotFound` を返します。Docker は
  `--pull=never` も渡すため strict な no-fetch 経路がありますが、Apple Container
  には同等の run-time switch がないため precheck は no-fetch 保証ではありません
  （#112）。

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
    "testing"

    container "github.com/hirokazumiyaji/container-go"
)

func PullPolicy(ctx context.Context, t testing.TB) {
    always, err := container.Run(ctx, "redis:7-alpine",
        container.WithPullPolicy(container.PullAlways))
    container.Cleanup(t, always)
    if err != nil {
        t.Fatal(err)
    }

    never, err := container.Run(ctx, "redis:7-alpine",
        container.WithPullPolicy(container.PullNever))
    container.Cleanup(t, never)
    if errors.Is(err, container.ErrImageNotFound) {
        return
    }
    if err != nil {
        t.Fatal(err)
    }

    _ = container.Pull(ctx, "redis:7-alpine")
}
```

例は `Run` の error を確認する前に cleanup を登録する。現在のチェックアウトでは
`PullNever` の `ErrImageNotFound` は backend 固有であり、Apple では best-effort
である。Apple の no-fetch 保証ではない（#112）。

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
   現在は label がないことも許容します。これは replacement を防ぐ
   一般的な保証ではなく、#83 が missing-generation 経路を扱います。
   lock、inspect、delete の error は `Run` の返り値に連結されません。
   name conflict はこの経路では削除しません。
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

現在の reaper は full `inspect` output を namespace のない `mktemp` file に
staging し、通常の完了または inspect-error path では削除します。reaper が
kill されると、環境 data を含む file が残る可能性があります。cleanup 前に container-go と reaper
process を停止し、実効 `TMPDIR` で user 所有の regular file だけを metadata-only
listing し、affected time window に限定してください。file content を表示・grep
したり、symlink を追跡したり、broad recursive delete を実行しないでください。
該当 run に確実に帰属する file だけを削除し、inspect output に含まれた可能性が
ある credential を rotate してください（#111）。これは no-leak 保証ではありません。

`CONTAINERGO_KEEP=1` は `Cleanup`、`TerminateContainer`、reaper 登録を
省略し、コンテナを調査用に残します。明示的な `Container.Terminate`、
作成後の失敗に対する rollback、create 失敗後の best-effort cleanup の
動作は変えません。

`container.Prune(ctx)` は、現在の backend の filter が選ぶ、本 library が
作成したコンテナを削除します。Apple は managed コンテナのうち stopped
状態を選びます。Docker は現在 managed コンテナのうち exited 状態だけ
を選ぶため、dead 状態のコンテナは #113 を適用するまで削除されません。
この filter は running または created 状態を選びません。Apple では現在の
`Prune` と `PruneReuseGroup` の list-to-delete path が、per-name lock の中で
candidate を fresh inspect せず name を delete するため、replacement が
競合する可能性があります（#98）。

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
- 既存コンテナは `WithReuse` marker を持ち、互換性のある image である
  必要があります。create 競合による name conflict は成功として扱い、
  既存へ attach します。
- 停止済みコンテナは、現在の reuse marker と image の check に
  合格した場合だけ削除・再作成の対象にする。running のまま ready に
  ならない場合は削除せず error を返す。
- image 互換性は両 backend で検査する。port の互換性は backend ごとに
  異なる。Docker は自動公開された exposed-port binding と明示的な
  publication を比較する。Apple は明示的な published binding を比較するが、
  library の Apple inspect model に `WithExposedPorts` の宣言が残らないため、
  それを比較できない。Apple では新しい handle ごとに自分の exposed-port
  宣言を共有コンテナ IP へ適用するので、宣言を分離したい場合は名前を変える。
- `env` / `cmd` / `mounts` の差は既存へ黙って attach する。重要な設定は
  別の名前を使う。現在のチェックアウトでは `WithFiles` と `PullAlways` は
  reuse creation path だけで適用され、attach caller では無視される（#94）。
- このチェックアウトが作成するコンテナは通常 generation label を持ちます。
  ただし、既存の reuse コンテナでは現在の check が managed label と
  creation label のすべてを要求せず、`WithReuse` marker と互換 image
  だけを要求します。generation が空の場合、name ベースの delete に
  到達する可能性があります。現在の code は readiness 後に generation
  を再確認しません。#83 と #84 がこれらの fail-open 経路を扱います。
  適用されるまでは、reuse を信頼できない same-name replacement への
  保護として扱わないでください。Docker の delete handle は利用可能な
  immutable ID を使いますが、他の operation は #74 を適用するまで logical
  name を使います。
- `Cleanup` / `TerminateContainer` / watchdog reaper は reused handle を
  削除しない。明示的な `ctr.Terminate` だけが共有コンテナを削除できる。
- 名前単位の `flock` は generation-checked な通常の `Terminate` /
  failed-create cleanup 経路を保護する。現在の Apple `Prune` /
  `PruneReuseGroup` list-to-delete path は fresh candidate validation がなく、
  外部 reaper の inspect / delete window にもこの lock は使われないため、
  これらの経路は協調していないものとして扱う（prune は #98）。
- `container.PruneReuseGroup(ctx, "integration")` はその group の
  コンテナを強制削除する（CI teardown）。group は再利用 key ではなく
  label である。Apple では現在の list-to-delete path が `Prune` と同じ
  fresh revalidation / per-name lock の欠落を持つ（#98）。通常の `Prune` は
  上記の backend filter を使い、現在の Docker backend では exited コンテナ
  だけが対象で、dead は対象外である。

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
| network / volume の作成と lifecycle 管理 | 当面スコープ外。`WithNetwork` は既存 network へ接続し、`WithMounts` は mount 指定を受け取るが、Windows / remote bind source の制限は #76 |
| `GenericContainerRequest.Reuse` | `WithReuse` + `WithName`: process 間 get-or-create。再 wait 必須、Cleanup / reaper は所有しない |

## 開発

```text
make test                # backend 不要の unit test
make vet
make integration         # integration test（bench / singleflight を除外）。backend がなければ skip
make integration-docker  # Docker backend の integration test のみ
make bench-integration   # pull が重い bench / singleflight（bench module は Go 1.25+）
```

Integration test の image は Docker Hub の匿名 pull 制限を避けるため
`public.ecr.aws/docker/library/...` を使います。`CONTAINERGO_BACKEND=apple`
または `docker` で片方だけを実行できます。

GitHub Actions は `ubuntu-latest` で Go 1.23.0 と安定版 Go を使い、unit
test、race test、lint、`govulncheck` を実行します。同じ runner で Docker
integration test の行列も実行します。Apple Container integration は
GitHub ホストランナーに service がないためローカル専用です。
integration tag のないドキュメント test は、この README の英語版・日本語版
と design document の fenced code block をすべて抽出し、対応する block を
コメント除去後に比較します。Go block は引用符付きローカル `replace` を持つ
一時 module 内でコンパイルします。tag付き `examples/` の example が実
backend を動かします。

Design document: [docs/design.md](docs/design.md)（日本語版:
[docs/design.ja.md](docs/design.ja.md)）。設計ドキュメントの「実装フェーズ」は
履歴であり、現在の API 契約ではありません。

コントリビューション: [CONTRIBUTING.md](CONTRIBUTING.md)。セキュリティ報告:
[SECURITY.md](SECURITY.md)。

## ライセンス

MIT
