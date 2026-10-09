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
- Docker の handle は `docker run` が出力した immutable ID を delete に使います
  が、inspect から得た ID には target validation がありません。他の backend
  operation は現在 logical name を対象にします。そのため stale handle が同じ
  名前の置き換えを inspect または変更する可能性があります。#74 が operation
  target の修正を担当し、#103 が Docker inspect target validation を担当します。
- Docker の `Prune` は現在 exited コンテナだけを選び、dead 状態は
  選びません。dead 状態の対応は #113 が担当します。
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
  `ErrContainerNotFound` ではなく generic error を返す場合がある。Docker
  では empty または malformed な inspect data がその path になることがあり、
  現在の parser は返された object の ID や name が要求値と一致するか検証しない。
  valid だが mismatched な object は信頼できる no-match signal ではない。#103 が
  その classification gap を担当する。
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
| macOS | Apple Container | macOS 26+、Apple Silicon、[Apple Container](https://github.com/apple/container) CLI 1.2.2 または 1.3.0、`container system start` 実行済み |
| Linux | Docker | docker CLI 29.x（29.7.2 を確認）と稼働中のデーモン |
| Windows | Docker | docker CLI 29.x（29.7.2 を確認）と稼働中のデーモン（watchdog リーパーなし。後述） |
| macOS | Apple Container | macOS 26+、Apple Silicon、[Apple Container](https://github.com/apple/container) 1.2.x(`container system start` 実行済み) |
| Linux | Docker | docker CLI + 稼働中のデーモン。copy-out には client/server 29.7.0 以上が必要 |
| Windows | Docker | docker CLI + 稼働中のデーモン。copy-out には client/server 29.7.0 以上が必要(watchdog リーパーなし。後述) |

macOS で Docker（Docker Desktop など）を使う場合は
`CONTAINERGO_BACKEND=docker` を、Apple Container を明示する場合は
`CONTAINERGO_BACKEND=apple` を設定します。

本ライブラリはバックエンドの CLI（`container` / `docker`）を子プロセスとして
呼び出します。cgo もデーモン API クライアントも使いません。
`DOCKER_HOST`、コンテキスト、レジストリ認証は docker CLI 自身が解決します。
ライブラリが remote Docker endpoint の選択に使うのは
`DOCKER_HOST=tcp://...` だけで、remote Docker context は検出しません。

現在のチェックアウトで確認した backend の動作（ライブラリが照合する CLI
stderr 文言と inspect JSON 形状）:

| バックエンド | このチェックアウトで使用した根拠 |
|---|---|
| Apple Container CLI | 1.2.2 と 1.3.0 の source、help、inspect fixture |
| Docker Engine / CLI | 29.x 形式の inspect fixture と、ローカル開発時の 29.7.2 |
| Apple Container | 1.2.x–1.3.x |
| Docker Engine / CLI | 29.x。安全な copy-out には client/server 29.7.0 以上が必要 |

この表はリポジトリの根拠を示すもので、範囲内のすべての release を確認済みだと
いう互換性宣言ではありません。新しい CLI ではエラー文言や JSON フィールドが
変わる可能性があります。`engine_apple.go` / `engine_docker.go` 先頭の stderr
マッチャと、`internal/inspect/testdata/`・`testdata/` のフィクスチャを参照して
ください。

`WithName` と name-addressed reaper entry は共有の library guard
`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`（1～63 文字）を使う。これは保守的な安全
規則であり、Docker または Apple の完全な name grammar を述べるものではない。
Apple Container の CLI はより厳密な 2～63 文字の規則
`^[a-zA-Z0-9][a-zA-Z0-9_.-]{1,62}$` を持つ。この checkout は Apple 固有の
preflight check をまだ適用しないため（#112）、1 文字の name は library
validation を通過しても Apple で拒否されることがある。

## ファイル取り出しのバックエンド制限

`CopyFileFromContainer` は、安全な file-open semantics を持つ host でのみ
Docker バックエンド経由で利用できます。本ライブラリは materialize された
結果を regular file として検証しますが、すべての Docker host がすべての
container file type を表現できるとは主張しません。Apple Container の
`container cp` には型を保持し symlink を追跡しない copy-out モードが
なく、host 側の検証前に link や special file を dereference/consume する
ことがあります。そのため Apple Container では CLI を起動せず
`ErrCopyFileFromContainerUnsupported` を返します。no-follow と nonblocking
な file open を持たない host でも同じ fail-closed error を返します。Windows
では必要な Windows file flag を `os.OpenFile` が伝播しない Go 1.23 から
1.25 が該当するため、Docker の copy-out には Go 1.26 以降を使ってください。
macOS で安全な copy-out が必要な場合は `CONTAINERGO_BACKEND=docker` を
使ってください。コピー API に渡すコンテナパスは `/` 区切りの POSIX 絶対パス
であり、バックスラッシュは拒否されます。Docker の copy-out には Docker client と
server の両方がバージョン 29.7.0 以上必要です。メソッドは private な一時
ディレクトリの作成や `docker cp` の実行前に両方のバージョンを確認し、最低
バージョンを確認できない場合は `ErrCopyFileFromContainerUnsupported` を返します。
CI の unit と Docker integration job は Linux のみを対象とするため、Windows
の runtime coverage は手動で、Go 1.26 以上と Docker client/server 29.7.0 以上を
使って実行してください。

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
(`go get ...@v0.2.0`)。

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
**Docker バックエンド**: コンテナ IP にはホストから届かないことが多いため
(Docker Desktop)、`WithExposedPorts` で宣言したポートはデーモンが割り当てる
ランダムポートへ自動公開されます(testcontainers と同じモデル)。
ローカルはループバック(`-p 127.0.0.1::<port>`)、リモートの `DOCKER_HOST`
では全IF(`-p 0.0.0.0::<port>`)に束縛します。
リモートとは、このマシンを指さないあらゆる `DOCKER_HOST` のことです。
`tcp://host`(CI での `tcp://docker:2375` など)、`ssh://user@host`、
スキームなしの `host:port` / ホスト名(Docker CLI と同じく `tcp://` を前置)
が該当し、`unix://`、`npipe://`、ループバックアドレス、空値はローカルのままです。
Docker CLI が受け付けるクライアントプロトコルは `unix`、`tcp`、`npipe`、
`ssh` であり、それ以外のスキームは利用可能な `DOCKER_HOST` ではありません。
`Host` は `127.0.0.1`(リモート時はそのホスト名)、`MappedPort` は割り当てられた
ポートを返します。
IPv6 の束縛先は正規化してもアドレスファミリーを維持するため、`::` は
`[::1]` として解決します。
リモートデーモンでは、明示的な loopback 束縛も再利用時の既存 loopback
束縛も拒否します。
これらをリモートの host へ書き換えても実際の待ち受け先には到達できない
ためです。
`ssh://` のホスト名は直接 dial 可能でなければなりません。
CLI の SSH セッションが運ぶのは Docker API だけで、公開ポートは運ばれないため、
ProxyJump や踏み台越しでしか届かないエイリアスは手動の `ssh -L` 転送が必要です。

Docker の `host` と `none` モードは、このライブラリが管理するポート束縛を
作成できません。
`Internal: true` または isolated bridge gateway mode の外部遮断
ネットワークも同じです。
明示的に指定した default 以外の network では、`Run` は image の pull や
コンテナ作成より先にその network を inspect し、これらのネットワークと
`WithExposedPorts` または `WithPublishedPort` を組み合わせた場合は
`*ConfigError` を返します。
このエラーは `ErrInvalidConfig` と一致します。
`host` と `none` の publish 組み合わせは、image やコンテナを起動する前に
拒否されます。

ポート指定なしの host モードは利用できます。
`Host` はクライアントから見たデーモンの host を返しますが、
`MappedPort` と `Endpoint` は host namespace のサービスポートを推測しません。
ライブラリが宣言して束縛したポートが必要です。
`none` モードには到達可能な host がないため、`Host` は
`ErrNoReachableHost` と一致するエラーを返します。
Docker 側で host networking が無効な場合は、推測した endpoint ではなく
バックエンド CLI の開始エラーを返します。
実行時の network 不一致は `ErrNetworkMismatch` で判別できます。

`WithNetwork` を省略した場合、Docker CLI に `--network` を渡さず、
daemon の platform デフォルトに委譲します
(Linux では `bridge`、native Windows では `nat`)。
`Host` と endpoint 解決では inspect の実際の mode と
`NetworkSettings.Networks` を使います。
既存のコンテナが Docker の特別な `default` mode を返す場合も、実際の
network 名に正規化して再利用します。
互換性判定では daemon の server platform も取得し、authoritative な
default を確定します。そのため user-defined な `bridge` や `nat` を
default と誤認しません。identity を取得できない場合や曖昧な場合は
`ErrNetworkMismatch` で失敗します。
`WithReuse` は一致する daemon default を受け付けますが、省略指定を
`host`、`none`、任意の名前付き network の wildcard にはしません。
Docker の handle は `run` が返した immutable な container ID を保持し、
endpoint、Host、lifecycle、reuse の inspect はその ID を対象にします。
network、IP、binding などの dynamic データは毎回更新され、古い snapshot
は再利用されません。

`DOCKER_HOST` のみを host reachability の判定に使用します。
`docker context` 経由のリモート daemon は endpoint host の書き換えには
使用しません。
割り当てはデーモンが起動時に原子的に行うため、並列テストがポートを奪い合う
こともありません。

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
すべての葉戦略は `WithStartupTimeout`（0 は 60 秒）と `WithPollInterval`（0 は 100 ミリ秒、`ForExec` は 250 ミリ秒）を受け付けます。
`ForLog` の poll interval は、パターンが見つかる前にストリームが終了してから再-open するまでの待ち時間です。
`ForAll` と `ForAny` は既定では合成全体のタイムアウトを持ちません。正の `WithStartupTimeout` を指定すると合成全体を制限し、0 または負の値では制限せず各子の戦略のタイムアウトを適用します。

コンテナが stopping、stopped、paused の状態のいずれかになると、待機は即座に失敗します。
created、restarting、unknown と一時的な inspect エラーはタイムアウトまで再試行し、一時的なログストリームの open と EOF は再-open します。終了コードを伴うログストリームエラーは返却します。
成功マーカーは、制限時間内の最終ライフサイクル観測で `Running` が確認された場合のみ受理されます。`ForLog` はワンショットジョブではなく、長時間稼働するサービス向けです。
`ForLog` は、再-open 時に再生されるログの共通部分を除外してから出現回数を累計します。
待機に失敗した場合はロールバック削除し、エラーにログ末尾を添付します。

カスタム戦略向けの `wait.Target` は従来の `Running` メソッドを維持します。
起動中の一時状態を区別できるターゲットでは、任意の `wait.StateTarget` インターフェースも実装してください。
組み込み戦略は `StateTarget` を自動利用し、互換性のため `Running` にフォールバックします。

`ForListeningPort` と `ForExposedPort` は TCP 専用の readiness probe です。
UDP は endpoint 設定には宣言できますが、現在の実装は probe 前に `/udp` を
reject せず TCP dial に渡します。そのため timeout したりする別の TCP listener に
接続する可能性があります。不正な port specification も wait の終了まで
retry されます。#77 を参照してください。

| API | TCP | UDP |
|---|---|---|
| `WithExposedPorts` / `WithPublishedPort` | 現在の option parser が受理 | endpoint / publish 設定では受理 |
| `wait.ForListeningPort` | `PORT` または `PORT/tcp` | UDP probe ではない。UDP declaration を TCP dial する場合がある |
| `wait.ForExposedPort` | protocol にかかわらず最初の宣言を使う | 最初の宣言が UDP の場合も TCP dial に渡される |

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
固有です。Docker は `Tail` と `Since` の両方を受け付けます。確認した
Apple Container CLI の各 version は tail に `-n` を使い、`--since` は
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
`*CLIError` が text に flatten される場合がある（#104）。Docker では、
successful inspect response が空または malformed なら generic parse error を
返すことがある。現在の parser は返された object が要求した ID や name に
一致するかも検証しないため、valid だが mismatched な object は信頼できる
no-match signal ではない（#103）。

`Terminate` が success になるのは、backend が認識した not-found CLI failure
または delete 成功の場合だけなので、冪等性の主張はその範囲に限られる。
empty または malformed な inspect output を not-found に変換せず、現在の
parser は valid だが mismatched な Docker object を no-match として拒否もし
ない。どちらのケースも冪等性の保証には含まれず、unrelated な delete failure
はそのまま返る。

`ForListeningPort` とポート宣言では、対応プロトコルが異なります。

| API | TCP | UDP |
|---|---|---|
| `WithExposedPorts` / `WithPublishedPort` | 対応 | 対応 |
| `wait.ForListeningPort` | `6379` または `6379/tcp` | 接続を試みる前に `*wait.ConfigError` |

`ForListeningPort` は、不正なポート指定にも `*wait.ConfigError` を返します。
エラーメッセージを比較せず分類する場合は、`errors.Is(err, wait.ErrInvalidConfiguration)` を使えます。

## イメージの pull

コンテナを新規作成する必要がある場合、`Run` は起動前に明示的な pull policy
を適用します。

- `PullMissing`（既定値）はローカルストアを検査し、イメージがないときだけ
  明示的に pull します。
- `PullAlways` は Run の実行ごとに明示的な pull を要求します。
  `WithReuse` の attach 前にも共有コンテナを返す前に実行されます。
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

この存在確認は `Run` ごとの daemon round trip である。テスト実行中に
backend のストアが安定していると分かっている場合、
`WithImagePresenceCache` は TTL の間 "present" の答えを再利用し、同じ
image の後続 `Run` では inspect を省略する。Option は一度作って再利用する —
`WithImagePresenceCache` を呼ぶたびに別の空キャッシュになる:

```go
package docexample

import (
    "context"
    "time"

    container "github.com/hirokazumiyaji/container-go"
)

func ImagePresenceCache(ctx context.Context) {
    presence := container.WithImagePresenceCache(5 * time.Minute)
    _, _ = container.Run(ctx, "redis:7-alpine", presence)
    _, _ = container.Run(ctx, "redis:7-alpine", presence) // skips image inspect
}
```

既定では無効で、キャッシュするのは "present" の答えだけである。不在を
キャッシュすると、それが引き起こした pull で無効化する必要が出る。帯域外で
image が消えた場合はエントリ期限まで再 pull されないので、毎回確実に知りたい
呼び出し側はオフのままにする。`PullNever` は契約上 image 不在で失敗するため、
キャッシュの有無に関わらず常に inspect する。

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
   含めます。作成後の env-file cleanup が失敗した場合は、利用可能な
   ハンドルと結合済みエラーを返します。呼び出し側はそのハンドルを確認
   または明示的に `Terminate` してください。
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

`CONTAINERGO_KEEP=1` はプロセス全体の診断用フラグです。`Cleanup` と
`TerminateContainer` による自動削除、reaper 登録を省略し、作成失敗、
`WithFiles` コピー失敗、wait strategy 失敗時の自動 rollback を抑制して、
調査のためにコンテナを残します。明示的な `Container.Terminate` は抑制
されず、`Prune` や `PruneReuseGroup` は引き続きコンテナを削除できます。
`WithReuse` の停止済みコンテナ置換も変わらないため、条件に一致した stopped
reuse container は削除・再作成されます。この変数をグローバルな削除ロック
として扱わないでください。

`container.Prune(ctx)` は、現在の backend の filter が選ぶ、本 library が
作成したコンテナを削除します。Apple は managed コンテナのうち stopped
状態を選びます。Docker は現在 managed コンテナのうち exited 状態だけ
を選ぶため、dead 状態のコンテナは #113 を適用するまで削除されません。
この filter は running または created 状態を選びません。Apple では、各
candidate は削除前に安定した名前単位 lock の下で再 inspect され、世代、
session、管理対象ラベル、および停止状態が再確認されます。

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
  別の名前を使う。`WithFiles` は attach caller を含むすべての reuse caller
  でコピーされます。attach 時のコピー失敗は共有コンテナを削除せずにエラーを
  返します。`PullAlways` は attach 前にも各 caller で実行されます。
- このチェックアウトが作成するコンテナは通常 generation label を持ちます。
  ただし、既存の reuse コンテナでは現在の check が managed label と
  creation label のすべてを要求せず、`WithReuse` marker と互換 image
  だけを要求します。generation が空の場合、name ベースの delete に
  到達する可能性があります。現在の code は readiness 後に generation
  を再確認しません。#83 と #84 がこれらの fail-open 経路を扱います。
  適用されるまでは、reuse を信頼できない same-name replacement への
  保護として扱わないでください。`docker run` が出力した完全 ID を保持する
  Docker handle はその ID で削除します。inspect から得た ID は target validation
  に依存し、現在の Docker parser はその検証を行わない（#103）。他の operation
  は #74 を適用するまで logical name を使います。
- `Cleanup` / `TerminateContainer` / watchdog reaper は reused handle を
  削除しない。明示的な `ctr.Terminate` だけが共有コンテナを削除できる。
- `CONTAINERGO_KEEP=1` はこの reuse contract を変えない。条件に一致した
  stopped reuse container は削除・再作成され、`PruneReuseGroup` はその
  group を削除できる。
- 名前単位の `flock` は generation-checked な通常の `Terminate` /
  failed-create cleanup 経路、および Apple の `Prune` と `PruneReuseGroup`
  を保護する。外部 reaper の inspect / delete window にはこの lock は使われない
  ため、外部ツールによる協調していない CLI 操作は非協調として扱う。
- `container.PruneReuseGroup(ctx, "integration")` はその group の
  コンテナを強制削除する（CI teardown）。group は再利用 key ではなく
  label である。Apple Container では、削除前に名前単位 lock の下で fresh
  candidate 検査（世代、session、managed/reuse/group ラベル、running/stopped
  状態）を適用する。通常の `Prune` は上記の backend filter を使い、現在の
  Docker backend では exited コンテナだけが対象で、dead は対象外である。
- `WithName` 必須。待機戦略は attach 時も必ず再実行する。
- 競合する create の名前衝突は成功として扱い、既存へ attach する。
- stopped の残骸は削除して再作成する。running のまま ready にならない
  場合は削除せずエラーを返す。
- image / port が既存と不一致なら分かりやすいエラーを返す。
  Docker では network も一致する必要があります。
  `WithNetwork` 省略時は daemon の platform default を `default` として
  扱い、inspect の `NetworkSettings.Networks` から `bridge` (Linux) または
  `nat` (native Windows) を照合します。`host`、`none`、名前付き network は
  wildcard にしません。
  `env` / `cmd` / `mounts` の差は既存へ黙って attach する仕様です。
- 各作成は世代ラベルを持ち、`Terminate` と stopped 再作成経路は置き換わった世代の削除を拒否する。watchdog リーパーも同様にガードする。
- `Cleanup` / `TerminateContainer` / watchdog リーパーは reused ハンドルを
  削除しない。明示的な `ctr.Terminate` だけが共有コンテナを消し得る。
- `container.PruneReuseGroup(ctx, "integration")` はそのグループの
  コンテナを強制削除する(CI 終了時)。通常の `Prune` は stopped のみ。

ライブラリはテスト間のアプリケーションデータを自動初期化しません。
key prefix、schema 分離、`Exec` による reset（`FLUSHALL` など）を使って
ください。

## セキュリティ上の注意

- すべての CLI 呼び出しは argv 配列で行い、シェルを経由しません。唯一の
  shell script（reaper）は固定文字列で、検証済みの container name と、
  #73 適用後の Docker ID だけを stdin data として受け取ります。
- Unix では環境変数を、正規化済みの `os.UserCacheDir()` 配下の、所有者と
  モードを検証した 0700 ディレクトリ内の 0600 ファイル経由で渡します。
  既存の symlink 祖先は一度だけ解決し、`..` と書き込み可能な信頼できない
  祖先は拒否します。`TMPDIR` は使用しません。
  24 時間経過した staging ディレクトリだけを age で回収し、marker 作成後、
  lock 作成前に終了した状態や tombstone 化した部分削除は自動修復します。
  初期化済みディレクトリは age ではなく書き込み側 lock で生存を判断します。
  marker が不正な場合、置き換えられた場合、許可されない子がある場合は
  fail closed します。利用者が内容を確認して手動で削除してください。
  cleanup 失敗は API から返し、関数から返る前に deferred retry します。
- Windows の Go `chmod` はユーザー単位の秘密性を保証しません。
  空でない環境変数指定を含む `Run`/`Exec` は `ErrEnvFileUnsupported` で
  fail closed します。
  それ以外の Windows 機能は利用できます。
- 環境変数キーは、空でない有効な UTF-8 であり、`=`、Unicode 空白、制御
  文字、先頭の `#`、先頭 BOM を含むできません。
  値は有効な UTF-8 であり、Unicode 制御文字、NUL、CR/LF、U+2028、U+2029
  を含むできません。
  制御文字でない Unicode、空白、`=` は値として許容します。
  制御文字(タブを含む)や不正な UTF-8 を拒否する方針は、backend が受け付ける
  値でも env ファイルには書かないという意図的な互換性変更です。
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

リポジトリ CI の unit/race job は Ubuntu で実行します。
Windows 固有の環境変数のテストは開発時にコンパイル確認しますが、
この変更では Windows 実行時 CI のカバレッジを主張しません。

Design document: [docs/design.md](docs/design.md)（日本語版:
[docs/design.ja.md](docs/design.ja.md)）。設計ドキュメントの「実装フェーズ」は
履歴であり、現在の API 契約ではありません。

コントリビューション: [CONTRIBUTING.md](CONTRIBUTING.md)。セキュリティ報告:
[SECURITY.md](SECURITY.md)。

## ライセンス

MIT
