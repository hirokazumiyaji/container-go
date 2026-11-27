# container-go

English (primary): [README.md](README.md)

[Apple Container](https://github.com/apple/container) と Docker を実行基盤とする、
[testcontainers](https://testcontainers.com/) スタイルの Go ライブラリです。
Go のテストから使い捨てコンテナを起動できます。コンテナクライアントのサードパーティ依存はありません。唯一の非標準 Go モジュールはプラットフォームの process と file 操作に使う `golang.org/x/sys` で、それ以外は標準ライブラリだけで実装しています。

## バージョンの適用範囲

最新のタグ付きリリースは `v0.3.0`（2026-10-26）です。
このチェックアウトは、そのタグより後の開発版です。
下記のインストールコマンドはリリース版 API を導入します。
`v0.3.0` には [CHANGELOG.md](CHANGELOG.md) の `0.3.0` 節に記載した API と修正が含まれます。

root ライブラリのチェックアウトには Go 1.26 以降が必要です。
`make bench-integration` が使う nested `bench/` モジュールには
Go 1.26 以降が必要です。`v0.3.0` モジュールにも Go 1.26 以降が必要です。

コード例は単独ファイルとしてリポジトリのドキュメントテストがコンパイルします。

現在のチェックアウトには、次の意図的な制約があります。

- すべてのプラットフォームで Go ネイティブの watchdog リーパーを使います。
  ライブラリ自身の実行ファイルを再実行して、親の pipe が閉じた後に Docker の
  immutable ID を回収し、name-addressed な Docker create は inspect が返す
  immutable ID で回収します。`/bin/sh` の helper はありません。Windows では
  reaper 子プロセスが専用の非表示コンソールプロセスグループで動くため、
  コンソールの Ctrl+C では親と一緒に終了しません。また reaper は Job Object に
  割り当てられ、reaper とそれが起動した全プロセスが同じ teardown 境界を共有
  するため、停止時はツリーごと終了します。reaper は EOF まで backend コマンドを
  起動しないため、これは多層防御であり、観測されたリークの修正ではありません。
  job は `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` を設定しないため、親の死亡で
  カーネルが親の handle を閉じても watchdog は残って reap できます。
  remote daemon や self-exec できない build では、明示的な `Terminate`、
  `Cleanup`、`TerminateContainer`、`Prune`、および rollback 経路に依存します。
  Windows では `WithEnv` と `WithExecEnv` が現在のログオンだけに許可した DACL で
  環境ファイルを書き、Docker の copy-out には Go 1.26 以降が必要です。
- network の作成とライフサイクル管理はスコープ外です。`WithNetwork` は既存の
  network へ接続します。volume の作成とライフサイクル管理もスコープ外で、
  `WithMounts` は bind、名前付き volume、tmpfs の mount を受け付けます。
  ライブラリが匿名 volume を作ることはありません。
- remote Docker daemon に対する bind mount は `ErrUnsupportedCapability` で
  拒否します。daemon は source path を自分の host で解決するため、client 側の
  path を検証できないからです。ローカル Docker では Windows の host path を
  受け付けます。
- Apple Container は `PullNever` を実行できず
  (`ErrPullNeverUnsupported`)、型を保持する copy-out もありません
  (`CopyFileFromContainer` は `ErrCopyFileFromContainerUnsupported` を返す)。
  `--since` log option もないため、`LogsWithOptions` は `Tail` を `-n` に
  対応させ、`Since` を `ErrUnsupportedCapability` で拒否します。Apple Container
  には healthcheck や wait primitive もないため、readiness は client 側で
  probe します。
- Dockerfile からの build はスコープ外です。`container build` または
  `docker build` を直接使ってください。

以下の節は、これらの制約を文脈とともに説明します。

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
| Linux | Docker | docker CLI 29.x（29.7.2 を確認）と稼働中のデーモン。copy-out には client/server 29.7.0 以上が必要 |
| Windows | Docker | docker CLI 29.x（29.7.2 を確認）と稼働中のデーモン。copy-out には client/server 29.7.0 以上が必要（Docker コンテナはネイティブ watchdog リーパーで回収。name-addressed create も inspect の immutable ID 経由で回収。remote daemon は `TestMain` で `Prune` を実行。クリーンアップの契約を参照） |

macOS で Docker（Docker Desktop など）を使う場合は
`CONTAINERGO_BACKEND=docker` を、Apple Container を明示する場合は
`CONTAINERGO_BACKEND=apple` を設定します。

watchdog リーパーは Docker コンテナを immutable ID で回収し、name-addressed
create も inspect が返す immutable ID で回収します。テストプロセスのローカルな
子プロセスなので remote daemon はローカルのテスト環境より長く残り、self-exec
できない build では reaper 自体がありません。リーパーによる残骸の回収を期待せず、
クリーンアップの契約に従って `TestMain` の前後で `Prune` を実行してください。

本ライブラリはバックエンドの CLI（`container` / `docker`）を子プロセスとして
呼び出します。cgo もデーモン API クライアントも使いません。
`DOCKER_HOST`、コンテキスト、レジストリ認証は docker CLI 自身が解決します。
ライブラリは `DOCKER_HOST` と Docker context（アクティブまたは明示設定）の
両方から remote Docker endpoint を検出・解決します。

ルートモジュールには Go 1.26 以上が必要です。オプションの
ネストした `bench/` ベンチマークモジュールは Go 1.26 以上が必要です
(root module と testcontainers-go の依存関係の要件)。詳細は
[bench/README.md](bench/README.md) を参照してください。

動作確認済みのバックエンド(本ライブラリがマッチする CLI stderr 文言と
inspect JSON 形状):

| バックエンド | このリポジトリで確認した根拠 |
|---|---|
| Apple Container CLI | 1.2.2 / 1.3.0 の source・help・inspect fixture |
| Docker Engine / CLI | 29.x。29.x 形式の inspect fixture とローカルの 29.7.2。安全な copy-out には client/server 29.7.0 以上が必要 |

Apple の API サーババージョンは `container system version` または
`container system status` から実行時に取得し、CLI バージョンから推測しません。
オプトインした live matrix は、サーババージョンを取得できない場合や CLI と
異なる場合に skip するため、未確認の API サーバ互換範囲を主張しません。
この表はリポジトリの根拠を示すもので、すべての release を確認済みだという
互換性宣言ではありません。新しい CLI ではエラー文言や JSON フィールドが
変わる可能性があります。stderr マッチャは `engine_apple_errors.go` /
`engine_docker_errors.go` にあり、`internal/inspect/testdata/`・`testdata/` の
フィクスチャを参照してください。

`WithName` と name-addressed reaper entry は共有の library guard
`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`（1～63 文字）を使う。これは保守的な安全
規則であり、Docker または Apple の完全な name grammar を述べるものではない。
Apple Container の CLI はより厳密な 2～63 文字の規則
`^[a-zA-Z0-9][a-zA-Z0-9_.-]{1,62}$` を持ち、Apple バックエンドは image 解決の
前にこれを適用するため、1 文字の name は pull 後ではなく事前に拒否される。

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
CI の unit test は Linux、macOS、Windows で実行し、Docker integration job は Linux で実行します。
Windows での実際の Docker copy-out は、Go 1.26 以上と Docker client/server 29.7.0 以上を使って手動で統合テストしてください。

## インストール

最新のタグ付きリリースを導入する場合:

```text
go get github.com/hirokazumiyaji/container-go@v0.3.0
```

`v0.3.0` のリリース版 API が導入されます。
このチェックアウトにはタグ以降の変更が含まれることがあるため、
再現可能なビルドには上記のようにモジュールバージョンを明示してください。

## API の安定性

1.0 未満では、マイナーリリース（0.x）に破壊的変更が含まれる場合があります。
再現可能なビルドにはモジュールバージョンを明示して pin してください
(`go get ...@v0.3.0`)。

ルートの `container` パッケージ（`Run`、オプション、ライフサイクルヘルパー）が
主な統合面で、マイナーシリーズ内では比較的安定を目指します。
`wait` パッケージも公開 API ですが、カスタム strategy 向けのインターフェース
（特に [`wait.Target`](wait/wait.go)）はバックエンドやプローブ要件の変化に応じて
変更される可能性があります。可能なら組み込み strategy を使ってください。

[CHANGELOG.md](CHANGELOG.md) も参照してください。
`0.3.0` の節はタグ付きリリースを説明し、`Unreleased` の節は
そのタグより後の変更を説明します。

## 出力の契約

CLI の出力は bounded または caller 所有の sink へ流します。
`LogsOptions.Tail` は backend の**行単位の末尾**(Docker は `--tail`、Apple は `-n`)で、
`MaxBytes` はその後に適用される**先頭 byte の prefix 上限**です。
つまり `MaxBytes` は stdout/stderr の結合到着ストリームの先頭を保持し、
末尾を保持する tail ではありません。`WithExecMaxBytes(n)` も同じ
prefix ポリシーを `Exec` に適用します。旧 unbounded reader は stdout の後に stderr を連結し、bounded capture は到着順を使います。
`ExecTo` と `LogsTo` は `io.Writer` へ直接ストリームし、
`OutputStats` が破棄前の総観測量と truncation を返します。writer が
失敗した場合は、子プロセスの終了が成功でも出力 delivery エラーを必ず
返します。CLI の終端エラーも同時に発生した場合は同じ error chain に
保持します。分類は `errors.Is(err, container.ErrOutputDelivery)` で確認し、
元の writer error も chain に残ります。
`WithExecStderr(w)` は exec した process の stderr を stdout から分離します。
`Exec` の戻り値 reader は stdout だけになり、`ExecTo` は stdout を output、
stderr を `w` へ流します。この場合 `WithExecMaxBytes` は stdout だけを制限します。
stdout と stderr に同じ比較可能な writer 値（通常は同じポインタ）を渡すこともでき、
その場合は書き込みが直列化されます。異なる wrapper や比較不能な動的型は
同じ writer として検出されず、直列化されません。
bounded reader は `Truncated() bool` を提供し、保持したデータを返した後の
terminal read error が `ErrOutputTruncated` になります。backend の
infrastructure、cancellation、launch、missing-container エラー時も
partial reader と truncation state を返します。`FollowLogs` は長期
ストリームなので自動 byte 上限はありません。`wait.ForExec` は
出力 matcher を指定しない限り各 poll の出力を `io.Discard` へ送ります。
matcher を指定した場合、container adapter は matcher 用に最大 64KiB だけ
保持します。失敗診断のログは固定長 1MiB の末尾リングに保持します。
internal `CLIError` は失敗した invocation ごとに分類用の
末尾 stderr ウィンドウを有界に保持し、`Error` はそのウィンドウを
秘匿化してから末尾最大 64KiB を描画します。これは `Run` の全量出力や
caller-owned sink の上限ではありません。

`Logs`、`WithExecMaxBytes` を指定しない `Exec`、`MaxBytes == 0` は
ソース互換性のため従来の全量保持を維持します。これは信頼できない
出力には deprecated であり、新コードでは正の制限値または
`ExecTo` / `LogsTo` を選んでください。
`LogsOptions` に `MaxBytes` を追加したため、Go の構造体の性質上、旧
2 要素 unkeyed literal はコンパイルできません。`LogsOptions{Tail: ...,
Since: ...}` の keyed literal へ移行してください。これは pre-1.0 の
意図的な source compatibility tradeoff として明記しています。
大きな出力の integration test は bounded forwarding と draining のみを
確認し、peak host memory は外部の RSS sampler なしには測定しません。

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
loopback（`-p 127.0.0.1::<port>`）、リモートの `DOCKER_HOST` では
全インターフェース（`-p 0.0.0.0::<port>`）へ束縛します。
リモートとは、このマシンを指さないあらゆる `DOCKER_HOST` のことです。
`tcp://host`（CI での `tcp://docker:2375` など）、`ssh://user@host`、
スキームなしの `host:port` / ホスト名（Docker CLI と同じく `tcp://` を前置）
が該当し、`unix://`、`npipe://`、ループバックアドレス、空値はローカルのままです。
Docker CLI が受け付けるクライアントプロトコルは `unix`、`tcp`、`npipe`、
`ssh` であり、それ以外のスキームは利用可能な `DOCKER_HOST` ではありません。
`DOCKER_HOST` と Docker context（アクティブまたは明示設定）の両方を remote
daemon として検出し、解決します。`Host` は `127.0.0.1`（リモート時はその
ホスト名）、`MappedPort` は割り当てられたポートを返します。
割り当てをデーモンが起動時に原子的に行うため、並列テストがポートを
奪い合うことはありません。
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
endpoint の読み取りは後述の短い TTL の dynamic snapshot を共有し、
lifecycle と reuse の確認では更新します。

並列テストで同じ論理名を使いたい場合は、テストごとに user-defined
network を作成し（`docker network create` など）、`t.Cleanup` で削除した
うえで、その `WithNetwork` と `WithNetworkAliases("db")` を渡します。
各コンテナは自分の network 内だけで `db` として解決され、他のテストや
container 名と衝突しません。network alias には明示的な user-defined
network が必要です。Docker の default bridge、`host`、`none`、`nat` は
alias を解決しないため、`Run` は backend 呼び出し前に `*ConfigError` で
拒否します。alias は reuse の互換性判定にも含まれるため、alias を持たない
既存コンテナへは attach しません。ライブラリは既存 network へ接続するだけで、
network の作成・削除は行いません。

`DOCKER_HOST` と Docker context の両方を host reachability と endpoint host の
解決に使います。

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

endpoint 関連メソッドは、同一性を検証した短い（約 1 秒）dynamic inspect
スナップショットを `Shared()` handle 間で共有し、readiness の retry ごとに
backend CLI を起動しないようにします。キャッシュした network、IP、binding は
TTL で失効し、`State` は常に再 inspect します。Docker が複数 network を
報告し top-level address がない場合、network 名の辞書順で最初に見つかる
空でない IP address を使います。

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
ビルダーは新しい戦略を返し、レシーバを変更しません。既存の値を再設定する場合は戻り値を代入してください（`s = s.WithHeader("X-Tenant", tenant)`）。

現在の開発チェックアウトは、Docker の `HEALTHCHECK` 状態を待つ
`wait.ForHealthcheck()`、任意のコールバックで readiness を判定する
`wait.ForFunc`、`database/sql` の probe を行う `wait.ForSQL` も追加して
います。`ForSQL` は driver を同梱せず、呼び出し側が driver を import して
登録し、その名前を渡します。未登録の driver はコンテナ作成前の検証で拒否され、
各 SQL probe の試行には deadline があり、応答しない handshake は再試行されます。`HTTPStrategy` には単一のステータスコードを
受理する `WithStatusCode` と、リクエストボディを送る `WithBody` を、
`ExecStrategy` にはコマンド出力を判定する `WithResponseMatcher` /
`WithOutputContains` を追加しています。出力 matcher は任意の
`wait.ExecOutputTarget` を実装する target を必要とし、container adapter は
最大 64KiB のコマンド出力を保持します。ヘルスチェック戦略は Docker 専用で
あり、Apple バックエンドはコンテナを作成する前にこの戦略を拒否します。

コンテナが stopping、stopped、paused の状態のいずれかになると、待機は即座に失敗します。
created、restarting、unknown と一時的な inspect エラーはタイムアウトまで再試行し、一時的なログストリームの open と EOF は再-open します。終了コードを伴うログストリームエラーは返却します。
成功マーカーは、制限時間内の最終ライフサイクル観測で `Running` が確認された場合のみ受理されます。`ForLog` はワンショットジョブではなく、長時間稼働するサービス向けです。
稼働中の follow ストリームでマーカーを観測した場合は追加の settle 待機をせずに受理し、CLI が既に終了しているストリームは終端エラーを隠さないよう drain してから受理します。
`ForLog` は、再-open 時に再生されるログの共通部分を除外してから出現回数を累計します。
待機に失敗した場合はロールバック削除し、エラーにログ末尾を添付します。

カスタム戦略向けの `wait.Target` は従来の `Running` メソッドを維持します。
起動中の一時状態を区別できるターゲットでは、任意の `wait.StateTarget` インターフェースも実装してください。
組み込み戦略は `StateTarget` を自動利用し、互換性のため `Running` にフォールバックします。
Docker のヘルスチェック待機では、任意の `wait.HealthTarget` インターフェースも使います。
`ForHealthcheck` はこれを実装しないターゲットに `*wait.ConfigError` を返します。
`ForExec` の出力 matcher は任意の `wait.ExecOutputTarget` インターフェースを
同様に使い、実装しないターゲットには `*wait.ConfigError` を返します。

Docker では、公開ポートへの TCP 接続が成功してもコンテナ内のプロセスが
listen しているとは限りません。`docker-proxy` や Docker Desktop の port
forwarder が先に handshake を完了するためです。そのため
`ForListeningPort` と `ForExposedPort` は、host 側の接続成功後に、宣言した
コンテナ側ポートが `LISTEN` 状態であることを確認します。この確認は
`exec` 経由で `/proc/net/tcp` と `/proc/net/tcp6` を読み取ります。テーブルは
逐次スキャンするため、IPv4 の接続テーブルが大きくても IPv6 側が切り捨て
られることはありません。確認を
実行できない場合（Apple Container は VM の endpoint に直接接続するため
不要です。distroless など `cat` を持たないイメージや、その他の未対応
ターゲットも同様です）は、host 側の接続確認だけにフォールバックします。
このフォールバックは best-effort です。コンテナ側の確認が利用できない
ために従来の host 側だけの判定に戻るのであり、ポートが ready であることを
確認したわけではありません。コンテナ側の確認を明示的に省略するには
`WithHostOnly()` を使います。

`ForListeningPort` と `ForExposedPort` は TCP 専用の readiness probe です。
`ForListeningPort` は `/udp` と不正な port specification を probe 前に
`*wait.ConfigError` で拒否します。`ForExposedPort` は `WithExposedPorts` で
最初に宣言した TCP port を選び、TCP の exposed 宣言がなければ最初の TCP
published port にフォールバックします。UDP だけの宣言が TCP dial に渡ることは
ありません。

| API | TCP | UDP |
|---|---|---|
| `WithExposedPorts` / `WithPublishedPort` | 受理 | endpoint / publish 設定では受理 |
| `wait.ForListeningPort` | `PORT` または `PORT/tcp` | probe 前に `*wait.ConfigError` |
| `wait.ForExposedPort` | 最初の TCP exposed port、なければ最初の TCP published port | skip。TCP 宣言がなければ `ErrPortNotExposed` |

### Stop の timeout

nil の `Container.Stop` timeout は backend CLI の既定の grace period を使います。
非 nil の timeout は非負でなければならず、negative の場合は CLI 呼び出し前に
`*container.ValidationError` を返します。両 backend は whole seconds を受け付ける
ため、正の sub-second timeout は切り上げ（切り捨てない）、0 は即時終了を要求し、
丸めた秒数が backend の limit（確認した両 CLI では `math.MaxInt32` 秒）を超える
値は CLI 呼び出し前に拒否されます。timeout は操作の context budget にも加算
されます。

`CHANGELOG.md` の `0.3.0` 節に記載された次の wait API は
このチェックアウトに含まれ、`v0.2.0` にはありません。

```go
package docexample

import (
    "crypto/tls"
    "net/http"
    "time"

    "github.com/hirokazumiyaji/container-go/wait"
)

func ReleasedWaitOptions() {
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

現在の開発チェックアウトには、`v0.3.0` に加えて次の API が追加されています。

```go
package docexample

import (
    "context"
    "fmt"
    "net/http"
    "time"

    "github.com/hirokazumiyaji/container-go/wait"
)

func DevelopmentWaitOptions() {
    _ = wait.ForHTTP("/health").
        WithStatusCode(http.StatusNoContent).
        WithBody([]byte(`{"probe":true}`)).
        WithStartupTimeout(time.Second)
    _ = wait.ForExec([]string{"pg_isready"}).
        WithOutputContains("accepting connections")
    _ = wait.ForFunc(func(context.Context, wait.Target) error { return nil })
    _ = wait.ForSQL("5432/tcp", "postgres", func(host string, port int) string {
        return fmt.Sprintf(
            "host=%s port=%d user=postgres password=postgres sslmode=disable",
            host, port,
        )
    })
}
```

新規作成し再利用していないコンテナで待機に失敗した場合、コンテナをロールバックし、上限付き log の取得が成功した場合だけ 1 MiB 上限の末尾をエラーに付けます。再利用コンテナは、後述の共有ライフタイムの終わりまで残します。

## ログ

`Logs` と、成功する `LogsWithOptions` 呼び出しは CLI process が終了するため
有限のスナップショットを返します。`Logs` は利用可能な出力をすべて要求し、
byte 上限を課しません。`LogsWithOptions` は `LogsOptions.MaxBytes` が正の
ときだけ byte 上限を課し、結合出力の先頭から最大その byte 数を保持します
（前述の出力契約と同じ prefix 上限）。
`LogsOptions` は backend
固有です。Docker は `Tail` と `Since` の両方を受け付けます。確認した
Apple Container CLI の各 version は tail に `-n` を使い、`--since` を
実装していないため、Apple backend は `Tail` を `-n` に対応させ、`Since` を
`ErrUnsupportedCapability` で拒否します。範囲を指定しても大きさは
コンテナ出力に依存するため、出力が大きい、または信頼できない場合は
正の `MaxBytes` を指定してください。
`FollowLogs` はストリーミングする `io.ReadCloser` を返し、Close するか
context をキャンセルするとバックエンド CLI を停止します。`ForLog` は
`FollowLogs` を使い、`Logs` は新しい出力を追尾しません。
`LogsOptions` と `LogsWithOptions` は `CHANGELOG.md` の `0.3.0` 節に
記載されています。
`LogsOptions.Tail` と `MaxBytes` は非負として検証され、
`PruneReuseGroup` は `WithReuseGroup` と同じ reuse-group grammar を使います。

次の例は Docker 固有です。`v0.2.0` より後に追加された log stream、exec
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

これらの error check は path ごとに異なります。liveness 分類でも元の
`*CLIError` は `ErrSystemNotRunning` の error chain に保持されるため、
`errors.As` で引き続き取得できます。Docker では、要求した ID や name に
一致する entry がない inspect response は `ErrContainerNotFound` を返します。
empty な output も not-found として扱い、malformed な output は沈黙した
no-match ではなく decode error になります。

`Terminate` は backend が認識した not-found CLI failure を success として
扱うため、冪等性の主張は not-found と delete 成功を対象にします。
malformed な inspect response は沈黙した success ではなく error になり、
unrelated な delete failure はそのまま返ります。

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
- `PullNever` は Docker では strict な no-fetch policy で、`--pull=never` を
  渡し、image がない場合は `ErrImageNotFound` を返します。Apple Container には
  同等の run-time switch がないため、`Run` は `ErrPullNeverUnsupported` で
  この policy を拒否します。

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
    // Docker は image 不在を ErrImageNotFound で、Apple Container は
    // PullNever を ErrPullNeverUnsupported で報告します。
    if errors.Is(err, container.ErrImageNotFound) ||
        errors.Is(err, container.ErrPullNeverUnsupported) {
        return
    }
    if err != nil {
        t.Fatal(err)
    }

    _ = container.Pull(ctx, "redis:7-alpine")
}
```

`PullNever` の厳密な no-fetch 保証は Docker バックエンドだけです。Apple
Container の `container run` には CLI 自身のイメージ解決を止めるスイッチが
ないため、ライブラリはレジストリへの暗黙の fetch を許さず、CLI 呼び出し前に
拒否します。Apple では `PullMissing`(既定)または `PullAlways` を fallback と
して使います。ただし `PullMissing` は image がなければ pull するため、
no-network 保証ではありません。

この存在確認は `Run` ごとの daemon round trip である。テスト実行中に
backend のストアが安定していると分かっている場合、
`WithImagePresenceCache` は TTL の間 "present" の答えを再利用し、同じ
image の後続 `Run` では inspect を省略する。Option は一度作って再利用する —
`WithImagePresenceCache` を呼ぶたびに別の空キャッシュになる。
キャッシュは image/store/platform の組を最大256件保持する。上限では失効時刻が最も早いエントリを削除するため、TTL 内でも再 inspect する場合がある:

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
呼び出し側はオフのままにする。Docker の `PullNever` は契約上 image 不在で失敗するため、
キャッシュの有無に関わらず常に inspect する。

## 実行オプション

`WithCPUs(v float64)` は `0.5` や `1.25` のような小数の CPU quota を受け付け、Docker には `--cpus` として渡します。zero 以下と非有限値は option 評価時に拒否します。Apple Container の `--cpus` は整数のみを受け付けるため、小数は `ErrUnsupportedCapability` で拒否し、整数はそのまま動作します。

`WithMemory(size string)` と `WithShmSize(size string)` は、整数と任意の `K`/`M`/`G`/`T`/`P` suffix という同じ文法を使い、zero、不正な値、overflow を backend 呼び出し前に拒否します。`WithShmSize` は Docker と Apple Container の `--shm-size` に渡します。

その他の option は run flag に直接対応します。`WithPrivileged`（`--privileged`）、`WithCapAdd` / `WithCapDrop`（`--cap-add` / `--cap-drop`。名前は optional な `CAP_` prefix 付きの大文字または `ALL` で、caller の表記をそのまま渡し、重複は prefix に関係なく無視する）、`WithUlimit(name, soft, hard)`（`--ulimit name=soft:hard`。Docker の小文字名集合を使い、`-1` は無制限を表す）、`WithExtraHost(host, ip)`（`--add-host host:ip`。IP に `host-gateway` を許可し、同名または衝突する host は拒否する）、`WithHostname`（`--hostname`。63 byte 以下の RFC 1123 hostname）である。先頭が `-` の文字列値や制御文字を含む値は拒否します。

残りの option は security と process に関わる run flag に対応します。`WithReadOnlyRootFS()` は `--read-only` を渡し、コンテナのルート filesystem を読み取り専用にする。`WithInit()` は `--init` を渡し、signal を転送して孤児 process を回収する軽量 init を有効にする。`WithDNS(servers ...string)` は server ごとに `--dns` を 1 つ渡す。各 server は IPv4 または IPv6 の IP address リテラルでなければならず、空文字、不正な値、flag 形式の値は option 評価時に拒否する。`WithStopSignal(signal string)` は `--stop-signal` を渡し、1〜64 の Linux signal 番号、`SIGTERM` や `TERM` のような標準の大文字 signal 名、または `SIGRTMIN+3` や `SIGRTMAX-1` のような realtime 表記を受け付ける。小文字、未知の名前、範囲外の番号、flag 形式の値は拒否する。Docker は 4 つすべてをサポートする。

Apple Container は `--shm-size`、`--cap-add`、`--cap-drop`、`--ulimit` をサポートするため同じ flag を受け取ります。`--read-only`、`--init`、繰り返し指定可能な `--dns` もサポートするため、`WithReadOnlyRootFS`、`WithInit`、`WithDNS` はそのまま渡されます。privileged container、extra host、`--hostname`、実行時の stop signal に相当する機能はないため、`WithPrivileged`、`WithExtraHost`、`WithHostname`、`WithStopSignal` は image や container の command を実行する前に `ErrUnsupportedCapability` を返します。

これらの option は `WithReuse` の互換性 identity に含まれます。再利用する container は CPU、shm、privilege、ulimit、extra host、hostname の設定が一致しなければなりません。capability は requested-subset rule で、caller が追加・削除を要求した capability がすべて存在する必要があり、capability の変更を要求しない caller は inspect された任意の set を受け入れます。read-only root filesystem、init、DNS server のリスト、stop signal は作成 fingerprint に記録されるため、異なる値（または option を省略）で作成された既存コンテナへの attach は `ErrReuseMismatch` を返します。環境変数の値は `WithReuseEnvFingerprint` が別に扱います。これらの check に失敗すると `Run` は attach を拒否します。

### Apple の capability validation

Apple backend は image の解決や container create の前に静的 capability を検証します。
Apple が受理する platform grammar は `os/arch[/variant]` で、bare `linux` や
選択した architecture に定義されていない variant を拒否します。bare な
`armel` は `linux/armel/v6` と同じ `v6`、bare な `arm`/`armhf` は `v7` を意味します。platform
未指定時の `CONTAINER_DEFAULT_PLATFORM` は実効 platform として解決し、image
inspect、pull flight、pull、run で同じ値を使います。明示した `WithPlatform`
が優先されます。これは Apple CLI の設定なので Docker では消費しません。
Apple 固有の grammar も Docker には適用しません。

live matrix は `CONTAINERGO_BACKEND=apple` と `CONTAINERGO_APPLE_LIVE=1` の
両方を明示した場合だけ実行します。一意な name と ownership label を使い、
label が一致しない container は削除しません。

```sh
CONTAINERGO_BACKEND=apple CONTAINERGO_APPLE_LIVE=1 \
  go test -tags integration -run TestIntegrationAppleCapabilityMatrix ./...
```

`container name` は 2〜63 文字で
`^[a-zA-Z0-9][a-zA-Z0-9_.-]{1,62}$` に一致する必要があります。network name は小文字の 1〜63 文字です。
公開 API の `WithNetwork` は MAC/MTU の comma 付き property を意図的に受け付けません。
memory は 200 MiB 以上で、`WithMemory` と同じ整数(任意の `K`/`M`/`G`/`T`/`P` suffix)表記を使い、unit 誤りや overflow を拒否します。
publish port は 2〜65535、publish descriptor は最大 64 個です。
`WithExposedPorts` は library 側の宣言であり、Apple の `--publish` flag には変換されません。

Apple は `--shm-size`、`--cap-add`、`--cap-drop`、`--ulimit`、`--read-only`、`--init`、繰り返し指定可能な `--dns`、整数の `--cpus` をサポートするため、`WithShmSize`、`WithCapAdd`、`WithCapDrop`、`WithUlimit`、`WithReadOnlyRootFS`、`WithInit`、`WithDNS`、整数の `WithCPUs` はそのまま渡されます。`WithPrivileged`、`WithExtraHost`、`WithHostname`、`WithStopSignal`、小数の `WithCPUs` は CLI 実行前に `ErrUnsupportedCapability` で拒否されます。

## イメージの解決と固定

`Run` は `PullMissing` と `PullAlways` の両方で、image inspect の後に解決した image identity を backend の `run` に渡します。
Docker は repository が一致する `RepoDigests` を優先し、取得できない場合は inspect した local image `Id` にフォールバックします。
別の repository の `RepoDigests` を持つ local tag は alias として扱い、その immutable `Id` で安全に実行します。
明示的な `image@digest` の repository または digest が一致しない場合は `ErrImageIdentityMismatch` になります。
Apple Container は root image descriptor を run / reuse の identity として使い、指定した platform の variant は別の検証情報として保持します。
descriptor が利用可能な ID だけの record は digest 参照に正規化します。
Apple が未指定の入力を custom default registry へ解決した場合、`Run` は pinned 参照と reuse 比較に backend が報告した canonical repository を保持します。
Reuse は元の mutable tag ではなく、解決済み digest / image ID で互換性を判定します。
作成後の検証に失敗した新規 reuse container は削除します。

Apple Container には実行時の `--pull=never` がないため、`Run` は `PullMissing` と `PullAlways` で、pinned された Apple reference を `container run` に渡す前に local store にあるかを確認します。
存在せず選択した policy が fetch を許可する場合は exact digest を明示的に pull してから再確認します。
`container run` に暗黙の fetch をさせません。
identity を取得できない inspect の成功は `ErrImageIdentityUnavailable`、local に存在しないことが確認された場合は `ErrImageIdentityNotLocal` です。
addressability check の transport、permission、context cancellation エラーは元の operational error として返し、mutable fallback では隠しません。
identity を取得できない、または resolved reference が local にない mutable input では、互換 fallback として `WithAllowMutableImageTag()` を明示指定できます。

```go
package docexample

import (
    "context"

    container "github.com/hirokazumiyaji/container-go"
)

func AllowMutableImageTag(ctx context.Context) {
    _, _ = container.Run(ctx, "redis:7-alpine",
        container.WithAllowMutableImageTag())
}
```

Apple の `name@digest` と `name:tag@digest` は pinned として扱い、この option を指定しても mutable へ降格させません。
この fallback は tag の置き換えを防ぐ保証ではなく、bare digest や Docker image ID も降格しません。
pull 完了から inspect の間に発生する mutable tag の変更や、共有 daemon に対する別 process の変更を、CLI API だけで原子的に排除できるとは仮定しません。
再現性が重要な場合は、backend が atomic address を提供する場合の `image@sha256:...` を呼び出し側から渡してください。
Apple が単一 manifest の周囲に合成した local index の root は registry から取得できるとは限らないため、無条件に exact pull しません。
local に addressable な tag または alias を使うか、明示的な mutable-tag fallback を要求します。
新規作成直後の container は再 inspect し、root、platform、選択された variant が異なる場合は `Run` が返す前にロールバックします。
local build の Apple image に利用可能な local digest 参照がない場合は `WithAllowMutableImageTag` が必要になることがあります。

## プロセスの終了コード

`Container.ExitCode(ctx)` は、コンテナが停止し、バックエンドが終了コードを報告した場合に値を返します。
停止前は `ErrContainerNotStopped` を返し、このエラーは `ErrExitCodeUnavailable` にも一致します。
`Wait(ctx)` は停止まで待ち、停止後に終了コードが未報告なら `ErrExitCodeUnavailable` を返します。
`InspectInfo.ExitCode() == 0` を成功と解釈する前に、`State()` と `ExitCodeReported()` を確認してください。
以前の `0, nil` を成功としていた呼び出し側は、これらのエラーを処理する必要があります。

Apple の `status.exitCode` は任意項目で、保存済みの CLI 1.2.2・1.3.0 の inspect fixture には含まれていません。
省略された場合、停止状態だけでは正常終了を判断できません。
実際に動作した Apple コンテナの終了コードは、サービス未稼働のため検証できていません。
Docker の `dead` 状態では報告されたプロセス終了コードを返しますが、デーモンによる後処理の成功は示しません。

`InspectInfo` は再 inspect なしで parse 済みの backend metadata を公開します。
`Health()` は Docker の healthcheck status と連続失敗回数を返し、
`ImageDigest()` は backend が digest を公開する場合にそれを返します
（Docker は run 時の参照が digest 固定の場合だけ）。
`NetworkMode()`、`Networks()`、`NetworkAliases()` は接続 network の情報を返します。
Apple Container は Networks を報告しますが、healthcheck、network mode、
network alias は持ちません。

## Exec のタイムアウトと長時間実行

`Container.Exec` は有限時間・バッファリングされる操作です。caller の
context に deadline がない場合、Exec は 30 秒の既定 deadline を適用し、
ハングした backend が test を無期限にブロックしないようにします。
正の `WithExecTimeout(d)` を指定した場合、effective deadline は `d` と
caller の deadline の早い方です。`WithExecTimeout(0)` は library の既定
deadline のみを無効化し、caller の deadline は取り除きません。これは
意図的な長時間実行コマンドの場合だけ使用し、cancellable な context
(可能なら deadline 付き)を併用してください。長寿命のプロセスは通常、
コンテナの本体的コマンド(`WithCmd`)として起動し、出力には `FollowLogs`
を使ってください。長-open な `Exec` 呼び出しは避けてください。

Exec は既存の `(exitCode, output, error)` 契約を維持します。
command の非ゼロ終了は結果であり、backend、timeout、cancellation のエラーは分類済み error として返されます。
CLI が終了コードを返している場合は、その error と併せて `exitCode` にも保持されます。
いずれのエラーでも、失敗前に生成された partial stdout/stderr を保持しているため `output` を読んでください。

context error によって local CLI が終了した場合、stable process identity を持つ Unix target でのみ、直接の command lifecycle が所有する間だけ process group をベストエフォートで停止します。Linux では `waitid(WSTOPPED|WNOWAIT)` で停止状態を確かめられない場合、pidfd へ直接 signal を送り、古い PGID には signal を送りません。group signal の後は直接の子にも kill を送るため、child が process group を変更した場合も直接の lifecycle を終了できます。他の Unix target は直接の子 handle だけを使います。直接の子を回収した後は、古い process group ID には signal を送りません。
Windows では lifecycle が `OpenProcess` handle を保持し、`TerminateProcess` で直接の子だけを終了します。detach された descendant や親を付け替えられた descendant は保証外です。終了済み child への kill 成功は active process の証拠として扱いません。
Windows 固有の lifecycle test は build constraint 付きです。開発環境では Windows package を cross-compile と vet できますが、Windows の process-handle runtime test や exit-259 の active-child test は実行できません。
backend CLI には exec instance を kill する共通操作がないため、active-child の証拠が利用でき、command の起動中に context error が実際に競合した場合だけ `*ExecTerminationError`(`errors.Is(err, ErrExecTerminationUnsupported)`)を返します。保守的な direct-handle fallback では、remote process の終了を主張せず context error だけを返すことがあります。この分類は local の終了コードに依存しません。
Windows の `Process.Kill` が終了コード `1` を返す場合でも、その status を保持したまま型付き error を返します。
後から inspect が deadline を使い切っただけでは `ExecTerminationError` にはなりません。
backend 側 process が残っている可能性があるため、caller は container を terminate するか backend 固有の cleanup を実行してください。

`FollowLogs` は起動エラーを直接返します。ストリームを返した後は EOF まで `Read` してください。
CLI の終端エラー(CLI status と context cancel が競合した場合も含む)は `Read` から返ります。
`Close` と context cancel は意図的な終了経路なので、EOF または context error になることがあります。
直接 child 終了後に descendant が stdout/stderr を保持しても、stream は制限時間だけ drain してから endpoint を閉じるため、EOF が無期限に待ちません。
`ForLog` は match を受け入れる前に終端 stream error を確認し、終端 error と context error を `errors.Join` または `%w` で保持します。
cancel 後に caller context から外した state probe を開始しません。
timeout の分類は structured context、`Timeout() bool`、signal、`CLIError.OperationTimeout` の証拠だけを使い、arbitrary な workload stderr を timeout として解釈しません。

### ログのストリーミング

`FollowLogs` はバックエンド CLI の起動後に `io.ReadCloser` を返します。
EOF まで `Read` してください。CLI の起動後に異常終了した場合、終了エラーと
`CLIError` の詳細は `FollowLogs` ではなく `Read` から返ります。
`Close` またはコンテキスト取消は意図的な終了です。
その後の `Read` が EOF またはコンテキストエラーを返すことがあります。
`wait.ForLog` は一致行を準備完了として確定する前に終端状態を確認するため、
マージされた CLI 診断が終端エラーを隠すことはありません。

ライブラリは直接の CLI 子プロセスを必ず待って回収します。
Unix 系では、停止状態を確認したあと CLI の process group へベストエフォートの `SIGKILL` を送り、続けて直接の子にも kill を送ります。
同じグループに残った helper は終了することがありますが、回収は保証しません。
停止状態を確認できない場合は直接の子（Linux では pidfd）だけを対象にし、古い PGID には signal を送りません。
直接の子を回収した後は、古い数値の process-group ID は使いません。
Windows では Close と context cancel は保持した process handle で直接の CLI 子だけを終了し、descendant は終了しません。
その他の対応 OS でも直接の子だけを対象とします。
CLI がヘルパーをデタッチする設計の場合は、子孫の終了を保証できません。

## クリーンアップの契約

通常の終了、作成後の rollback、create 失敗、異常終了では、エラーの見え方が異なります。

1. **通常の cleanup**: `container.Cleanup(t, ctr)` は
   `t.Cleanup` 経由で `TerminateContainer` を登録します。終了エラーは
   テストの log に残ります。defer 形式の
   `container.TerminateContainer(ctx, ctr)` はエラーを呼び出し元へ返します。
   caller の context を使い、`Cleanup` と `StrictCleanup` は
   name-lock 待ちを含む完全な teardown を `terminateTimeout`
   (既定 30 秒) の context で制限します。どちらの helper も nil 安全です。
2. **作成後の rollback**: 再利用でない `Run` が file copy または readiness
   待ちの途中で失敗すると、`Run` は `Terminate` を呼びます。削除にも失敗
   した場合は、元の失敗とコンテナが残ったことを示すメッセージを返り値に
   含めます。作成後の env-file cleanup が失敗した場合は、利用可能な
   ハンドルと結合済みエラーを返します。呼び出し側はそのハンドルを確認
   または明示的に `Terminate` してください。
3. **create 失敗**: backend の `run` 自体が失敗した場合、best-effort の
   `cleanupFailedCreate` は、この process の managed / session label を
   持つコンテナだけを inspect します。creation label は存在し、この run と
   一致しなければならず、label がない場合や一致しない場合は削除しません。
   これは replacement を防ぐ best-effort の guard であり、トランザクション
   保証ではありません。lock、inspect、delete の error は `Run` の返り値に
   連結されません。name conflict はこの経路では削除しません。
4. **異常終了**: 親 process が終了して reaper pipe が閉じられると、
   best-effort watchdog が強制削除を試みます。process を終了させる
   panic、`SIGKILL`、`os.Exit` でも pipe は閉じます。recover した panic では
   閉じません。親が生きている間、reaper は何もしません。これは
   トランザクション的な保証ではなく、登録 error や各 entry の delete error は
   `Run` へ返されません。

reaper は実 CLI コンテナを非 reuse 経路で登録した場合に遅延起動します。
対応するすべてのプラットフォームで Go ネイティブのループを使います。
ライブラリ自身の実行ファイルを内部マーカー付きで再実行し、inspect 出力を
`sed` や `awk` ではなく、ライブラリの strict JSON パーサと inspect パーサで
解析します。`/bin/sh` の実装も機能フラグもありません。

動作中の reaper を停止する経路（最後の entry を unregister したときや、pipe
への書き込み失敗で置き換えるときなど）では、その reaper が所有するプロセス
ツリーをまとめて破棄します。Unix は reaper が停止したことを確認してから
reaper のプロセスグループへシグナルを送ります。Windows は reaper を
Job Object に割り当て、直接の kill の前に `TerminateJobObject` を呼ぶため、
reaper とそれが起動した全プロセスを 1 つのツリーとして終了します。現在の
native ループは EOF まで backend コマンドを起動せず、停止経路は親が生きて
いる間しか動かないため、現時点で in-flight の `docker.exe` は存在しません。
これは EOF より前に backend を起動する将来のループに対する多層防御であり、
Windows の「直接の子だけを kill する」制限を置き換えるものです。この job は
`JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` を意図的に設定しません。job handle を
持つのは親だけで、親の死亡時にカーネルがそれを閉じるため、この制限を設定
すると watchdog が reap のために生き残るべき瞬間に終了してしまいます。
既存の非表示コンソールと新しいプロセスグループは変更しません。job を
作成できない、または reaper を割り当てられない場合（ネストした job が許可
されない環境など）でも reaper は動作し、停止経路は直接の子だけを終了する
従来の動作に fallback します。

self-exec にはこの package を含む main package の実行ファイルが必要なため、
reaper は `-buildmode=c-shared` と `c-archive`（独自の Go runtime を持つ）では
無効化され、Unix では setuid/setgid バイナリでも無効化されます。さらに build
information の main module または dependency にこの module
(`github.com/hirokazumiyaji/container-go`) が含まれることを要求します。
`/bin/sh` への fallback はないため、条件を満たさない host では watchdog を
作らず、登録は no-op になります。異常終了時の回収は利用できず、明示的な
`Terminate`、`Cleanup`、`Prune`、rollback 経路だけが cleanup 手段です。
`-buildmode=plugin` と `shared` は runtime が host 実行ファイルの build info を
返す可能性があり buildmode を確実に検出できませんが、module 検査により、この
package を link していない plugin host は re-exec されません。link している
host は安全に reaper を実行できます。これらの build mode は引き続き未サポート
です。
また再実行はその時点の executable path を起動するため、実行中に
バイナリを置き換えると respawn 後の reaper は新しい版になります。Windows
では実行中の reaper が executable image を終了まで開いたままにするため、
reap が spawn 元の process より一時的に長生きすることがあります。テストで
起動した場合は test binary の終了前に reap の完了を待ってください。
作成するコンテナは backend create の開始前に名前と creation generation で
pre-register されます。create が成功すると、ライブラリは managed / session /
creation label を検証してから Docker の完全な 64 桁 hex immutable ID を追加の
reaper entry として保持します。名前で address する entry（Apple は常に、
Docker は `run` が ID を返すまで）は、lock を持つ場合は通常の
generation-checked 削除と同じ名前単位 `flock` の下で inspect・削除されます。
Docker の generation 削除は inspect が返す immutable ID に束縛されるため、
lock のない name-addressed Docker entry（name lock を持たないホスト）もその ID で安全に
回収されます。Apple の name 削除は常に lock を必要とします。Docker ID entry は
name lookup なしで ID により削除されます。backend が解析可能な ID を返さない
場合は、name と generation の経路が削除対象として残ります。

reaper の spawn failure は retry 上限後に一度だけ log へ残ります。
delete failure は reaper が無視します。reaper の動作を cleanup の成功確認に
使わないでください。

現在の reaper は inspect output をメモリ上で解析し、raw JSON を disk へ
書きません。変更前のバージョンは namespace のない `mktemp` file を使って
いたため、inspect 中に reaper が kill されると環境 data を含む file が残る
可能性があります。旧バージョンの file を扱う場合は、cleanup 前に
container-go と reaper process を停止し、実効 `TMPDIR` で user 所有の
regular file だけを metadata-only listing し、affected time window に限定して
ください。file content を表示・grep したり、symlink を追跡したり、broad
recursive delete を実行しないでください。該当 run に確実に帰属する file だけを
削除し、inspect output に含まれた可能性がある credential を rotate してください。
これは旧バージョンに対する no-leak 保証ではありません。

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
状態を選びます。Docker は managed コンテナのうち exited と dead 状態を
選びます。この filter は running または created 状態を選びません。Apple
では、各 candidate は削除前に安定した名前単位 lock の下で再 inspect され、
世代、session、管理対象ラベル、および停止状態が再確認されます。

package レベルの `Prune` は backend 中立の `PruneOption` だけを受け付け、
Docker target の option を持ちません。特定の Docker daemon や context を
対象にする場合は、`WithClientDockerHost` または
`WithClientDockerContext` で `Client` を作り、`Client.Prune` を呼びます。
`Pull` は異なり、package レベルの `Pull` は 1 回の fetch 用に
`WithPullDockerHost` と `WithPullDockerContext` を持ちます。
`Client.Pull` は Client が target を固定するため、これらの上書きを
拒否します。

`Container.Kill(ctx, signal string)` は signal を 1 つだけ取ります。
空文字列は既定の SIGKILL です。空でない signal は正の番号または
`SIGTERM` のような大文字の signal 名でなければならず、それ以外の綴りは
backend 実行前に検証エラーとして拒否されます。

```go
package docexample

import (
    "context"
    "testing"

    container "github.com/hirokazumiyaji/container-go"
)

func Lifecycle(ctx context.Context, t testing.TB, ctr *container.Container) {
    // caller の context が TerminateContainer の上限になる。
    defer container.TerminateContainer(ctx, ctr)

    if err := ctr.Kill(ctx, "SIGTERM"); err != nil {
        t.Fatal(err)
    }
    if err := ctr.Kill(ctx, ""); err != nil { // 空文字列は SIGKILL。
        t.Fatal(err)
    }
}
```

`WithPruneQueryTimeout(d)` は一覧取得と、各候補のロック待ち・検証・削除の時間を制限します。
Client の `WithClientQueryTimeout(d)` より優先され、どちらも指定しない場合は30秒です。
`WithPruneTimeout(d)` は prune 全体の時間を制限します。
Client の query timeout は Client 構築と短い inspect にも適用されます。個別の Run では `WithQueryTimeout(d)` で上書きできます。
転送、スナップショットログ、ライフサイクル変更、検証の再試行には、呼び出し側が設定した期限が適用されます。期限がなければ query timeout が適用されます。
イメージ取得には `WithPullTimeout` が適用されます。
`Cleanup` と `StrictCleanup` には、処理全体に独立した30秒の制限があります。

`container.WithPruneOlderThan(d)` は、backend が報告する作成時刻が
now-d より前の candidate だけに対象を絞ります。共有 daemon や remote
daemon で掃除するとき、並行して動く job が最近停止したコンテナを
残せます。作成時刻を報告しない candidate も残します。通常の `Prune` は
引き続き停止済みの managed コンテナだけを選ぶため、実行中のまま残った
コンテナは停止するまで削除されません。

`container.WithPruneLabel(key, value)` は `Prune` と `PruneReuseGroup` の
対象を、その label を持つコンテナに限定します。value が空でない場合は完全一致、
空の場合は key の存在だけを要求します。複数回指定した場合はすべての label を
要求します。Docker は `ps` の filter として適用し、検証した candidate でも
再確認します。Apple Container は list した candidate を client-side で
filter します。`container.WithPruneDryRun()` は ownership、state、age、label の
同じ検証を行い、実際に削除する ID を削除せずに返します。reaper の状態も
変更しません。

### remote daemon と非対応 host の cleanup

リーパーはローカルの子プロセスです（ライブラリ自身の実行ファイル）。CI の
job cancel や `go test -timeout` でローカルの process tree や runner VM が
破棄された後には動作できません。remote Docker daemon 上のコンテナも
ローカル環境より長く残ります。self-exec できない build では reaper が
ありません。リーパーが動作する場合でも登録と削除は best-effort であり、
cleanup の成功確認には使えません。

そのような環境では、suite の前後に `TestMain` から停止済みの managed
コンテナを掃除します。次の helper を `TestMain` から呼び、その戻り値で
終了してください（`func TestMain(m *testing.M) { os.Exit(pruneAndRun(m)) }`）。

```go
package docexample

import (
	"context"
	"testing"
	"time"

	container "github.com/hirokazumiyaji/container-go"
)

func pruneAndRun(m *testing.M) int {
	ctx := context.Background()
	// Reap leftovers that are old enough not to belong to a concurrent run.
	_, _ = container.Prune(ctx, container.WithPruneOlderThan(6*time.Hour))
	code := m.Run()
	// Remove this run's stopped containers.
	_, _ = container.Prune(ctx)
	return code
}
```

`Prune` は本 library が作成した停止済みコンテナだけを、session に
関係なく削除します。running と created は対象外です。共有 daemon や
remote daemon では、suite 前の掃除に age filter を残して他の job が
最近停止したコンテナを保護し、suite 後に通常の `Prune` を使うのは
その job 専用の daemon のときだけにしてください。これらの呼び出しは、
process の環境変数経由でも、その daemon に固定した `Client` 経由でも
remote daemon に対して使えます。

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
- 既存コンテナは managed marker、`WithReuse` marker、有効な creation
  generation、および Docker では有効な immutable UID を持つ必要があります。
  image も互換でなければなりません。create 競合による name conflict は成功
  として扱い、既存へ attach します。
- 停止済みコンテナは、これらの ownership check と image check に合格した
  場合だけ削除・再作成の対象にします。running のまま ready にならない場合は
  削除せず error を返します。
- image 互換性は元の mutable tag ではなく、解決済み digest / image ID で
  判定します。Docker では要求した network identity も一致する必要があります。
  `WithNetwork` 省略時は daemon の platform default を意味し、inspect の
  `default` mode は `NetworkSettings.Networks`（Linux では `bridge`、native
  Windows では `nat`）と照合します。`host`、`none`、名前付き network は
  wildcard にしません。reuse は互換性 check の前と readiness 後、handle を
  返す直前に再 inspect します。Docker は immutable UID、Apple は creation
  generation を再確認するため、wait 中に置き換えが起きた場合は adopt せず
  error を返します。
- port の互換性は backend ごとに異なります。Docker は自動公開された
  exposed-port binding と明示的な publication を比較します。Apple は明示的な
  published binding を比較しますが、library の Apple inspect model に
  `WithExposedPorts` の宣言が残らないため、それを比較できません。Apple では
  新しい handle ごとに自分の exposed-port 宣言を共有コンテナ IP へ適用するので、
  宣言を分離したい場合は名前を変えてください。
- memory、user、working directory、entrypoint、cmd、mounts、custom labels、
  `WithReadOnlyRootFS`、`WithInit`、`WithDNS`、`WithStopSignal` は、省略時も
  含め作成時の fingerprint と一致する必要があります。reuse group も一致
  しなければ `ErrReuseMismatch` を返します。fingerprint のない既存コンテナは
  再作成してください。`env` の差は attach 時に無視するため、重要なら別の
  名前を使ってください。`WithFiles` は attach caller を含むすべての reuse
  caller でコピーされ、attach 時のコピー失敗は共有コンテナを削除せずに
  error を返します。`PullAlways` は attach 前にも各 caller で実行されます。
- `WithReuseEnvFingerprint()` は `WithReuse` caller の環境変数一致を opt-in
  します。library は環境変数の key と value の組を sort したものの SHA-256
  digest を予約ラベルに保存し、生の値をラベルにも argv にも書きません。
  この option を指定した attach caller は同じ digest を必要とし、見つから
  ないか異なる場合は `ErrReuseMismatch` を返します。ラベルのない既存
  コンテナは再作成してください。option を指定しない場合、attach 時の環境
  変数差分は従来どおり無視されます。この option は `WithReuse` が必要です。
- `Container.Created()` はその `Run` 呼び出しがコンテナを作成したかを返し、
  `Container.Reused()` はその逆を返します。`WithReuse` で作成した caller は
  `Created`、既存 generation へ attach した caller は `Reused` です。
- `WithPostReadyHook(fn)` は readiness 後、`Run` の返却前に実行されます。
  フックが失敗すると通常の Create は rollback され、作成者の `WithReuse` は
  その generation を破棄します。フックは attach caller でも実行されますが、
  caller 間の同期機構ではありません。初期化は冪等にするか、wait strategy や
  application の readiness signal で完了を検出してください。
- `WithFiles` の directory root は、source directory 名ではなく中身を
  `ContainerPath` にコピーするため、reuse の attach を繰り返しても nest しません。
  Docker はこの動作を `<staged>/.` source form として定義しています。
  Apple Container も同じ form を受け取りますが、検証は Apple integration suite が
  backend 利用可能な host で行い、利用できない場合は skip します。
  そのため Apple の挙動はその test を実行した host でのみ検証済みです。
  `WithTestLogger(t)` はコンテナ log を test の間 `t.Logf` へ転送します。
  新規コンテナは履歴を replay し、Docker の reuse attach は時計ずれで直後の log を
  落とさないよう attach の少し前から resume します（Apple は `--since` を持たないため
  履歴を replay します）。
- 各作成は generation label を持ちます。`Terminate`、stopped 再作成経路、
  watchdog reaper は置き換わった generation の削除を拒否します。Docker の
  handle は `docker run` が出力した（または inspect で検証した）immutable ID で
  削除するため、same-name replacement は削除対象になりません。Apple の名前
  ベース削除は generation を要求し、名前単位 `flock` の下で inspect と delete を
  行います。
- `Cleanup` / `TerminateContainer` / watchdog reaper は reused handle を
  削除しません。明示的な `ctr.Terminate` だけが共有コンテナを削除できます。
- `CONTAINERGO_KEEP=1` はこの reuse contract を変えません。条件に一致した
  stopped reuse container は削除・再作成され、`PruneReuseGroup` はその
  group を削除できます。
- 名前単位の `flock` は generation-checked な通常の `Terminate` /
  failed-create cleanup 経路、Apple の `Prune` と `PruneReuseGroup`、および
  名前で address する reaper entry を保護します。完全な immutable ID を使う
  Docker reaper entry は名前で address しないため lock を必要としません。
  外部ツールによる協調していない CLI 操作は非協調として扱ってください。
- `container.PruneReuseGroup(ctx, "integration")` はその group の
  コンテナを強制削除します（CI teardown）。group は label であり、
  reuse 互換性と shared-flight key の一部です。attach には完全一致が必要です。
  Apple Container では、削除前に名前単位 lock の下で fresh
  candidate 検査（世代、session、managed/reuse/group ラベル、running/stopped
  状態）を適用します。通常の `Prune` は上記の backend filter を使い、
  Docker では exited と dead コンテナが対象です。
- reused ハンドルでは、peer の観測結果を変えてしまう操作を
  `ErrSharedContainer` で拒否します: `Stop`、`CopyToContainer`、
  `CopyContentToContainer`、`CopyFileFromContainer`。ガードがなければ 1 パッケージの `Stop` が
  他パッケージが使用中のコンテナを停止し、被害は相手側では原因の
  特定できない接続失敗として現れます。読み取り系（`Endpoint`、`State`、
  `Exec`、`Logs` など）は対象外です。共有コンテナの lifecycle を自プロセスが
  掌握する場合は `ctr.Shared()` で拒否を解除したハンドルを得ます。
  `Cleanup` / `TerminateContainer` は依然として reused ハンドルを削除しません。
  `WithFiles` は `Run` の一部として実行されるためガードの対象外です。
  ファイルは create 時に stage され、attach するすべての caller が
  コピーし直すため、後続 caller の上書きを peer が観測できます。

ライブラリはテスト間のアプリケーションデータを自動初期化しません。
key prefix、schema 分離、`Exec` による reset（`FLUSHALL` など）を使って
ください。

## セキュリティ上の注意

- すべての CLI 呼び出しは argv 配列で行い、シェルを経由しません。リーパーは
  コンテナ ID を検証済みの stdin データとしてのみ受け取り、メモリ上で解析
  します。name-addressed entry には共有の `nameRE`
  トークン規則を、Docker の full 64-hex immutable ID は別に受け付けます。
  Apple のより厳密な 2〜63 文字の名前チェックはコンテナ作成前に適用します。
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
- Windows では `%LocalAppData%\containergo-env-v1` 配下に、現在のログオン
  (および Administrators と LOCAL SYSTEM。copy staging と同じ許可リスト)
  だけに許可した保護 DACL で env ファイルを保存します。
  ルート、各ディレクトリ、各ファイルを使用前と削除前に再検証し、パス構成要素に
  reparse point があれば fail closed とします。
  `%LocalAppData%` の祖先に reparse point がある場合も拒否するため、
  junction でプロファイルを移している環境では `Run`/`Exec` は
  リンク越しに書き込まず fail closed になります。
  cleanup は env ファイル、marker、lock ファイルの順に削除し、最初の失敗で止まるため、
  一時的な sharing violation が起きても marker と writer lock が残ります。
  再試行を諦めた時点(再試行不可の失敗、または有界の再試行予算の使い切り)で
  writer lock を解放するため、次回の scan が安全に回収できます。
  POSIX advisory lock の代わりに `LockFileEx` で作成とクラッシュ後の回収を直列化します。
- 環境変数キーは、空でない有効な UTF-8 であり、`=`、Unicode 空白、制御
  文字、先頭の `#`、先頭 BOM を含むことはできません。
  値は有効な UTF-8 であり、Unicode 制御文字、NUL、CR/LF、U+2028、U+2029
  を含むことはできません。
  制御文字でない Unicode、空白、`=` は値として許容します。
  制御文字(タブを含む)や不正な UTF-8 を拒否する方針は、backend が受け付ける
  値でも env ファイルには書かないという意図的な互換性変更です。
- `CopyToContainer` は通常ファイルとディレクトリを最終要素のリンクを追わずに開きます。
  開いたハンドルをユーザー専用の非公開ステージングへコピーしてから、backend を呼び出します。
  `WithFiles` は通常ファイルとディレクトリを受け付けます。
  option 評価時にはルートがシンボリックリンク、reparse point、特殊ファイルなら拒否し、
  スナップショット作成時にはディレクトリ内のシンボリックリンク、reparse point、特殊ファイルを拒否します。
  スナップショットの上限は `MaxCopyToContainerSize`（64 MiB）、`MaxCopyToContainerEntries`（10,000 エントリ）、`MaxCopyToContainerDepth`（128）、`MaxCopyToContainerMetadataSize`（訪問したソースパスの合計 8 MiB）です。
  保持するメタデータは名前、バイト列、Unix パーミッションだけです。
  タイムスタンプ、所有者、setuid/setgid、ACL、拡張属性、alternate data stream、sparse layout、hardlink identity は保持しません。
- `WithFiles` は `HostPath` と nil でない `Content` バイト列のどちらか一方だけを指定します。
  両方またはどちらも指定しない場合は、option 適用時に `ValidationError` を返します。
  nil でない空の slice は空ファイルとしてコピーします。
  `File.Mode` は非公開ステージング上のコピーの Unix パーミッションを上書きします。
  0 は `HostPath` のパーミッションを保持し、`Content` では `0600` を既定値にします。
  非 0 の値は permission bits だけを含む必要があり、type、setuid、setgid、sticky bit は拒否します。
  `CopyContentToContainer` は既存の handle に対して同じステージングと mode の規則を適用します。
  Content は argv、error、診断に現れず、ステージングファイルは成功、失敗、キャンセルのいずれでもステージングディレクトリごと削除されます。
- レジストリ認証情報は本 library では扱いません。Apple Container では
  `container registry login`、Docker では `docker login` を使い、認証情報と
  registry context は backend CLI が管理します。

### 旧バージョンの reaper staging ファイル

現在の watchdog は inspect 出力をメモリ上で解析して処理し、raw 出力を
ディスクへ書きません。旧バージョンは名前空間もない `mktemp` ファイルを
使っていたため、inspect 中に reaper が kill されると、コンテナの環境変数を
含むファイルが残る可能性があります。旧バージョンのファイル名は本ライブラリの
ものと安全に特定できません。調査・削除する場合は、先にすべての container-go と
watchdog プロセスを停止し、実効 `TMPDIR`(`/tmp` と決めつけません)でメタデータ
だけを列挙し、そのユーザーと該当時間帯の通常ファイルに範囲を絞ります。ファイル
内容を grep して端末へ表示したり、シンボリックリンクを追跡したり、広範な再帰削除
をしたりしないでください。該当実行に確実に帰属するファイルだけを削除して
ください。inspect 出力に含まれた可能性がある認証情報は失効済みとして扱い、
ローテーションしてください。stale ファイルを削除しても secret 自体は失効しません。
- `Run` は image inspect が返した検証済み identity、または Docker の image ID を backend に渡すため、inspect 後の local tag 変更で create 対象が変わりません。
  Apple の descriptor-backed reference は create 前に local addressability を確認します。
  呼び出し側の `name@digest` と `name:tag@digest` は pinned のまま残り、`WithAllowMutableImageTag` は mutable tag と明示的に許可された互換 fallback にだけ適用されます。
  `container run` を暗黙の fetch として使いません。
  これは backend API の範囲での保証であり、mutable tag の pull から inspect までの操作を原子化するものではありません。
  identity を取得できない backend では `WithAllowMutableImageTag` による明示的な fallback が必要です。


## 公開 API 一覧

動作の説明は前述の各節にあります。この一覧は、エクスポートしているすべての
symbol を README から辿れるようにするためのものです。独立した節を持たない
symbol はここで説明します。

**コンストラクタとパッケージ関数** — `Run` はコンテナを作成・起動・待機して
handle を返します。`Pull` はコンテナを作らずに image を取得します。`Prune` は
この library が管理する停止済みコンテナを force なしで削除するため、inspect
後に再起動したコンテナは残ります。`PruneReuseGroup` は指定した
`WithReuseGroup` の値でタグ付けされたすべてのコンテナを、実行中も含めて
強制削除します（CI teardown 用）。
`NewClient` と `NewClientContext` は `Client.Run`、`Client.Pull`、
`Client.Prune`、`Client.PruneReuseGroup` が使う backend・target・runner の
設定を固定し、process の環境変数を構築時に一度だけ読んで凍結します。
`Cleanup` と `StrictCleanup` は test teardown を登録し、`StrictCleanup` は
teardown の失敗を test の失敗にします。`TerminateContainer` は defer 用の
nil 安全なヘルパーです。

**Container** — `ID`、`Image`、`State`、`Inspect`、`Created`、`Reused`、
`Shared`、`TargetHost`（この handle で選ばれた実効 backend host）、`Start`、
`Stop`、`Restart`、`Kill`、`Pause`、`Unpause`、`Terminate`、`Wait`、
`ExitCode`、`Endpoint`、`Host`、`MappedPort`、`ContainerIP`、`Exec`、
`ExecTo`、`Logs`、`LogsWithOptions`、`LogsTo`、`FollowLogs`、
`FollowLogsTo`、copy 系のメソッド、および attachment の accessor
`StopAttached`、`AttachedDone`、`AttachedError`。`WithAttachOutput` は作成中に
writer を開始し、`StopAttached` は log stream を停止しますが writer は閉じません
（writer は caller が所有し、caller が閉じます）。`AttachedDone` が stream の
完了を通知し、`AttachedError` がその error を返します。

**実行オプション** — `Option` と、「実行オプション」節で説明するコンテナ
option。`WithWaitStrategy`、`WithPullPolicy`、`WithLabels`、`WithMounts`、
reuse 系の option を含みます。

**Exec オプション** — `ExecOption` と `WithExecEnv`、`WithExecUser`、
`WithExecWorkDir`、`WithExecMaxBytes`、`WithExecStderr`、`WithExecTimeout`。

**Client** — `Client`、`ClientOption`、および option の `WithClientBackend`、
`WithClientBinary`、`WithClientDockerHost`、`WithClientDockerContext`、
`WithClientDockerConfig`、`WithClientPlatform`、`WithClientRunner`、
`WithClientQueryTimeout`。accessor は `Client.Backend`、
`Client.DefaultHost`、`Client.IsRemote` です。

**Pull** — `PullOption` と `WithPullBinary`、`WithPullRunner`、
`WithPullPlatform`、`WithPullTimeout`、`WithPullDockerHost`、
`WithPullDockerContext`。

**Prune** — `PruneOption` と `WithPruneBinary`、`WithPruneRunner`、
`WithPruneLabel`、`WithPruneParallelism`、`WithPruneTimeout`、
`WithPruneOlderThan`、`WithPruneDryRun`。

**Mount** — `Mount`、`MountType`、`WithMounts`、およびコンストラクタ
`BindMount`、`TmpfsMount`、`VolumeMount`。

**ファイルとコピー** — `File`、`FileFromHost`、`FileFromContent`、`WithFiles`、
`CopyContentToContainer`、`CopyToContainer`、`CopyFileFromContainer`、
`MaxCopyToContainerSize`。

**上限付き・ストリーミング出力** — `ExecTo`、`LogsTo`、`FollowLogsTo`、
`LogsWithOptions`、`OutputStats`、`TruncationReporter`、`Streamer`、
`ErrOutputTruncated`。詳細は「出力の契約」を参照してください。

**Runner と CLI 注入** — `Runner`、`ExecRunner`、`ExternalRunner`、
`WithRunner`、`WithBinary`、`WithRunTimeout`、`WithProbeTimeout`、
`WithDockerHost`、`WithDockerConfig`、`WithAttachOutput`、
`WithReuseAttachTimeout`、`WithReusePollInterval`。

**Inspect** — `InspectInfo` と、それが返す `PortBinding`。

**エラー** — `CLIError`、`ConfigError`、`ValidationError`、`CleanupError`、
`ErrCleanupFailed`、`ExecTerminationError`、`ErrExecTerminationUnsupported`、
`SystemNotRunningError`、`ErrSystemNotRunning`、`ErrContainerNotFound`、
`ErrGenerationReplaced`、`ErrSharedContainer`、`ErrReuseMismatch`、
`ErrUnsupportedCapability`、`ErrInvalidOption`、`ErrInvalidConfiguration`、
`ErrOutputTruncated`、`ErrImageIdentityUnavailable`、
`ErrImageIdentityMismatch`、`ErrImageIdentityNotLocal`、`ErrImageNotFound`、
`ErrPullNeverUnsupported`、`ErrContainerNotStopped`、`ErrExitCodeUnavailable`、
`ErrPortNotExposed`、`ErrEndpointUnreachable`、`ErrNoReachableHost`、
`ErrNetworkMismatch`、`ErrEnvFileUnsupported`、`ErrOutputDelivery`、
`ErrLogStreamSetup`、`ErrCopySourceUnsupported`、`ErrCopySourceChanged`、
`ErrCopySourceTooLarge`、`ErrCopySourceTooManyEntries`、`ErrCopySourceTooDeep`、
`ErrCopySourceMetadataTooLarge`、`ErrCopyFileNotRegular`、
`ErrCopyFileFromContainerTooLarge`、
`ErrCopyFileFromContainerUnsupported`。

最初の例は `Client` で target を固定し、image を pull してコンテナを起動し、
endpoint を解決して prune します:

```go
package docexample

import (
    "context"
    "fmt"

    container "github.com/hirokazumiyaji/container-go"
)

func UsingClient(ctx context.Context) error {
    client, err := container.NewClient(
        container.WithClientBackend("docker"),
        container.WithClientDockerHost("unix:///var/run/docker.sock"),
    )
    if err != nil {
        return err
    }
    if err := client.Pull(ctx, "redis:7-alpine",
        container.WithPullPlatform("linux/amd64")); err != nil {
        return err
    }
    ctr, err := client.Run(ctx, "redis:7-alpine",
        container.WithExposedPorts("6379/tcp"),
    )
    if err != nil {
        return err
    }
    defer func() { _ = ctr.Terminate(ctx) }()
    endpoint, err := ctr.Endpoint(ctx, "6379/tcp")
    if err != nil {
        return err
    }
    fmt.Println(endpoint)
    removed, err := client.Prune(ctx,
        container.WithPruneDryRun(),
        container.WithPruneParallelism(2),
    )
    fmt.Println(removed)
    return err
}
```

次の例は Docker backend を固定して host・tmpfs・volume の mount を設定し、
ファイルを stage し、ディレクトリをコピーイン、ファイルをコピーアウトして、
取得ログに上限を付けます。copy-out には Docker client と server の 29.7 以降が
必要で、Apple Container は `ErrCopyFileFromContainerUnsupported` を返します。

```go
package docexample

import (
    "bytes"
    "context"
    "io"
    "os"

    container "github.com/hirokazumiyaji/container-go"
)

func UsingMountsAndCopy(ctx context.Context, dir string) error {
    client, err := container.NewClient(container.WithClientBackend("docker"))
    if err != nil {
        return err
    }
    ctr, err := client.Run(ctx, "alpine:3.20",
        container.WithCmd("sh", "-c", "sleep 60"),
        container.WithMounts(
            container.BindMount(dir, "/host", true),
            container.TmpfsMount("/scratch"),
            container.VolumeMount("data", "/data"),
        ),
        container.WithFiles(
            container.FileFromContent("/etc/motd", []byte("hello\n"), 0o644),
        ),
    )
    if err != nil {
        return err
    }
    defer func() { _ = ctr.Terminate(ctx) }()

    if err := ctr.CopyToContainer(ctx, dir, "/copied"); err != nil {
        return err
    }
    reader, err := ctr.CopyFileFromContainer(ctx, "/etc/motd")
    if err != nil {
        return err
    }
    defer func() { _ = reader.Close() }()

    var logs bytes.Buffer
    if _, err := ctr.LogsTo(ctx, &logs, container.LogsOptions{MaxBytes: 4096}); err != nil {
        return err
    }
    _, err = io.Copy(os.Stdout, reader)
    return err
}
```

## testcontainers-go との違い

非対応（相当機能が存在しない、またはスコープ外）:

| testcontainers-go | 本 library |
|---|---|
| `wait.ForHealthCheck` | Docker 専用（`wait.ForHealthcheck`）。Apple Container にはヘルスチェックがない |
| Dockerfile からの build | スコープ外（`container build` / `docker build` を直接使用） |
| Ryuk reaper container | ローカルの watchdog reaper process で代替。ただし上記の制限がある |
| random host port mapping | Apple はコンテナ IP へ直接接続。Docker はランダム loopback port へ自動公開 |
| network / volume の作成と lifecycle 管理 | 当面スコープ外。`WithNetwork` は既存 network へ接続し、`WithMounts` は bind、名前付き volume、tmpfs の mount を受け取ります。remote Docker daemon への bind mount は `ErrUnsupportedCapability` で拒否し、ローカル Docker では Windows の host path を受け付けます。 |
| `GenericContainerRequest.Reuse` | `WithReuse` + `WithName`: process 間 get-or-create。再 wait 必須、Cleanup / reaper は所有しない |

## 開発

```text
make test                # backend 不要の unit test
make vet
make integration         # integration test（bench / singleflight を除外）。backend がなければ skip
make integration-docker  # Docker backend の integration test のみ
make bench-integration   # pull が重い bench / singleflight（bench module は Go 1.26+）
```

Integration test の image は Docker Hub の匿名 pull 制限を避けるため
`public.ecr.aws/docker/library/...` を使います。`CONTAINERGO_BACKEND=apple`
または `docker` で片方だけを実行できます。

GitHub Actions は `ubuntu-latest` で Go 1.26.0 と安定版 Go を使い、unit
test、race test、lint、`govulncheck` を実行します。同じ runner で Docker
integration test の行列も実行します。Apple Container integration は
GitHub ホストランナーに service がないためローカル専用です。
integration tag のないドキュメント test は、この README の英語版・日本語版
と design document の fenced code block をすべて抽出し、対応する block を
コメント除去後に比較します。Go block は引用符付きローカル `replace` を持つ
一時 module 内でコンパイルします。tag付き `examples/` の example が実
backend を動かします。

cross-platform job では macOS と Windows でも unit test と integration tag 付き vet を実行します。
Windows 固有の環境ファイルと process のテストも実行対象です。

Design document: [docs/design.md](docs/design.md)（日本語版:
[docs/design.ja.md](docs/design.ja.md)）。設計ドキュメントの「実装フェーズ」は
履歴であり、現在の API 契約ではありません。

コントリビューション: [CONTRIBUTING.md](CONTRIBUTING.md)。セキュリティ報告:
[SECURITY.md](SECURITY.md)。

## ライセンス

MIT
