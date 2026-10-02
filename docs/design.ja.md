# container-go 設計ドキュメント

English (primary): [design.md](design.md)

作成日: 2026-08-18（v0.2 バックエンド節を 2026-08-19 追記）
最終同期: 2026-09-25
対象: Apple Container CLI 1.2.2 と 1.3.0（macOS 26 以降、Apple Silicon。リポジトリの根拠で表した version）、Docker 29.x（ローカル 29.7.2 と v29 inspect fixture。Linux、Windows、macOS）、root module Go 1.23 以降、nested `bench/` module Go 1.25 以降

この文書は現在のチェックアウトにある実装を説明する。
「実装フェーズ」の節は初期設計の歴史として残るが、現在の API や今後のロードマップを約束するものではない。
現在の節で説明する公開 API と動作は、実装とテストを基準にしている。

このチェックアウトは、バックエンド固有の動作を改修する後続 issue の
修正を統合した状態ではありません。Apple の log option は #82、
動的な endpoint の更新は #85、reuse の ownership と最終 generation
確認は #83 と #84、stale Docker operation の target 修正は #74、
Docker inspect target validation は #103、Docker の dead-state prune は
#113 に依存します。Apple `Prune` の
list-to-delete cleanup validation は #98、WithReuse attach side effect は #94、
missing inspect の分類は #103、error chain の保持は #104、Apple PullNever の
capability handling は #112 に依存します。Windows / remote の bind source は
#76、TCP 専用 readiness の validation は #77、Stop timeout の validation は
#89、wait error chain の統一は #92、public option の validation は #102、reaper
staging の cleanup は #111 に依存します。
以下の現在の節は、その変更前の動作を説明します。

最新のタグ付きリリースは `v0.2.0`（2026-09-02）です。
このチェックアウトはそのタグより後の開発版です。
`v0.2.0` モジュールには Go 1.27 以降が必要ですが、現在の root チェックアウトには
Go 1.23 以降が必要で、nested `bench/` モジュールには Go 1.25 以降が必要です。
リリース版 API と現在の開発版 API は同じもの
ではありません。`LogsOptions` / `LogsWithOptions`、追加された
`wait.ForHTTP` の setter、公開型 `wait.AllStrategy` / `AnyStrategy` と
合成 strategy の `WithStartupTimeout`、root の `CLIError`、
`ErrContainerNotFound`、`ErrGenerationReplaced` は `v0.2.0` 以降の
開発版追加です。基本の `Run`、options、lifecycle、endpoint、`Exec`、
`Logs`、`FollowLogs`、copy、pull policy、Reuse、Cleanup、backend 選択、
元の wait strategy は `v0.2.0` に存在しています。
作成日: 2026-08-18(v0.2 バックエンド節を 2026-08-19 追記)
対象: Apple Container v1.2.x(macOS 26 以降、Apple Silicon)、Docker(Linux、Windows、macOS。copy-out には client/server 29.7.0 以上)、Go 1.23 以降

## 目的

**container-go** は、Apple Container（[apple/container](https://github.com/apple/container)）と Docker をバックエンドとする testcontainers スタイルの Go ライブラリである。
Go のテストコードから使い捨てコンテナを起動し、接続情報を返し、作成したコンテナを通常のテスト終了時と異常終了時のベストエフォート経路で削除する。

先行事例として、Rust には [shiguredo/container-rs](https://github.com/shiguredo/container-rs) がある。
本ライブラリは同じ問題領域を Go で扱うが、実現方式は後述のとおり異なる。

設計上の制約は次の三つである。

- **依存ゼロ**：サードパーティの Go モジュールに依存せず、標準ライブラリだけで実装する。
- **セキュリティ**：subprocess argv でインジェクションは防ぐが、異常終了時の reaper staging には環境データ 노출の余地があるため、現行の no-leak 保証ではない（#111）。
- **パフォーマンス**：テストスイートを左右するのはコンテナ（VM）の起動時間である。ライブラリ側のオーバーヘッドをそれに対して無視できる水準に保ち、並列起動を妨げない。

## 前提とする Apple Container の仕様

設計の根拠となる Apple Container の CLI・source の仕様を先に整理する。
リポジトリの inspect fixture は 1.2.2 と 1.3.0 を対象とする。
これは 1.2.x や 1.3.x のすべての release を確認済みとする互換性宣言ではない。

- ホスト要件は macOS 26 以降かつ Apple Silicon である。
- 既定では、各コンテナは軽量 VM として起動し、vmnet ブリッジ（network は `default`、`192.168.64.0/24`）上の実 IP を持つ。ホストはこの IP に直接到達できるため、ポート公開（`--publish`）は必須ではない。
- すべての操作は `container` CLI から行える。`ls --format json` と `inspect` は機械可読な JSON を返し、追加フィールドは `internal/inspect` が無視する。
- CLI は launchd 配下の `container-apiserver` と XPC で通信する。サービスが未起動だとコマンドは失敗し、`container system status` で状態を確認する。
- コンテナ名がそのまま ID になる。共有 library guard は
  `^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$` で、1 文字から 63 文字までを受け付ける。Apple Container の CLI はより厳密な 2～63 文字の規則 `^[a-zA-Z0-9][a-zA-Z0-9_.-]{1,62}$` を持ち、現在の checkout はその backend 固有の preflight check を適用しない（#112）。
- ヘルスチェック、`wait` コマンド、イベントストリーム、`ls` のラベルフィルタ、実行中コンテナへの再アタッチは存在しない。必要な動作はクライアント側で実装する。
- `--label` は存在するが、ラベルの絞り込みは JSON 出力をクライアント側で行う必要がある。ラベルキーは小文字の Docker/OCI 形式に限られる。
- `container cp` は実行中のコンテナにだけ使える。
- 各コンテナは軽量 VM として起動し、vmnet ブリッジ(既定は `default`、`192.168.64.0/24`)上の実 IP を持つ。ホストはこの IP に直接到達できるため、ポート公開(`--publish`)は必須ではない。
- すべての操作は `container` CLI から行える。`ls --format json` と `inspect` は機械可読な JSON を返す(追加フィールドは `internal/inspect` が無視する)。
- CLI は launchd 配下の `container-apiserver` と XPC で通信する。サービスが未起動だとコマンドは失敗する。起動状態は `container system status` で確認できる。
- コンテナ名がそのまま ID になる。名前は `^[a-zA-Z0-9][a-zA-Z0-9_.-]+$` かつ 63 文字以内でなければならない。
- Docker にある次の機能が存在しない：ヘルスチェック、`wait` コマンド、イベントストリーム、`ls` のラベルフィルタ、実行中コンテナへの再アタッチ。これらに相当する挙動はクライアント側で実装する必要がある。
- `--label` はあるがフィルタは JSON 出力をクライアント側で絞り込むしかない。ラベルキーは小文字英数字とハイフン、ドット区切りの Docker/OCI 形式に限られる。
- `container cp` は実行中のコンテナに対してのみ使える。copy-out には型を保持し symlink を追跡しないモードがなく、確認したバージョンでは host 側の検証前に symlink、FIFO、device node を dereference/consume することがある。そのため `CopyFileFromContainer` は Apple Container では `ErrCopyFileFromContainerUnsupported` を返し fail closed する。Docker では型を保持する extractor を利用するため client/server の両方が 29.7.0 以上を必要とし、必要な flag を host が扱える場合に host 側の Lstat/open 検査を維持する。どちらの条件も満たさない場合は fail closed する。
- `--rm` で削除しても匿名ボリュームは残る。
- エラー分類は `engine_apple.go` が保持する CLI stderr の部分文字列に依存する（名前衝突、イメージやコンテナの不在）。ライブ CLI に対する回帰テストは `cli_compat_integration_test.go` にある。

## 実現方式の選定

実現方式の候補は二つである。

- **CLI ラッパー方式**：`os/exec` で `container` CLI を子プロセスとして起動し、JSON 出力を解釈する。
- **XPC 直結方式**：container-rs が採る方式で、`container-apiserver` の XPC サービスを C ブリッジ経由で直接呼ぶ。

本ライブラリは CLI ラッパー方式を採用する。
理由は次の三点である。

第一に、依存ゼロ要件との整合である。
XPC 直結方式は cgo と自前の C ブリッジを必要とし、ビルドに macOS SDK が絡む。
CLI ラッパー方式は純 Go と標準ライブラリだけで完結し、`CGO_ENABLED=0` でもビルドできる。

第二に、安定性である。
XPC のルート名やメッセージ構造は Apple Container の内部実装であり、互換性の保証がない。
CLI はユーザー向けの公開インターフェースであり、JSON スキーマも Swift の公開ソースで確認できる。

第三に、性能上の差が問題にならないことである。
子プロセス起動のコストは 1 呼び出しあたり数十ミリ秒程度で、コンテナ（VM）起動の数秒に対して十分小さい。
テスト用途では XPC 直結による短縮効果を体感しにくい。

CLI ラッパー方式の弱点は、CLI のバージョン間で出力形式が変わることと、CLI が公開していない機能（ラベルフィルタなど）を使えないことである。
実装は inspect と Apple の list データに JSON を使い、バックエンドが提供する場合は Docker の機械向け name 出力と run ID も解析する。
人が読む表は解析しない。
CLI が公開しない機能はクライアント側フィルタで代替する。

## 公開 API

API の形は testcontainers-go（v0.44 系）の新 API に寄せる。
既存の testcontainers ユーザーが学習なしで使えることを狙う。

モジュールパスは `github.com/hirokazumiyaji/container-go`、ルートパッケージ名は `container` とする。

### 基本的な使い方

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
        container.WithEnv(map[string]string{"REDIS_ARGS": "--appendonly yes"}),
        container.WithWaitStrategy(wait.ForListeningPort("6379/tcp")),
    )
    container.Cleanup(t, ctr) // nil 安全。t.Cleanup で Terminate を登録する
    if err != nil {
        t.Fatal(err)
    }

    endpoint, err := ctr.Endpoint(ctx, "6379/tcp") // 例: "192.168.64.3:6379"
    if err != nil {
        t.Fatal(err)
    }
    _ = endpoint
}
```

### Run とオプション

以下はシグネチャの一覧であり、実行する source file ではありません。

```text
func Run(ctx context.Context, image string, opts ...Option) (*Container, error)
```

コンテナを新規作成する必要がある場合、`Run` は起動前に pull policy を適用してから待機戦略を完了する。
既定の `PullMissing` はローカルイメージストアを検査し、イメージがないときだけ明示的に pull する。
`PullAlways` は新規コンテナ作成の試行ごとに明示的な pull を要求する。`PullNever` は backend 固有で、現在のチェックアウトでは best-effort precheck を行い、image がなければ `ErrImageNotFound` を返す。Docker は `--pull=never` も渡すため strict な no-fetch 経路を持つが、Apple Container には同等の run-time switch がないため、precheck は no-fetch 保証ではない（#112）。起動後の操作や待機が再利用でない経路で失敗した場合は、作成したコンテナをロールバックしてからエラーを返す。Reuse の共有ライフタイムは後述の別契約で扱う。

オプションは functional options で提供する。
以下は現在の開発チェックアウトの API 一覧である。
ここで列挙した基本 option は `v0.2.0` にもあった。
後述する `LogsOptions` / `LogsWithOptions`、追加 HTTP setter、
合成 wait の setter は `v0.2.0` にはなかった。

- `WithExposedPorts(ports ...string)`：接続対象のコンテナポート（`"6379/tcp"` 形式）を宣言する。`MappedPort` と `Endpoint` はこの宣言を解決に使える。Docker は宣言したポートを自動公開し、Apple Container は既定でコンテナ IP から解決する。Apple ではこれは handle 側の endpoint 宣言であり、backend に別の `--expose` flag を渡さない。
- `WithEnv(env map[string]string)`：環境変数を設定する。値はコマンドライン引数ではなく一時ファイル経由で渡す。
- `WithCmd(cmd ...string)` / `WithEntrypoint(entrypoint string)`：コマンドとエントリポイントの上書き。エントリポイントは `docker run --entrypoint` の仕様どおり 1 トークンとする。複数トークンは `WithCmd` を使う。
- `WithWaitStrategy(s wait.Strategy)`：起動完了の判定方法を指定する。
- `WithName(name string)`：コンテナ名を指定する（省略時は `containergo-<ランダムな 16 進数>`）。共有の 1～63 文字 guard を使う。Apple のより厳密な 2～63 文字 rule は現在の checkout の backend preflight では強制されない（#112）。
- `WithLabels(labels map[string]string)`：追加ラベルを指定する。ライブラリが管理するラベルは予約済みである。
- `WithMounts(mounts ...Mount)`：bind、名前付きボリューム、tmpfs のマウントを指定する。現在の bind source validation は Unix-style で、Windows host path と remote Docker の bind source semantics は #76 に残る。
- `WithFiles(files ...File)`：起動後のコンテナへファイルをコピーする。コピーに失敗すると `Run` をロールバックする。現在の `WithReuse` attach ではこの option を無視する（#94）。
- `WithPublishedPort(spec string)`：ホスト側ポートの明示的な公開を指定する。Apple Container は通常コンテナへ直接接続し、Docker は `WithExposedPorts` で宣言したポートを daemon が host port へ自動公開する。特定の host port が必要な場合だけ明示的な binding を使う。
- `WithPullPolicy(policy PullPolicy)`：`PullMissing`（既定）、`PullAlways`、`PullNever` を選ぶ。現在のチェックアウトでは `WithReuse` attach が `PullAlways` を無視し、reuse creation path だけが適用する（#94）。
- `WithReuse()`：名前付き `Run` を get-or-create にする。現在のチェックアウトでは attach caller は `WithFiles` と `PullAlways` を適用せず、creation path だけで適用する（#94）。
- `WithReuseGroup(group string)`：再利用するコンテナにラベルを付け、`PruneReuseGroup` の対象にする。`WithReuse` が必要で、再利用キーには含まれない。`PruneReuseGroup` の validation grammar は現在より弱い（#102）。
- `WithCPUs(n int)` / `WithMemory(size string)`：リソース制限を指定する。`WithMemory` は現在 zero を受け付け、backend capability limit を強制しない（#102）。
- `WithUser(u string)` / `WithWorkingDir(dir string)`：実行ユーザーと作業ディレクトリを指定する。
- `WithNetwork(name string)`：既存の名前付きネットワークへ接続する。
- `WithPlatform(p string)`：`linux/amd64` のようなイメージプラットフォームを指定する（Apple Container では Rosetta を含む）。
- `WithExposedPorts(ports ...string)`：接続対象のコンテナポート(`"6379/tcp"` 形式)を宣言する
- `WithEnv(env map[string]string)`：環境変数
- `WithCmd(cmd ...string)` / `WithEntrypoint(entrypoint string)`：コマンドとエントリポイントの上書き。entrypoint は `docker run --entrypoint` の仕様上 1 トークン。複数トークンは `WithCmd` に寄せる
- `WithWaitStrategy(s wait.Strategy)`：起動完了の判定
- `WithName(name string)`：コンテナ名(省略時は `containergo-<乱数16進>` を採番)
- `WithLabels(labels map[string]string)`：追加ラベル
- `WithMounts(mounts ...Mount)`：bind、volume、tmpfs マウント
- `WithFiles(files ...File)`：起動後にコンテナへコピーするファイル
- `WithPublishedPort(spec string)`：ホスト側ポート公開(既定では公開しない。後述)
- `WithCPUs(n int)` / `WithMemory(size string)`：リソース制限
- `WithUser(u string)` / `WithWorkingDir(dir string)`：実行ユーザーと作業ディレクトリ
- `WithNetwork(name string)`：接続先ネットワーク。Docker で省略した場合は daemon が選ぶ既定 network (Linux では `bridge`、native Windows では `nat`)
- `WithPlatform(p string)`：`linux/amd64` 指定(Rosetta 利用)など

バックエンド中立の CLI 面にないオプションは意図的に提供しない。
公開 API にログ注入用のフックはない。
ログを使うには `FollowLogs` でストリームを取得するか、`Logs` と `LogsWithOptions` でスナップショットを取得する。

### Container ハンドル

以下のシグネシャ一覧には現在の開発版メソッドも含まれます。実行する
source file ではありません。

```text
type Container struct {
    // unexported fields omitted
}

func (c *Container) ID() string
func (c *Container) Host(ctx context.Context) (string, error)
func (c *Container) MappedPort(ctx context.Context, port string) (int, error)
func (c *Container) Endpoint(ctx context.Context, port string) (string, error)
func (c *Container) ContainerIP(ctx context.Context) (string, error)
func (c *Container) State(ctx context.Context) (State, error)
func (c *Container) Exec(ctx context.Context, cmd []string, opts ...ExecOption) (int, io.Reader, error)
func (c *Container) Logs(ctx context.Context) (io.ReadCloser, error)
func (c *Container) LogsWithOptions(ctx context.Context, opts LogsOptions) (io.ReadCloser, error)
func (c *Container) FollowLogs(ctx context.Context) (io.ReadCloser, error)
func (c *Container) CopyToContainer(ctx context.Context, hostPath, containerPath string) error
func (c *Container) CopyFileFromContainer(ctx context.Context, containerPath string) (io.ReadCloser, error)
func (c *Container) Stop(ctx context.Context, timeout *time.Duration) error
func (c *Container) Terminate(ctx context.Context) error
```

`Stop` の非 nil timeout は、両 backend が fractional value を truncate して whole seconds に変換する。negative や極端な値は reject されず、timeout を saturation なしで query budget に加算するため、library 側の maximum も強制されない。backend limit が適用され、#89 が 未実装の validation を担当する。

`Exec` は終了コードと標準出力、標準エラーをまとめて返す。
実行中コンテナで実行したコマンドが 0 以外で終了した場合、その終了コードはインフラエラーではなく結果として返す。
停止中または到達できないコンテナの操作はエラーを返すことがある。
`WithExecEnv`、`WithExecUser`、`WithExecWorkDir` は個別の exec 呼び出しを設定する。環境変数の値は `WithEnv` と同じ一時ファイル方式を使う。
`Logs` と、成功する `LogsWithOptions` 呼び出しは backend command が終了するため有限のスナップショットを返すが、単体で byte 上限は課さない。`Logs` は全出力を要求する。`LogsOptions` は backend 固有で、Docker は `Tail` と `Since` の両方を受け付ける。確認した Apple Container CLI の各 version は tail に `-n` を使い、`--since` option を持たない。このチェックアウトは Apple にも Docker 形式の `--tail` と `--since` を渡すため、`LogsWithOptions` は backend 中立ではない。Apple の機能として文書化する前に #82 を適用する必要がある。#82 の変更は `Tail` を Apple の `-n` に対応させ、`Since` を unsupported として拒否する予定である。範囲を指定しても大きさはコンテナ出力に依存する。`FollowLogs` は意図的に上限のない stream で、リーダーを閉じるか context をキャンセルするまで続く。`LogsOptions` と `LogsWithOptions` は `v0.2.0` にはない現在の開発 API である。generation を持つ handle では `Terminate` は世代ガードを持つが、現在の reuse と空 generation の経路には #83 と #84 が扱う制限がある（後述の Reuse を参照）。

現在のチェックアウトでは validation は一様ではありません（#102）。negative な `LogsOptions.Tail` は 0/all として扱われ、`WithMemory("0")` は現在の parser に受理され、unknown な `MountType` は `WithMounts` で reject されず、`PruneReuseGroup` は `WithReuseGroup` より弱い grammar を使います。これらは現在の動作であり、backend がその設定を受け付ける保証ではありません。

`Terminate` は Apple Container では `container delete --force`、Docker では `docker rm --force` に対応する。backend が認識した not-found CLI failure または delete 成功だけを成功として扱うため、冪等性の主張はその範囲に限られる。empty または malformed な inspect output を not-found に変換せず、現在の Docker parser は要求した target と ID や name が異なる valid object を no-match として拒否しない。どちらのケースも冪等性の保証には含まれず、unrelated な delete failure はそのまま返る。`Cleanup(t, ctr)` と `TerminateContainer(ctr)` は nil 安全なヘルパーであり、`Run` のエラーを確認する前にクリーンアップを登録できる。

`CopyFileFromContainer` は安全な host file-open semantics を持つ Docker バックエンド向けの copy-out API である。materialize された結果が regular file であることを検証するが、すべての Docker host がすべての container file type を表現できると主張するものではない。Apple Container の CLI は host が結果を開く前にすべての source file type を保持・拒否できないため、`container cp` を起動せず `ErrCopyFileFromContainerUnsupported` を返す。Docker の copy-out は client と server の両方が 29.7.0 以上である必要がある。メソッドは private な一時ディレクトリを作成する前、および `docker cp` を実行する前に両方のバージョンを確認し、確認できない場合は同じ error で fail closed する。no-follow/nonblocking な file open がない host も同じ error で fail closed する。Windows では Go 1.23 から 1.25 が `os.OpenFile` に必要な Windows file flag を伝播しないため、Docker の copy-out には Go 1.26 以降が必要である。`CopyToContainer` は両バックエンドで使用できる。

## 接続エンドポイントの設計

testcontainers の Docker 実装ではコンテナポートをホストのランダムポートへ公開し、`localhost:<mapped>` へ接続する。
Apple Container ではこの方式を既定にしない。

Apple Container では、`Host` は inspect の最初の `status.networks[].ipv4Address` から CIDR サフィックスを除いたコンテナの実 IP を返し、`MappedPort` はコンテナポートをそのまま返す。Docker は代わりに公開 host endpoint を既定にする。local daemon は loopback に publish し、検出された remote daemon は remote-safe な binding を使う。Apple の direct-IP を既定とする理由は三つある。

- Apple Container にはランダムポート割り当てがない。 ホストポートを自前で確保すると「空きポートを探してから起動する」までの競合が生じる（container-rs も同じ制約を持つ）。直接 IP 接続なら ホストポートを消費しないため、この競合が起きない。
- ホストポート衝突がないので、テストの並列実行を無制限に拡張できる。
- ポート転送プロキシを経由しないため、転送実装の不具合（大きな転送が途中で切れる事例が報告されている）の影響を受けない。

ローカルの Docker daemon でクライアントが `localhost` を要求する場合、またはコンテナ IP に到達できない構成では、`WithPublishedPort("127.0.0.1:15432:5432")` のように明示的に公開する。この loopback example は local-only であり、non-loopback の `DOCKER_HOST` では loopback binding を reject する。検出した non-loopback `DOCKER_HOST` デーモンでは remote-safe な binding と endpoint を使う。公開すると `Host` は指定したホストアドレスを、`MappedPort` はホストポートを返す。

`MappedPort` と `Endpoint` は、`WithExposedPorts` で宣言したポートと明示的に公開したポート、または利用できる host binding がないポートを `ErrPortNotExposed` にする。
宣言は wait strategy にも使われる。`ForExposedPort` は最初に宣言したポートを使い、`Target.Endpoint` の空 port も同じ最初の宣言を選ぶ。

現在の実装は、endpoint 関連メソッドが最初に行う inspect の結果をキャッシュする。
キャッシュにはコンテナ IP と host 側 binding が含まれるが、どちらも
container の lifecycle 中に変更される。`State` は新しい inspect を使うが、
endpoint の結果には古い値が含まれることがある。これは immutable な保証
ではなく snapshot の動作であり、動的データの更新は #85 が担当する。Docker が
複数 network を報告し top-level address がない場合、現在の network selection は
決定的な first-network contract ではなく unspecified である。
Docker の `host` と `none` モードは、このライブラリが管理するポート束縛を
作成できません。
`Internal: true` または isolated bridge gateway mode の外部遮断
ネットワークも、同じ endpoint 契約の対象です。
明示的に指定した default 以外の network では、`Run` は image やコンテナを
操作する前に指定 network を inspect し、これらの network と
`WithExposedPorts` または `WithPublishedPort` を組み合わせた場合は
`*ConfigError` を返します。
`host` と `none` の publish 組み合わせは起動前に拒否されます。

ポート指定なしの host モードは利用できます。
`Host` はクライアントから見たデーモンの host を返しますが、
`MappedPort` と `Endpoint` は host namespace のサービスポートを推測しません。
none モードには到達可能な host がないため、`Host` は
`ErrNoReachableHost` と一致するエラーを返します。
endpoint 解決は、要求した publish 文字列ではなく、要求した mode と inspect
の実際の network mode、host binding を照合します。
実行時の network 不一致は `ErrNetworkMismatch` で判別できます。

`WithNetwork` を省略した場合は `--network bridge` を合成せず、daemon の
platform default (Linux では `bridge`、native Windows では `nat`) に委譲します。
inspect の特別な `HostConfig.NetworkMode == "default"` は
`NetworkSettings.Networks` の具体的な network 名と照合するため、既存の
default コンテナを再利用できても、省略指定を `host`、`none`、任意の名前付き
network の wildcard にはしません。
daemon の server OS から authoritative な platform default も取得するため、
Linux の user-defined `nat` や Windows の user-defined `bridge` を default と
誤認しません。identity を取得できない場合は `ErrNetworkMismatch` で fail closed
します。

Docker の handle は `docker run --detach` が返した immutable な container ID
を保持します。Host、endpoint、lifecycle、reuse の inspect はその ID を対象
にして返された identity を検証します。network、IP、port binding は dynamic
data であり、操作ごとに更新されます。cache するのは UID、image、label などの
immutable identity だけです。

IP アドレスは `netip` で正規化します。
展開した IPv6 loopback や `::` は Docker inspect の出力と正しく
一致し、unspecified IPv6 bind はアドレスファミリーを保つ `::1` に
解決します。
リモートデーモンで loopback 束縛を inspect した場合は
`ErrEndpointUnreachable` を返します。
remote host へ書き換えても、デーモン側の loopback には到達できないためです。

`MappedPort` と `Endpoint` は、`WithExposedPorts` または
`WithPublishedPort` で宣言のないポート、および利用可能な host binding の
ないポートに対して `ErrPortNotExposed` を返します。
宣言は待機戦略(ForListeningPort の既定ポートなど)にも使います。

### Reuse の network 互換性

Docker の `WithReuse` は、image、宣言済み port、`WithNetwork` の network
identity を比較します。
`WithNetwork` 省略時の identity は daemon の platform default であり、
inspect の `default` mode を実際の network 名 (`bridge` または `nat`) と
照合します。
`host`、`none`、名前付き network の wildcard ではありません。
互換性チェックの直前と handle を返す直前に immutable Docker UID で
再 inspect します。
inspect に network mode が無い場合も互換性なしとして拒否します。
リモートデーモンの既存 loopback binding は
`ErrEndpointUnreachable` として拒否します。
`env`、`cmd`、`mounts` の差は従来どおり attach 時に無視します。

## 待機戦略

Apple Container にはヘルスチェックも wait コマンドもないため、起動完了の判定はすべてクライアント側で行う。
`wait` サブパッケージは次の戦略を提供する。

- `wait.ForLog(pattern)`：`FollowLogs` のストリームを読み、部分文字列（`AsRegexp` による正規表現も可）が現れるまで待つ。照合は行単位で行い、`WithOccurrence(n)` で出現回数を指定できる。ストリーム方式なので poll 方式ではない。
- `wait.ForListeningPort(port)`：解決したエンドポイントへの TCP 接続が成功するまで poll する。
- `wait.ForExposedPort()`：`WithExposedPorts` で最初に宣言したポートを使う。
- `wait.ForHTTP(path)`：解決したエンドポイントへ HTTP リクエストを送り、ステータスが条件（既定は 2xx、`WithStatusCodeMatcher` で変更可）を満たすまで待つ。`WithPort` と `WithMethod` で対象を指定する。現在の開発チェックアウトは `WithHeaders`、`WithHeader`、`WithBasicAuth`、`WithTLS`、`WithTLSConfig`、`WithHTTPClient` も追加している。これらは `v0.2.0` にはなかった。
- `wait.ForExec(cmd)`：バックエンド CLI でコマンドを起動し、終了コード（既定は 0）が条件を満たすまで待つ。
- `wait.ForAll(strategies...)` / `wait.ForAny(strategies...)`：戦略を合成する。子戦略は各自の設定を保持する。現在の開発チェックアウトでは公開型 `AllStrategy` と `AnyStrategy` が `WithStartupTimeout` で合成全体を制限できるが、`WithPollInterval` は公開しない。`v0.2.0` の関数は、合成全体の timeout setter を持たない interface を返す。
- `wait.ForLog(s string)`：`container logs --follow` の出力に部分文字列(または `AsRegexp` で正規表現)が現れるまで待つ。`WithOccurrence(n)` で出現回数を指定できる
- `wait.ForListeningPort(port string)`：コンテナの対象エンドポイントへ TCP 接続できるまで待つ。
  指定形式は `PORT` または `PORT/tcp` だけ。
  UDP と不正な形式は `Target.Endpoint` を呼び出す前に `*wait.ConfigError` 型で返す。
  このエラーは `wait.ErrInvalidConfiguration` と一致する。
- `wait.ForHTTP(path string)`：`net/http` で対象ポートへリクエストし、ステータスコード(既定 2xx、`WithStatusCodeMatcher` で変更可)を満たすまで待つ
- `wait.ForExec(cmd []string)`：`container exec` の終了コード(既定 0)を満たすまで待つ
- `wait.ForAll(ss ...Strategy)` / `wait.ForAny(ss ...Strategy)`：合成。`WithStartupTimeout` で合成全体のタイムアウトも設定可能

基本 strategy の起動タイムアウトは既定 60 秒である。
`ForListeningPort`、`ForExposedPort`、`ForHTTP` は既定 100 ミリ秒で poll し、`ForExec` は既定 250 ミリ秒で poll する。
現在の `ForLog` 型にも `WithPollInterval` があるが、stream を読むため効果がない。
接続・HTTP strategy は最大 1 秒間隔で停止状態を調べる。`ForExec` は poll 中に fail-fast せず、wait の期限到来時にコンテナ状態を調べる。`ForLog` は必要な pattern の出現回数に達した場合だけ成功を返す。先に log stream が終了した場合は最大 5 秒の `Running` probe を行い、停止を確認できた場合だけ「pattern 前に停止」の error を返す。それ以外は stream が先に終了したことを報告し、reader error があれば併記する。EOF だけでは停止の証明にならない。再利用でない `Run` の待機が失敗した場合はコンテナをロールバックし、上限付き log の取得が成功した場合だけ 1MiB 上限の末尾をエラーに付ける。再利用の待機失敗では共有コンテナを残す。

`ForListeningPort` と `ForExposedPort` は TCP 専用 probe です。UDP は endpoint 設定に
宣言できますが、現在の実装は probe 前に `/udp` を reject せず TCP dial に渡します。
そのため timeout したりする別の TCP listener に接続する可能性があります。不正な port
specification も wait の終了まで retry されます。#77 を参照してください。

| API | TCP | UDP |
|---|---|---|
| `WithExposedPorts` / `WithPublishedPort` | 現在の option parser が受理 | endpoint / publish 設定では受理 |
| `wait.ForListeningPort` | `PORT` または `PORT/tcp` | UDP probe ではない。UDP declaration を TCP dial する場合がある |
| `wait.ForExposedPort` | protocol にかかわらず最初の宣言を使う | 最初の宣言が UDP の場合も TCP dial に渡される |

strategy の契約は次のとおりです。

```text
type Strategy interface {
    WaitUntilReady(ctx context.Context, target Target) error
}
```

`Target` は `Endpoint`、`Running`、`FollowLogs`、`ExecCommand` を公開する小さなインターフェースである。`container.Run` が `*container.Container` を適合させる。`Target` は `ContainerIP` を公開しない。接続 strategy は解決済み endpoint を使う。この抽象化は Apple Container の direct IP と Docker の published port の両方に適合する。依存方向は `container` から `wait` だけであり、逆方向を作らないため循環参照を避けられる。

## クリーンアップ

ライブラリには通常の終了、作成後の rollback、create 失敗、異常終了の
別経路がある。
各経路でエラーの見え方は異なる。

**通常の cleanup**：`Cleanup(t, ctr)` が `t.Cleanup` 経由で `Terminate` を
登録する。`TerminateContainer(ctr)` は nil 安全な defer 形式である。
`Cleanup` の終了エラーはテストの log に残り、`TerminateContainer` は
エラーを呼び出し元へ返す。

**作成後の rollback**：再利用でない `Run` が file copy または readiness 待ちの
途中で失敗すると、作成した handle の `Terminate` を呼ぶ。削除にも失敗した場合、
返り値には元の失敗とコンテナが残ったことを示すメッセージを含める。wait の
失敗では、上限付き log の取得が成功した場合だけ log 末尾も付ける。

**create 失敗**：backend の `run` command 自体が失敗した場合、
`cleanupFailedCreate` が別の best-effort 経路で処理する。name conflict はスキップし、この process の managed / session label を持つコンテナだけを inspect する。creation label がある場合はこの run と一致해야 하지만、この best-effort 経路では label がないことを不一致とは扱わない。これは replacement を防ぐ一般的な保証ではなく、#83 が missing-generation 経路を扱います。lock、inspect、delete の error は `Run` の返り値に連結されない。

**異常終了**：親 process が終了して reaper pipe が閉じられると、外部
`/bin/sh` の watchdog が強制削除を試みる。process を終了させる未 recover の
panic、`SIGKILL`、`os.Exit` では pipe が閉じる。recover した panic では閉じない。
親が生きている間、reaper は何もしない。reaper は保険であり、トランザクション
保証ではなく、Windows では利用できない。登録 error と各 entry の delete error
は `Run` に返らない。spawn failure が retry 上限に達した場合は一度だけ log に
残り、delete failure は shell が無視する。backend 呼び出しには POSIX の
`sleep` / `kill` による期限がある。

reaper は実 CLI コンテナを非 reuse 経路で登録したときに遅延起動する。
Apple Container には別の不変 ID がないため、entry はコンテナ名と creation
generation を使う。reaper は名前で削除する前に creation label を検査するが、
per-name lock は取らない。そのため reaper の inspect / delete window は
same-name replacement と競合する。外部 CLI による delete / recreate も
名前では区別できない。これは別の cleanup limitation である。

Docker 側は `docker run` が返す不変の 64 桁 hex ID を使う準備をしている。
ただし、この checkout の `reaper.register` はまだ Apple 形式の名前だけを
受け付ける。そのため、通常の Docker `run --detach` 経路では現在の base の
reaper に Docker ID を登録できない。backend が解析可能な ID を返さない
場合だけ、`Run` は name と generation の経路にフォールバックする。
Docker-ID 登録経路には issue #73 を stack する必要がある。#73 適用後は
Docker entry が不変 ID を使い `docker rm --force` で削除する。通常の
Docker `Terminate` と rollback handle は ID を独立して利用できる。

**Reaper staging exposure**：現在の reaper は full `inspect` output を namespace の
ない `mktemp` file に staging し、通常の完了または inspect-error path では
削除する。reaper が kill されると、環境 data を含む file が残る可能性がある。cleanup 前に container-go と
reaper process を停止し、実効 `TMPDIR` で user 所有の regular file だけを metadata-only
listing し、affected time window に限定する。file content を表示・grep したり、symlink を
追跡したり、broad recursive delete を実行しない。該当 run に確実に帰属する file だけを
削除し、inspect output に含まれた可能性がある credential を rotate する（#111）。これは
no-leak 保証ではない。

**セッションラベル**：作成するコンテナには次のラベルを付ける。

- `com.github.hirokazumiyaji.container-go`：`true`（管理対象の印）
- `com.github.hirokazumiyaji.container-go.session`：プロセスごとのランダム ID
- 作成世代ラベルと、Reuse 時の再利用グループラベル

Apple CLI にはラベルフィルタがないため、孤児の掃除は `container ls -a --format json` をクライアント側で絞り込んで行う。`Prune(ctx)` は現在の backend の managed filter が選ぶコンテナを削除する。Apple は stopped 状態を選ぶ。現在の Docker filter は exited 状態だけを選ぶため、dead 状態のコンテナは #113 を適用するまで残る。running と created 状態は選ばない。Apple の現在の `Prune` と `PruneReuseGroup` は、list した candidate を per-name lock の中で fresh inspect せず name を delete するため、list と delete の間に replacement が入り込む。#98 は fresh identity/label/state の再確認と lock cleanup を担当する。

`CONTAINERGO_KEEP=1` を設定すると、自動 cleanup helper の `Cleanup` と
`TerminateContainer`、およびリーパー登録を省略する。明示的な
`Container.Terminate`、`Run` のロールバック経路、create 失敗後の
best-effort cleanup は抑制しない。`Prune` と `PruneReuseGroup` は
引き続きコンテナを削除できる。`WithReuse` の停止済みコンテナ置換も
変わらないため、条件に一致した stopped reuse container は削除・再作成され
る。この変数をグローバルな削除ロックとして扱わない。

匿名ボリュームは `--rm` でも残るので、ライブラリは匿名ボリュームを作らない。ボリュームを使う場合は名前付き，そのライフサイクルは呼び出し元に委ねる。現在の bind source validation は Unix-style で、Windows と remote Docker の bind-source semantics は #76 に残る。

## Reuse

`WithReuse` は安定した `WithName` に対する `Run` を get-or-create にする。共有は同じ host の process 間で行う。既存コンテナは現在の `WithReuse` marker を持つ必要があり、各呼び出しは自分の wait strategy を再実行する。現在の check はすべての ownership / generation label を要求せず、#83 が厳格な境界を扱う。image 互換性は両 backend で検査する。port 互換性は backend ごとに異なる。Docker は `WithExposedPorts` の自動公開 binding と明示的な `WithPublishedPort` binding を比較する。Apple は明示的な published binding を比較するが、library の Apple inspect model に `WithExposedPorts` の宣言が残らないため、その宣言は比較できない。Apple の各呼び出しは自分の exposed-port 宣言を共有コンテナ IP へ使う。宣言を分離したい場合は名前を変える。現在のチェックアウトでは `WithFiles` と `PullAlways` は reuse creation path で適用されるが、既存共有コンテナへの attach では無視される（#94）。`env`、`cmd`、`mounts` の差は既存コンテナへ黙って attach する。分離が必要なら異なる名前を使うか、`Exec` で状態を初期化する。

このチェックアウトが作成するコンテナは通常 16 桁の 16 進数
`creationLabel` generation を付ける。ただし、現在の reuse 経路の
ownership 確認は `WithReuse` marker と互換 image だけで、managed
label と creation label のすべてを要求しない。inspect した generation
が空の stopped コンテナは name ベースの delete に到達できる。
`Terminate` は non-empty generation を持つ handle にだけ fresh inspect
による確認を行う。空 generation の handle は legacy name-delete 経路を
使う。`reuseRun` は readiness の後に再 inspect しないため、wait 中に
置き換えが起きる可能性がある。#83 と #84 が ownership 確認と最終
generation 確認を追加する。`CONTAINERGO_KEEP=1` はこの reuse rule を変えない。
条件に一致した stopped reuse container は削除・再作成され、
`PruneReuseGroup` はその group を削除できる。

有効な non-empty generation を持つ Apple の handle では、delete 前に
fresh inspect を行い、inspect と delete を一時ディレクトリ内の名前単位
`flock`（`containergo-<name>.lock`）で直列化する。同じ host で本ライブラリの
generation-checked `Terminate` と failed-create cleanup は保護される。
現在の Apple `Prune`/`PruneReuseGroup` list-to-delete path は candidate を
再 inspect せず lock を保持しないため、#98 がこの race を扱う。外部 reaper も
現在のチェックアウトではこの lock を取らない。これは別の cleanup limitation
である。外部ツールによる同じ窓の delete / recreate も保証外である。Docker の
handle は `docker run` が出力した完全な不変 `Id` を保持するならその ID で
削除するため、同じ名前の置き換えとは delete target の ID が一致しない。
inspect から得る `Id` は target validation に依存し、現在の Docker parser は
その検証を行わない（#103）。ただし、現在の base でその他の Docker operation
は logical name を対象にする。#74 がそれらの operation にも immutable ID を
使うよう修正する。これらは別の保護であり、現在の base は reuse を一般的な
fail-closed 保証としていない。
watchdog reaper は Apple コンテナを名前と generation で登録し、label を行頭
固定の JSON field として読む。ラベルの部分文字列では一致にせず、世代が違えば
削除しない。現在の base は Docker の immutable ID を reaper に登録しない。
Docker reaper entry が `Id` を使うには issue #73 が必要である。各 backend
呼び出しは POSIX の `sleep` と `kill` による 10–30 秒の期限を持ち、1 つの
daemon 呼び出しが後続を妨げない。リーダーの pull と create は独立した
`runTimeout` 予算を使い、`reuseAttachTimeout` は別 process のコンテナへの
attach polling だけを制限する。

## セキュリティ設計

外部プロセスを起動するライブラリとして、次の原則は security boundary を表す。ただし #111 の reaper staging exception があるため、「情報漏洩がない」という end-to-end 保証ではない。

**シェルを経由しない**。すべての CLI 呼び出しは `exec.Command` に引数配列を渡し、シェル文字列を組み立てない。唯一の例外は watchdog reaper の shell script である。本文は固定文字列で、container ID は stdin data としてだけ渡す。script は `set -f`、`IFS=`、`read -r`、変数の quote で word splitting と glob 展開を封じる。現在の base では、name-addressed reaper entry に共有 library guard `^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`（1～63 文字）を適用してから pipe へ書く。この shared guard は Apple Container の完全な契約ではない。Apple の CLI は `^[a-zA-Z0-9][a-zA-Z0-9_.-]{1,62}$` の 2～63 文字を要求し、現在の checkout はその backend 固有の preflight check を適用しない（#112）。issue #73 適用後は、reaper は完全な小文字 64 桁 hex Docker ID も別々に受け付ける。reaper 登録 error は無視する。二重の防御により、登録された ID 経由の command injection を成立させない。
**シェルを経由しない**。
すべての CLI 呼び出しは `exec.Command` に引数配列を渡す形で行い、シェル文字列を組み立てない。
唯一の例外は watchdog リーパーのシェルスクリプトである。
ここはスクリプト本文を固定文字列とし、コンテナ ID は標準入力からデータとして渡す。
スクリプト側は `set -f`(グロブ無効)、`IFS=` と `read -r`、変数のクォートで語分割とグロブ展開を封じる。
ライブラリは、パイプへ書く前の ID を検証し、Apple Container の名前 `^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$` または Docker の完全な小文字 64 桁の 16 進数 ID `^[0-9a-f]{64}$` だけを受け入れる。
二重の防御により、ID 経由のコマンド注入を成立させない。

**環境変数を argv に載せない**。`--env key=value` を使うと、値がプロセス一覧（`ps`）から見える。データベースパスワードなど秘密情報を環境変数で渡す用途による情報漏洩を避けるため、ライブラリは `os.MkdirTemp` の mode 0600 ファイルへ環境変数を書き、`--env-file` で渡して起動後に削除する。

**Reaper staging**。現在の reaper は full `inspect` output を namespace のない `mktemp` file へ書き込んでから削除する。kill された場合、環境 data を含む file が残るため、#111 の mitigation を参照する。
**入力を検証する**。
コンテナ名は前述の名前規則、ラベルキーは CLI と同じ Docker/OCI 形式、ポートは数値範囲とプロトコル(`tcp`/`udp`)、環境変数キーは `=` と NUL を含まないこと、コピー対象のパスは `/` 区切りの POSIX 絶対パスでバックスラッシュを含まず、有効な UTF-8 であることを、CLI へ渡す前に検証する。
CLI 側にも検証はあるが、ライブラリ側で先に落とすことでエラーメッセージを明確にし、将来の CLI 側検証の変化にも依存しない。

**入力を検証する**。共有 `WithName` guard と name-addressed reaper check は前述の規則を使い、ラベルキー（CLI と同じ Docker/OCI 形式）、ポート（数値範囲と `tcp`/`udp`）、環境変数キー（`=` と NUL を含まない）、コンテナ内コピー先パス（絶対パス、有効な UTF-8、NUL なし）も CLI へ渡す前に検証する。この library-side guard は backend の完全な name validation を意味しない。Apple のより厳密な minimum は現在の checkout では強制されない（#112）。ホスト側コピー元パスは絶対パスへ解決する。CLI にも検証はあるが、先にライブラリで落とすことでエラーメッセージを明確にし、将来の CLI の変化に依存しない。public option の validation は部分的で、negative `LogsOptions.Tail`、zero memory、unknown mount type、reuse-group grammar は backend work 前に一様に reject されない（#102）。

**認証情報を扱わない**。レジストリ認証はバックエンド CLI に委ねる。Apple Container では `container registry login`、Docker では `docker login` を使う。ライブラリに認証情報を入力する経路はない。

**ログ注入 API はない**。公開 API にロガーフックはない。`CLIError` は診断用に失敗したコマンドの引数と上限付き stderr を提供するが、環境変数の値は一時 env ファイルにあり argv には現れない。watchdog は起動を繰り返し失敗した場合に標準ログへ一度だけメッセージを出す。

## パフォーマンス設計

**子プロセス数を最小にする**。作成と起動はバックエンドの `run --detach` 1 回で行う。最初の inspect 結果を endpoint 関連経路が再利用する。ただし現在のキャッシュにはコンテナ IP と host 側 binding も含まれ、これらは動的な値である。`State` と一部の lifecycle 操作は再 inspect する。これは stale data の制限であり、immutable な保証ではない。動的 endpoint データの更新は #85 が担当する。

**接続で確認できる待機は接続で行う**。`ForListeningPort` と `ForHTTP` は解決したエンドポイントへ直接接続する。`ForExec` と状態照会はバックエンド CLI を呼び出し、`ForLog` はログストリーム API を使う。接続 probe の既定間隔は 100ms、exec probe は 250ms である。

**並列起動を妨げない**。コンテナ作成にグローバルロックを置かない（reaper の ID 登録だけ 1 行の書き込みにミューテックスを使う）。Apple Container は既定でホストポートを消費せず、Docker はデーモンが公開ポートを原子的に割り当てる。

**要求された範囲だけスナップショットを制限する**。`Logs` と成功する `LogsWithOptions` 呼び出しは有限の CLI snapshot を完了するが、`Logs` は利用可能な全出力を buffer できる。`LogsOptions{Tail, Since}` は backend 固有で、Docker は両方を受け付けるが、確認した Apple Container CLI の各 version は Tail に `-n` を使い Since を持たない。このチェックアウトは Apple にも Docker の flag を渡すため、option の契約は #82 を待つ。どちらの経路も byte 上限を追加しない。`FollowLogs` は意図的に上限のない stream であり、`io.ReadCloser` を閉じるか context をキャンセルすると CLI process を終了する。wait failure の診断 log 末尾は 1MiB 上限である。

**操作ごとに期限を決める**。inspect、copy、スナップショットログ、delete、prune などの照会的な操作は、呼び出し元に deadline がなければ通常 30 秒を既定にする。`Run` と明示的な image fetch は pull と create に 10 分の予算を使う。共有 pull のリーダーだけは、個別の呼び出し元のキャンセルから意図的に切り離す。一人の呼び出し元が他の待機者の pull を中止できないようにするためで、各待機者は自分の context がキャンセルされた時点で待機を終了する。`Exec` のコマンド呼び出しと `FollowLogs` はライブラリ側の deadline を追加せず、呼び出し元の context をそのまま渡す。`Exec` のインフラ確認には、期限付きの後続 context を使うことがある。共有 pull のリーダー以外の操作でライブラリ側の既定を適用するときには、既存の呼び出し元 deadline を保持する。

## エラー処理

root と backend の error は `errors.Is` と `errors.As` で判別できる状態を意図する。ただし現在のチェックアウトには追加の制限がある。`Classify` は元の CLI error を `ErrSystemNotRunning` wrapper の text として format し、unwrap target として保持しない（#104）。target に一致する entry がない successful inspect response は `ErrContainerNotFound` ではなく generic error を返す場合がある（#103）。Docker では empty または malformed な inspect data がこの generic path に入り得ます。現在の parser は返された object が要求した ID や name に一致するかも検証しないため、valid だが mismatched な object は信頼できる no-match signal ではない。built-in wait strategy は timeout / cancellation 経路ごとに context error を一様に保持していない（#92）。primitive timeout や `ForLog` cancellation は string-only になる場合があり、composite strategy は context error を保持する場合がある。これらの follow-up issue を適用するまでは一つの error-chain contract を仮定しない。

- `ErrSystemNotRunning`：CLI が non-zero exit を返した後にバックエンドの liveness probe も失敗した場合。Apple Container のヒントは `container system start`、Docker のヒントは Docker daemon の起動である。missing または unlaunchable な CLI binary は launch error のままである。現在の wrapper は sentinel を保持するが元の `*CLIError` を text に flatten する（#104）。
- `ErrContainerNotFound`：backend が missing container を示す CLI failure を分類した場合。target に一致する entry がない successful inspect response はこの sentinel を保証せず、generic error になる場合がある（#103）。Docker では empty または malformed な inspect data がその generic path に入る。valid だが mismatched な object は現在の parser では no-match として拒否されない。
- `ErrImageNotFound`：現在の `PullNever` precheck がローカルイメージを見つけなかった場合。Apple では best-effort な backend-specific precheck であり、no-fetch 保証ではない（#112）。
- `ErrPortNotExposed`：ポートが宣言も公開もされておらず、または宣言済みポートに利用できる host binding がなかった場合。
- `ErrGenerationReplaced`：delete 時の generation check が同じ名前の置き換えを検出した場合。現在の reuse 経路には readiness 後の最終 check（#83、#84）がない。
- `*CLIError`：バックエンド CLI が 0 以外で終了した場合。バイナリ、引数、終了コード、stderr（診断用の stderr は 64KiB 上限）を保持する。root の `CLIError` alias は `v0.2.0` にはない現在の開発版追加である。現在の `ErrSystemNotRunning` 分類は元の error を text に flatten する場合がある（#104）。
- `ErrContainerNotFound` と `ErrGenerationReplaced` も現在の開発版追加である。
- `ErrSystemNotRunning`：CLI 呼び出しが失敗した際に `container system status` を追加で照会し、サービス未起動と判定できた場合に返す。メッセージに `container system start` の実行を促す文言を含める
- `ErrContainerNotFound`：inspect などの not found
- `ErrPortNotExposed`：`WithExposedPorts` 未宣言のポート照会
- `ErrCopyFileNotRegular`：Docker の copy-out 結果が regular file でない
- `ErrCopyFileFromContainerUnsupported`：選択した backend または host が型安全な copy-out を実装していない(Apple Container、Docker client/server が 29.7.0 未満、必要な open flag がない host、または Windows Go 1.23 から 1.25)
- `ErrInvalidConfig` / `*ConfigError`：作成前に拒否した backend 非互換の option 組み合わせ
- `ErrPortNotExposed`：未宣言、または利用可能な host binding がない port
- `ErrEndpointUnreachable`：リモートデーモンの loopback など、client から到達できない binding
- `*CLIError`：上記以外の CLI 失敗。実行したサブコマンド、終了コード、stderr(上限 64KiB)を保持する

backend の `run` が成功した後に非 reuse `Run` が失敗した場合、作成後の rollback error を返す。rollback 削除にも失敗すると、コンテナが残ったことを message に含める。backend の `run` 自体の失敗は別の best-effort `cleanupFailedCreate` 経路を使い、その cleanup error は元の classified error に連結しない。reaper の登録・削除 error も返さない。Reuse は wait error を返し、共有コンテナは残す。

ライブラリ自身が `container system start` を実行することはない。このコマンドはカーネルインストールで対話プロンプトを出しうるため、テストライブラリが暗黙に実行してよい操作ではない。

## パッケージ構成

以下は中核となる構成であり、ファイル一覧ではない。
ルートパッケージは公開 API とバックエンド中立のライフサイクルコードを持つ。
バックエンド実装は argv の組み立てと inspect の正規化を担う。

```
container-go/
├── container.go      // Run、Container、エンドポイントと状態
├── options.go        // 公開 functional options と検証
├── wait_adapter.go   // Container から wait.Target への適合
├── pull.go           // pull policy とプロセス内 pull 集約
├── reuse.go          // WithReuse と PruneReuseGroup
├── cleanup.go        // Cleanup、TerminateContainer、Prune
├── reaper.go         // watchdog reaper
├── exec.go           // Exec と ExecOption
├── logs.go           // Logs、LogsWithOptions、FollowLogs
├── copy.go           // WithFiles と copy メソッド
├── errors.go         // 公開エラー値
├── backend.go        // バックエンド選択
├── engine.go         // バックエンド interface と正規化情報
├── engine_apple.go   // Apple Container の argv / inspect 適合
├── engine_docker.go  // Docker の argv / inspect 適合
├── namelock.go       // 協調プロセス用の名前 lock
├── flight.go         // プロセス内 single-flight helper
├── internal/cli/     // CLI runner、streaming、エラー分類
├── internal/inspect/ // inspect JSON モデルとデコード
└── wait/             // 公開待機戦略
```

`internal/cli` の runner はインターフェースであり、テストではフェイク実装を注入する。
`internal` パッケージは外部 API ではないため、利用者は root と `wait` の公開パッケージに依存する。

内部 runner の契約は次のとおりです。

```text
type Runner interface {
    Run(ctx context.Context, args ...string) (stdout []byte, stderr []byte, err error)
}
```

## テスト戦略

**ユニットテスト**では、固定 JSON を返すフェイク `Runner` を注入し、実機なしで argv 組み立て、JSON デコード、エラー分類、wait strategy のロジックを検証する。本番 code が非 nil を前提とする依存には、テストでもフェイク実装を渡す。integration tag のない `examples/compile_test.go` は、README の英語版・日本語版と design document の両方から fence された code block をすべて抽出し、対応する block をコメント除去後に比較する。`go` block は parse して一時 module 内で compile し、一時 module は現在の checkout への引用符付きローカル `replace` を使う。`text` のシグネシャ一覧は実行対象ではない。

**統合テスト**は `integration` build tag を使う。root のテストスイートには Apple Container と Docker のライフサイクル、接続、exec、copy、cleanup、Reuse、watchdog のケースがある。各バックエンドの helper は先に CLI とサービスを確認し、利用できなければ skip する。`make integration` は両バックエンドを実行して pull が重い bench と singleflight を除外し、`make integration-docker` は Docker を選択する。`make bench-integration` は pull が重いシナリオと独立したベンチマークモジュールを実行する（nested `bench/` module は Go 1.25+）。

**CI**：`.github/workflows/ci.yml` は `ubuntu-latest` で Go 1.23.0 と安定版 Go を使い、ユニットテスト、race テスト、lint、`govulncheck` を実行する。同じ runner で Docker 統合テストの行列も実行する。Apple Container 統合テストは意図的にローカル専用である。GitHub の Linux runner には Apple Container サービスと必要なホスト環境がない。ローカルでは `CONTAINERGO_BACKEND=apple` または `CONTAINERGO_BACKEND=docker` でバックエンドを 1 つだけ実行できる。
**統合テスト**：ビルドタグ `integration` で分離し、実機 backend に対して実行する。Apple test は macOS 26 と起動済みの Apple Container を、Docker test は稼働中の daemon を必要とする。Apple の watchdog test は Darwin 専用で、リポジトリの Linux 限定 CI job では Windows runtime integration を実行しない。最初に backend の利用可否を確認し、daemon/service が停止していれば skip する。起動、接続、exec、コピー、通常のクリーンアップ、Darwin の watchdog を検証する。Windows の copy-out は Go 1.26 以上と Docker client/server 29.7.0 以上を使って手動検証する。

**CI**：unit、race、vet、lint、Docker integration job は Ubuntu で実行する。Apple integration は service と host の条件が満たされないためローカルで実行し、Windows runtime job は設定されていない。

## バックエンド（v0.2 と現在の開発版）

v0.1 は Apple Container 専用だった。
v0.2 で Docker バックエンドを追加し、Linux と Windows でも同じ API を使う。

**選択**：`CONTAINERGO_BACKEND` 環境変数を最優先し、`apple` または `docker` を受け付ける。未指定なら OS で決め、macOS は Apple Container、Linux と Windows は Docker になる。macOS で Docker Desktop を使う場合は `CONTAINERGO_BACKEND=docker` を設定する。

**方式**：Docker も `os/exec` の CLI ラッパーにする。container-rs は Docker Engine API を直接呼ぶが、本ライブラリは直接 API を使わない。直接 API クライアントは tar の生成、ログストリームの逆多重化、レジストリ認証、Windows の名前付きパイプを自前実装することになるため、CLI ラッパーを使う方が既存の runner 層（argv 実行、timeout、streaming）を共有できる。`DOCKER_HOST`、Docker context、認証の解決は docker CLI に委ねる。ただし、このライブラリが remote endpoint として検出するのは `DOCKER_HOST` だけで、Docker context の remote daemon は検出しない。

**内部構造**：バックエンドは argv 組み立てと inspect 正規化だけを持つ内部 interface にする。プロセス実行（runner）、待機戦略、cleanup、検証は両バックエンドで共有する。正規化した記録には state（running / stopped / stopping / created / unknown へ写像）、label、image 参照、利用できる場合の backend ID、コンテナ IP、host 側 port binding（コンテナポートから host アドレスとポートへ）を持つ。backend ID が stable identity で、IP と binding は動的である。現在の endpoint cache は後者を保持する。

**Image 処理**：両バックエンドとも、明示的な image inspect と pull コマンドで pull policy を実装する。Docker の run argv は `--pull=never` を追加し、Apple Container も同じ明示的な policy 経路を使う。並行 pull は同一プロセス内で、backend、image、platform、操作が同じ場合だけ集約する。
**内部構造**：バックエンドは「引数の組み立て」と「inspect 出力の正規化」だけを担う内部インターフェースにする。
プロセス実行(ランナー)、待機戦略、クリーンアップ、検証は両バックエンドで共有する。
正規化した情報には、状態(running / stopped / stopping / unknown への写像)、ラベル、immutable identity、image、コンテナ IP、Docker network mode、公開ポートの束縛(コンテナポート → ホストアドレスとポート)を含める。
状態、IP、network、port binding は dynamic data として inspect のたびに更新し、cache には immutable identity のみを保持する。

**接続エンドポイントの違い**：Docker Desktop(macOS / Windows)ではコンテナ IP にホストから到達できないため、Docker バックエンドは testcontainers と同じ公開ポートモデルを既定とする。
`WithExposedPorts` で宣言したポートは自動的にランダムポートへ公開する(ローカルは `-p 127.0.0.1::<port>`、リモートデーモンでは `-p 0.0.0.0::<port>`)。
リモートとは、このマシンを指さないあらゆる `DOCKER_HOST` のことであり、`tcp://host`、`ssh://user@host`、スキームなしの `host:port` / ホスト名が該当する。
一方 `unix://`、`npipe://`、ループバックアドレス、空値はローカルのままである。
Docker CLI が受け付けるクライアントプロトコルは `unix`、`tcp`、`npipe`、`ssh` であり、それ以外のスキームは利用可能な `DOCKER_HOST` ではない。
`Host` は `127.0.0.1`(リモート時はそのホスト名)、`MappedPort` は割り当てられたホストポートを返す。
unspecified 束縛は loopback へ解決しながら IPv4 と IPv6 のファミリーを維持し、明示的な IPv6 アドレスは `netip` で正規化する。
リモートデーモンで明示または再利用された loopback 束縛は拒否する。
その待ち受け先はリモートマシンの loopback であり、client 側の書き換えでは到達できないためである。
`ssh://` のホスト名は直接 dial 可能でなければならず、ProxyJump や踏み台越しのエイリアスは手動の `ssh -L` 転送が必要である(CLI の SSH セッションが運ぶのは Docker API だけであるため)。
`docker context` 経由のリモート指定は検知できない。
ランダム割り当てはデーモンが起動時に原子的に行うため、Apple Container で避けた「空きポート確保の競合」は発生しない。
Apple Container バックエンドの既定(直接 IP)は変えない。

**接続エンドポイントの違い**：Docker Desktop（macOS / Windows）ではホストからコンテナ IP に到達できないため、Docker バックエンドは testcontainers と同じ公開ポート方式を既定にする。
`WithExposedPorts` で宣言したポートは自動公開される。
ローカルでは `-p 127.0.0.1::<port>`、検出した non-loopback `DOCKER_HOST=tcp://...` デーモンではクライアントが到達できるように `-p 0.0.0.0::<port>` を使う。
`Host` は `127.0.0.1`（`tcp://` の `DOCKER_HOST` ならそのホスト）、`MappedPort` は割り当てられたホストポートを返す。
loopback と unspecified の binding は `defaultHost()` に読み替えるため、その remote デーモンで観測した `127.0.0.1` binding もリモートホストとして解決する。
検出した remote デーモンで loopback を明示した `WithPublishedPort` は `Run` が拒否する。
Docker はリモートマシンの loopback でしか listen せず、クライアント側の書き換えでは到達できないためである。
ライブラリがリモートデーモンを検出するのは `DOCKER_HOST` だけで、リモートデーモンを指す `docker context` は検出しない。
デーモンが起動時にポートを原子的に割り当てるため、Apple Container で避けた空きポートの競合は再び起こらない。
Apple バックエンドの直接 IP の既定は変えない。

**Cleanup の違い**：watchdog reaper は backend ごとに delete subcommand を切り替える（Apple は `delete --force`、Docker は `rm --force`）。`/bin/sh` に依存するため Windows では動かず、Windows は `Cleanup` と通常の rollback 経路に依存する。現在の checkout には Docker-ID 登録の前提条件（#73）がないため、通常の Docker handle が不変 ID を使う場合でも、reaper の entry には登録されません。`Prune` は Docker で daemon 側 filter を使うが、現在の filter は `status=exited` だけを選び、dead 状態は #113 を適用するまで選ばない。

**liveness detection**：probe command は backend ごとに切り替える（Apple は `system status`、Docker は `version --format {{.Server.Version}}`）。

## スコープ外

- `container build` / `docker build` による Dockerfile ビルド。
- ネットワークの作成と管理。`WithNetwork` は既存の名前付きネットワークへ接続できるが、ライブラリはネットワークを作らない。
- ボリュームの作成とライフサイクル管理。`WithMounts` は bind、名前付きボリューム、tmpfs を使えるが、ライフサイクルは呼び出し元が管理する。現在の bind source validation は Unix-style で、Windows と remote Docker の bind-source semantics は #76 に残る。
- postgres などの testcontainers module に相当する高水準パッケージ（コアが安定してから再検討）。
- Docker Engine API の直接クライアント。現在の transport は CLI ラッパーであり、直接クライアントは隠れた fallback ではなく将来的な決定事項である。
- `container build` / `docker build` による Dockerfile ビルド
- ネットワークの作成と管理(`WithNetwork` で既存の Docker network へ接続できる)
- ボリュームの作成と管理
- testcontainers のモジュール群(postgres など)に相当する高水準パッケージ(コア安定後に検討)
- Docker Engine API の直接クライアント(CLI ラッパーで足りなくなったら再検討)

## 未解決の製品上の決定

現在の API が暗黙に意味を与えない事项を、ここに明記する。

- **Apple の名前ベース削除**：non-empty で一致する作成世代チェックと per-name `flock` は、同じ host で本ライブラリの generation-checked `Terminate` / failed-create cleanup を保護する。現在の `Prune` / `PruneReuseGroup` list-to-delete path は candidate を再 inspect せず lock も保持せず、外部 reaper もこの lock を取らない。現在の base は空 generation の name delete も許し、同じ窓で外部の CLI が delete して再作成した場合には区別できない。#83、#84、#98 が ownership、最終確認、prune list-to-delete の gap を扱い、外部競合を閉じるには backend の不変 ID または原子的な条件付き削除が必要である。
- **remote Docker の検出**：endpoint 選択に使うのは `DOCKER_HOST=tcp://...` だけである。remote Docker context は検出しない。
- **Reuse の互換性**：両 backend で image を検査する。Docker は published port binding も検査するが、Apple は inspect data に `WithExposedPorts` の宣言を残さないため比較できない。`env`、`cmd`、`mounts` の差は意図的に attach する。今後の release で設定を比較すべきかは未解決の製品上の決定であり、分離が必要なら別の名前を使う。
- **logger injection**：公開 logger hook は存在しない。追加するには新しい API と、公開できるコマンドデータの範囲を決める決定が必要である。
- **Windows / remote bind mount**：現在の validation は Unix-style で、remote Docker daemon が client host path を解決できる保証はない。#76 が capability boundary を追加する。
- **UDP readiness**：`ForListeningPort` と `ForExposedPort` は TCP-only だが、UDP は endpoint 設定に宣言できる。#77 が fail-fast protocol validation を追加する。
- **Stop timeout unit**：現在の変換は fraction を truncate し、negative、overflow、backend limit を validation しない。#89 が public contract を追加する。
- **Wait error chain**：built-in strategy は context error を一様に保持しない。#92 が normalization を追加する。
- **Public option validation**：negative log tail、zero memory、unknown mount type、reuse-group grammar は一様に reject されない。#102 が typed validation を追加する。
- **Reaper staging**：異常終了時に inspect output の environment data を disk に残す余地がある。#111 が cleanup と mitigation を追加する。
- **reaper 登録**：登録は best-effort で、現在は Apple 形式の名前だけを受け付ける。Docker の full ID を登録するには #73 の前提条件が必要である。登録 error は無視され、通常の cleanup が残る。
- **`ForLog.WithPollInterval`**：現在のセッターはログの待機がストリーム方式なので効果がない。削除するか別の意味を与えるかは未解決である。
- **Docker Engine API の直接化**：現在のベンチマークの決定は延期する。CLI latency や別の要件が合意した閾値を超えた場合にだけ再検討する。

## 実装フェーズ（歴史）

以下は元の実装順序を記録したものである。
設計の歴史として保持するものであり、現在の API やロードマップの契約ではない。

1. プロジェクト基盤：go.mod、CI、Makefile
2. CLI runner 層：`internal/cli`、timeout、エラー分類、`ErrSystemNotRunning` 判定
3. inspect JSON モデル：`internal/inspect`
4. Core API：`Run`、options、`Container` の lifecycle（start、Stop、Terminate、rollback）
5. 接続情報 API：`Host`、`MappedPort`、`Endpoint`、`ContainerIP`、`WithPublishedPort`
6. 待機戦略：`wait` パッケージ一式
7. Exec、Logs、Copy
8. Cleanup：`Cleanup`、`Prune`、session labels、watchdog reaper
9. Security 仕上げ：env-file 経由の環境変数、入力検証の網羅、log masking
10. 統合テストと手順化
11. ドキュメントとサンプル：README、使用例

v0.2（Docker backend）は次の順で進めた。

12. Backend abstraction：argv 組み立てと inspect 正規化の interface、Apple 実装の切り出し
13. Docker engine：run / inspect / lifecycle / exec / logs / copy の argv と JSON 解析
14. Docker の接続情報：ランダム公開ポート、`Host` / `MappedPort`、`DOCKER_HOST`
15. Backend 選択と周辺：`CONTAINERGO_BACKEND`、OS 既定、reaper / Prune / probe の切替
16. Docker 統合テストとドキュメント更新

## 参考資料

- [apple/container](https://github.com/apple/container) v1.2.2 と v1.3.0 の command・source リファレンスおよび `ContainerResource` ソース。
- [shiguredo/container-rs](https://github.com/shiguredo/container-rs)：XPC 直結方式の先行実装。watchdog reaper、cleanup contract、macOS 固有の制約（ポート競合、転送切断）の整理を参考にした。
- [testcontainers-go](https://github.com/testcontainers/testcontainers-go) v0.44.0：API 形状（functional options、wait 戦略、nil 安全な cleanup）の参照元。
