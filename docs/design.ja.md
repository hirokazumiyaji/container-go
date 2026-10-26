# container-go 設計ドキュメント

English (primary): [design.md](design.md)

作成日: 2026-08-18（v0.2 バックエンド節を 2026-08-19 追記）
最終同期: 2026-10-26
対象: Apple Container CLI 1.2.2 と 1.3.0（macOS 26 以降、Apple Silicon。リポジトリの根拠で表した version）、Docker 29.x（ローカル 29.7.2 と v29 inspect fixture。Linux、Windows、macOS）、root module Go 1.26 以降、nested `bench/` module Go 1.26 以降

この文書は現在のチェックアウトにある実装を説明する。
「実装フェーズ」の節は初期設計の歴史として残るが、現在の API や今後のロードマップを約束するものではない。
現在の節で説明する公開 API と動作は、実装とテストを基準にしている。
backend に存在しない capability（たとえば Apple Container の `--since`
log option の不在）は、未実装の作業ではなく現在の動作として説明する。
リリースの履歴は CHANGELOG.md に置く。

最新のタグ付きリリースは `v0.2.0`（2026-09-02）です。
このチェックアウトはそのタグより後の開発版です。
`v0.2.0` モジュールには Go 1.27 以降が必要ですが、現在の root チェックアウトには
Go 1.26 以降が必要で、nested `bench/` モジュールには Go 1.26 以降が必要です。
リリース版 API と現在の開発版 API は同じもの
ではありません。`LogsOptions` / `LogsWithOptions`、追加された
`wait.ForHTTP` の setter、公開型 `wait.AllStrategy` / `AnyStrategy` と
合成 strategy の `WithStartupTimeout`、root の `CLIError`、
`ErrContainerNotFound`、`ErrGenerationReplaced` は `v0.2.0` 以降の
開発版追加です。基本の `Run`、options、lifecycle、endpoint、`Exec`、
`Logs`、`FollowLogs`、copy、pull policy、Reuse、Cleanup、backend 選択、
元の wait strategy は `v0.2.0` に存在しています。

## 目的

**container-go** は、Apple Container（[apple/container](https://github.com/apple/container)）と Docker をバックエンドとする testcontainers スタイルの Go ライブラリである。
Go のテストコードから使い捨てコンテナを起動し、接続情報を返し、作成したコンテナを通常のテスト終了時と異常終了時のベストエフォート経路で削除する。

先行事例として、Rust には [shiguredo/container-rs](https://github.com/shiguredo/container-rs) がある。
本ライブラリは同じ問題領域を Go で扱うが、実現方式は後述のとおり異なる。

設計上の制約は次の三つである。

- **最小依存**：標準ライブラリと、プラットフォームの process と file 操作に使う `golang.org/x/sys` のみを依存する。コンテナクライアントのサードパーティ Go モジュールは使わない。
- **セキュリティ**：subprocess argv でインジェクションを防ぐ。現在の watchdog reaper は inspect output を構造化フィルタへ流し、raw JSON を disk に staging しない。旧バージョンの staging file には環境データが残り得るため、そのバージョンを実行した環境では後述の Security 節の cleanup 手順に従う。
- **パフォーマンス**：テストスイートを左右するのはコンテナ（VM）の起動時間である。ライブラリ側のオーバーヘッドをそれに対して無視できる水準に保ち、並列起動を妨げない。

## 前提とする Apple Container の仕様

設計の根拠となる Apple Container の CLI・ソース仕様(フィクスチャは 1.2.2 と
1.3.0)を先に整理する。API サーババージョンは live check が必要になった時点で
実行時に取得し、CLI バージョンから推測しない。取得できない場合は確認済みとは
扱わず、利用不可として報告する。これは 1.2.x や 1.3.x のすべての release を
確認済みとする互換性宣言ではない。

- ホスト要件は macOS 26 以降かつ Apple Silicon である。
- 既定では、各コンテナは軽量 VM として起動し、vmnet ブリッジ（network は `default`、`192.168.64.0/24`）上の実 IP を持つ。ホストはこの IP に直接到達できるため、ポート公開（`--publish`）は必須ではない。
- すべての操作は `container` CLI から行える。`ls --format json` と `inspect` は機械可読な JSON を返し、追加フィールドは `internal/inspect` が無視する。
- CLI は launchd 配下の `container-apiserver` と XPC で通信する。サービスが未起動だとコマンドは失敗し、`container system status` で状態を確認する。
- コンテナ名がそのまま ID になる。共有 library guard は
  `^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$` で、1 文字から 63 文字までを受け付ける。Apple Container の CLI はより厳密な 2～63 文字の規則 `^[a-zA-Z0-9][a-zA-Z0-9_.-]{1,62}$` を持ち、Apple バックエンドは image 解決の前にこれを適用する。
- ヘルスチェック、`wait` コマンド、イベントストリーム、`ls` のラベルフィルタ、実行中コンテナへの再アタッチは存在しない。必要な動作はクライアント側で実装する。
- `--label` は存在するが、ラベルの絞り込みは JSON 出力をクライアント側で行う必要がある。ラベルキーは小文字の Docker/OCI 形式に限られる。
- `run`/`create` は Linux image のみを実行する。platform grammar は `os/arch[/variant]` で、bare `linux` や architecture に定義されていない variant を拒否する。Apple では platform 未指定時の `CONTAINER_DEFAULT_PLATFORM` を実効 cfg platform に解決し、image inspect、pull flight、pull、run で同じ値を使う。resource / forwarding の制約として memory は 200 MiB 以上、publish port は 2〜65535、publish descriptor は最大 64 個。network name は英小文字の 1〜63 文字で、英数字の途中にだけ dot/hyphen/underscore を使える。
- `container cp` は実行中のコンテナに対してのみ使える。copy-out には型を保持し symlink を追跡しないモードがなく、確認したバージョンでは host 側の検証前に symlink、FIFO、device node を dereference/consume することがある。そのため `CopyFileFromContainer` は Apple Container では `ErrCopyFileFromContainerUnsupported` を返し fail closed する。Docker では型を保持する extractor を利用するため client/server の両方が 29.7.0 以上を必要とし、必要な flag を host が扱える場合に host 側の Lstat/open 検査を維持する。どちらの条件も満たさない場合は fail closed する。
- エラー分類は `engine_apple.go` と `engine_docker.go` の、backend binary・command・行頭固定の CLI stderr 形式に依存する(名前衝突、image/container missing)。1.2/1.3 と Docker 29 の記録フィクスチャ、および `cli_compat_integration_test.go` のライブ CLI で回帰を確認する。capability matrix はさらに `CONTAINERGO_BACKEND=apple` と `CONTAINERGO_APPLE_LIVE=1` を要求し、API サーババージョンを確認できない場合は skip する。
- Apple の `run`/`create` には image 解決を止める `--pull=never` 相当がないため、厳密な `PullNever` は保証できない。backend は CLI 呼び出し前に `ErrPullNeverUnsupported` で拒否し、`PullMissing`/`PullAlways` を明示的な fallback とする。厳密な no-fetch は Docker だけが保証する。

## 実現方式の選定

実現方式の候補は二つである。

- **CLI ラッパー方式**：`os/exec` で `container` CLI を子プロセスとして起動し、JSON 出力を解釈する。
- **XPC 直結方式**：container-rs が採る方式で、`container-apiserver` の XPC サービスを C ブリッジ経由で直接呼ぶ。

本ライブラリは CLI ラッパー方式を採用する。
理由は次の三点である。

第一に、最小依存要件との整合である。
XPC 直結方式は cgo と自前の C ブリッジを必要とし、ビルドに macOS SDK が絡む。
CLI ラッパー方式は、プラットフォームの process 操作に使う `golang.org/x/sys` を除いて純 Go・標準ライブラリで完結し、`CGO_ENABLED=0` でもビルドできる。

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

コンテナを新規作成する必要がある場合、`Run` は選択した pull policy に従ってイメージを解決・適用してから待機戦略を完了する。
既定の `PullMissing` はローカルイメージストアを検査し、イメージがないときだけ明示的に pull する。
`PullAlways` は新規コンテナ作成の試行ごとに明示的な pull を要求する。`PullNever` は Docker では strict で、`--pull=never` を渡し、image がなければ `ErrImageNotFound` を返す。Apple Container には同等の run-time switch がないため、`Run` は `ErrPullNeverUnsupported` でこの policy を拒否する。起動後の操作や待機が再利用でない経路で失敗した場合は、作成したコンテナをロールバックしてからエラーを返す。Reuse の共有ライフタイムは後述の別契約で扱う。

オプションは functional options で提供する。
以下は現在の開発チェックアウトの API 一覧である。
ここで列挙した基本 option は `v0.2.0` にもあった。
後述する `LogsOptions` / `LogsWithOptions`、追加 HTTP setter、
合成 wait の setter は `v0.2.0` にはなかった。

- `WithExposedPorts(ports ...string)`：接続対象のコンテナポート（`"6379/tcp"` 形式）を宣言する。`MappedPort` と `Endpoint` はこの宣言を解決に使える。Docker は宣言したポートを自動公開し、Apple Container は既定でコンテナ IP から解決する。Apple ではこれは handle 側の endpoint 宣言であり、backend に別の `--expose` flag を渡さない。
- `WithEnv(env map[string]string)`：環境変数を設定する。値はコマンドライン引数ではなく一時ファイル経由で渡す。
- `WithCmd(cmd ...string)` / `WithEntrypoint(entrypoint string)`：コマンドとエントリポイントの上書き。エントリポイントは `docker run --entrypoint` の仕様どおり 1 トークンとする。複数トークンは `WithCmd` を使う。
- `WithWaitStrategy(s wait.Strategy)`：起動完了の判定方法を指定する。
- `WithName(name string)`：コンテナ名を指定する（省略時は `containergo-<ランダムな 16 進数>`）。共有の 1～63 文字 guard を使う。Apple のより厳密な 2～63 文字 rule は Apple バックエンドの preflight が image 解決前に強制する。
- `WithLabels(labels map[string]string)`：追加ラベルを指定する。ライブラリが管理するラベルは予約済みである。
- `WithMounts(mounts ...Mount)`：bind、名前付きボリューム、tmpfs のマウントを指定する。bind source は host OS の path 構文を使うため、ローカル Docker daemon では Windows の host path も受け付ける。remote Docker daemon への bind mount は、daemon が source path を自分の host で解決するため `ErrUnsupportedCapability` で拒否する。
- `WithFiles(files ...File)`：起動後のコンテナへ regular file をコピーする。各 `File` は `HostPath` と `Content` のどちらか一方だけを設定し、`Mode` で非公開ステージング上の Unix permission bits を上書きできる。option 評価時に host directory、symlink、特殊ファイルを拒否する。コピーに失敗すると `Run` をロールバックする。`WithReuse` では attach caller を含むすべての caller でコピーされ、attach 時の失敗は共有コンテナを削除せずエラーを返す。
- `WithPublishedPort(spec string)`：ホスト側ポートの明示的な公開を指定する。Apple Container は通常コンテナへ直接接続し、Docker は `WithExposedPorts` で宣言したポートを daemon が host port へ自動公開する。特定の host port が必要な場合だけ明示的な binding を使う。
- `WithPullPolicy(policy PullPolicy)`：`PullMissing`（既定）、`PullAlways`、`PullNever`（Docker のみ。Apple は `ErrPullNeverUnsupported`）を選ぶ。`WithReuse` の attach 前にも `PullAlways` は image を fetch する。
- `WithReuse()`：名前付き `Run` を get-or-create にする。attach caller にも `WithFiles` と `PullAlways` が適用される。CPU、shm、capability、ulimit、extra host、hostname の option は reuse の互換性 identity に含まれ、既存コンテナと一致しなければならない。capability は requested-subset rule で、要求した `--cap-add`/`--cap-drop` のすべてが存在する必要があり、空の要求は inspect された任意の capability set を受け入れる。それ以外の作成専用 option は attach 時に無視される。
- `WithReuseGroup(group string)`：再利用するコンテナにラベルを付け、`PruneReuseGroup` の対象にする。`WithReuse` が必要で、再利用キーには含まれない。`PruneReuseGroup` は `WithReuseGroup` と同じ group grammar を使う。
- `WithCPUs(v float64)` / `WithMemory(size string)`：リソース制限を指定する。`WithCPUs` は `0.5` のような小数を受け付け、zero 以下と非有限値を拒否する。Apple Container は整数のみを受け付けるため、小数は `ErrUnsupportedCapability` で拒否する。`WithMemory` と `WithShmSize` は整数と任意の `K`/`M`/`G`/`T`/`P` suffix の同じ文法を使い、zero と overflow を拒否する。
- `WithShmSize(size string)`：`/dev/shm` のサイズ（例: `64M`）を指定する。Docker と Apple Container はどちらも `--shm-size` として渡す。
- `WithPrivileged()`：Docker の `--privileged`。Apple Container に相当する capability はなく、`ErrUnsupportedCapability` で拒否する。
- `WithCapAdd(caps ...string)` / `WithCapDrop(caps ...string)`：Linux capability を追加・削除する。名前は optional な `CAP_` prefix 付きの大文字（または `ALL`）で、重複は prefix に関係なく無視し、caller の表記をそのまま渡す。両 backend が `--cap-add` / `--cap-drop` をどちらの形式でも受け付ける。
- `WithUlimit(name string, soft, hard int64)`：Docker の `name=soft:hard` 形式で resource limit を指定する。名前は Docker の小文字集合（`nofile`、`memlock`、`nproc` など）。`-1` は無制限を表す。`-1` 未満、有限の hard を超える soft、同名の重複を拒否する。
- `WithExtraHost(host, ip string)`：Docker の `--add-host host:ip`。IP には `host-gateway` を指定できる。同名または衝突する entry は拒否する。Apple Container に extra host の capability はなく、`ErrUnsupportedCapability` で拒否する。
- `WithHostname(name string)`：Docker の `--hostname`。RFC 1123 の hostname grammar と 63 byte の UTS nodename 上限で検証する。Apple Container に `--hostname` flag はなく、`ErrUnsupportedCapability` で拒否する。
- `WithUser(u string)` / `WithWorkingDir(dir string)`：実行ユーザーと作業ディレクトリを指定する。
- `WithNetwork(name string)`：既存の名前付きネットワークへ接続する。省略時は Docker が daemon の既定 network（Linux では `bridge`、native Windows では `nat`）を使う。
- `WithNetworkAliases(aliases ...string)`：Docker の `--network-alias` として network-scoped DNS alias を追加する。明示的な user-defined `WithNetwork` が必要で、default bridge（`bridge`/`nat`）、`host`、`none` は `*ConfigError`、Apple は `ErrUnsupportedCapability` で拒否する。alias は DNS label として検証し、reuse の互換性比較にも含める。
- `WithPlatform(p string)`：`linux/amd64` のようなイメージプラットフォームを指定する（Apple Container では Rosetta を含む）。


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
func (c *Container) ExecTo(ctx context.Context, cmd []string, output io.Writer, opts ...ExecOption) (int, OutputStats, error)
func (c *Container) Logs(ctx context.Context) (io.ReadCloser, error)
func (c *Container) LogsWithOptions(ctx context.Context, opts LogsOptions) (io.ReadCloser, error)
func (c *Container) LogsTo(ctx context.Context, output io.Writer, opts LogsOptions) (OutputStats, error)
func (c *Container) FollowLogs(ctx context.Context) (io.ReadCloser, error)
func (c *Container) CopyToContainer(ctx context.Context, hostPath, containerPath string) error
func (c *Container) CopyContentToContainer(ctx context.Context, content []byte, containerPath string, mode fs.FileMode) error
func (c *Container) CopyFileFromContainer(ctx context.Context, containerPath string) (io.ReadCloser, error)
func (c *Container) Stop(ctx context.Context, timeout *time.Duration) error
func (c *Container) Terminate(ctx context.Context) error
```

`Stop` は duration を受け付ける。非 nil の timeout は非負でなければならず、negative の場合は CLI 呼び出し前に拒否される。両 backend は whole seconds を受け付けるため、正の sub-second timeout は切り上げ、0 は即時終了を要求し、丸めた秒数が backend limit（確認した両 CLI では `math.MaxInt32` 秒）を超える値は拒否される。timeout は操作の query budget にも加算される。

`Exec` は有限時間かつバッファリングされる操作で、終了コードと stdout+stderr を返す（非ゼロ終了は結果であり error ではない）。caller の context に deadline がない場合は 30 秒の既定 deadline を適用する。正の `WithExecTimeout(d)` を指定した場合、effective deadline は `d` と caller の deadline の早い方です。`WithExecTimeout(0)` は library の既定 deadline のみを無効化し、caller の deadline は取り除かない。長寿命のプロセスは通常 `WithCmd` で本体コマンドとして起動し、出力は `FollowLogs` で取得する（Long-open な Exec は避ける）。
backend、timeout、cancellation error の場合も、failure 前に生成された partial stdout+stderr を reader に保持し、分類済み error とともに返す。CLI が返した終了コードが利用できる場合は、最初の戻り値にも保持する。command の起動中実際に context error が競合した場合だけ、その分類は終了コードに依存しません。Windows の `Process.Kill` が終了コード `1` を返す場合でも、その status を保持したまま型付き error を返します。後から inspect が deadline を使い切っただけでは `ExecTerminationError` にはなりません。
context 取消時は stable process identity を持つ Unix target でのみ、直接の command lifecycle が所有する間だけ local CLI process group を停止します。Linux では `waitid(WSTOPPED|WNOWAIT)` で停止状態を確認できない場合、pidfd へ直接 signal を送り、古い PGID には signal を送りません。group signal の後は直接の子にも kill を送ります。この identity を持たない Unix target は直接の子 handle だけを使います。直接の子を回収した後は古い process group ID に signal を送りません。
Windows では lifecycle が `OpenProcess` handle を保持し、`TerminateProcess` で直接の子だけを終了します。detach された descendant や親を付け替えられた descendant は保証外です。終了済み child への kill 成功は active process の証拠として扱いません。
Windows 固有の lifecycle test は build constraint 付きで、exit-259 の active-child test も含め `windows-latest` の unit job で実行します。
backend CLI には exec instance を kill する共通操作がないため、active-child の証拠が利用できる場合に `*ExecTerminationError`（`errors.Is(err, ErrExecTerminationUnsupported)`）を返します。保守的な direct-handle fallback では remote process の終了を主張せず context error だけを返すことがあります。process が残り得るため、caller は container を terminate するか backend 固有 cleanup を実行します。したがって error が non-nil でも reader を読む必要があります。Linux の stable identity は pidfd が process を参照する。pidfd 非対応 kernel では、他の Unix target と同じく保守的な direct-handle fallback を使う。

`WithExecEnv`、`WithExecUser`、`WithExecWorkDir` は個別の exec 呼び出しを設定する。環境変数の値は `WithEnv` と同じ一時ファイル方式を使う。
`Logs` と、成功する `LogsWithOptions` 呼び出しは backend command が終了するため有限のスナップショットを返すが、単体で byte 上限は課さない。`Logs` は全出力を要求する。`LogsOptions` は backend 固有で、Docker は `Tail` と `Since` の両方を受け付ける。確認した Apple Container CLI の各 version は tail に `-n` を使い、`--since` option を持たないため、Apple backend は `Tail` を `-n` に対応させ、`Since` を `ErrUnsupportedCapability` で拒否する。範囲を指定しても大きさはコンテナ出力に依存する。`FollowLogs` は意図的に上限のない stream で、リーダーを閉じるか context をキャンセルするまで続く。`LogsOptions` と `LogsWithOptions` は `v0.2.0` にはない現在の開発 API である。`Terminate` は世代ガードを持ち、Apple の名前ベース削除は有効な creation generation を、Docker の削除は immutable UID を要求する。reuse と failed-create の経路も同じ ownership 情報を削除前に要求する。

`LogsOptions.Tail` と `MaxBytes` は非負として検証され、`WithMemory` と `WithShmSize` は zero と overflow を拒否し、`WithMounts` は unknown な `MountType` を拒否する。`PruneReuseGroup` は `WithReuseGroup` と同じ group grammar を使う。

`Terminate` は Apple Container では `container delete --force`、Docker では `docker rm --force` に対応する。要求した target を含まない inspect 結果も含め、backend が認識した not-found CLI failure を成功として扱うため、冪等性の主張は not-found と delete 成功を対象にする。malformed な inspect response は沈黙した success ではなく error になり、unrelated な delete failure はそのまま返る。`Cleanup(t, ctr)` と `TerminateContainer(ctx, ctr)` は nil 安全なヘルパーであり、`Run` のエラーを確認する前にクリーンアップを登録できる。`TerminateContainer` は caller の context を使い、`Cleanup` と `StrictCleanup` は `terminateTimeout`（既定 30 秒）の context を渡して teardown の予算を維持する。

`CopyFileFromContainer` は安全な host file-open semantics を持つ Docker バックエンド向けの copy-out API である。materialize された結果が regular file であることを検証するが、すべての Docker host がすべての container file type を表現できると主張するものではない。Apple Container の CLI は host が結果を開く前にすべての source file type を保持・拒否できないため、`container cp` を起動せず `ErrCopyFileFromContainerUnsupported` を返す。Docker の copy-out は client と server の両方が 29.7.0 以上である必要がある。メソッドは private な一時ディレクトリを作成する前、および `docker cp` を実行する前に両方のバージョンを確認し、確認できない場合は同じ error で fail closed する。no-follow/nonblocking な file open がない host も同じ error で fail closed する。Windows では Go 1.23 から 1.25 が `os.OpenFile` に必要な Windows file flag を伝播しないため、Docker の copy-out には Go 1.26 以降が必要である。`CopyToContainer` は両バックエンドで使用できる。

`CopyToContainer` は通常ファイルとディレクトリを受け付ける。
各 source entry は、最終要素のリンクを追わずに一度だけ開く。
ディレクトリ内の child は、既に開いた親ハンドルからの相対 open で取得する。
そのハンドルを、backend CLI の実行前に現在のユーザー専用の staging root へコピーする。
`WithFiles` も同じ経路を使うが、option 評価時に通常ファイル以外
（ディレクトリ、シンボリックリンク、特殊ファイル）を `ValidationError` で拒否する。
staging と CLI には独立した deadline を設定し、caller の deadline は両方に優先する。
ディレクトリは file bytes、entry 数、深さ、訪問したパス metadata の合計で制限する。
スナップショットが保持するのは名前、バイト列、Unix permission bits だけで、timestamp、owner、setuid/setgid、ACL、extended attributes、alternate data stream、sparse layout、hardlink identity は保持しない。

`File.Content` と `CopyContentToContainer` は、メモリ上のバイト列を 1 つの regular file として同じ現在ユーザー専用 staging root へ書き込む。
ステージングファイルは `0600` で作成し、`File.Mode`（または `mode` 引数）が非 0 ならその値へ変更する。
0 は host file の permission を保持し、Content では `0600` を既定値にする。
permission bits 以外の bit は拒否するため、0 で mode `0000` を要求することはできない。
Content は `MaxCopyToContainerSize` の上限を共有し、container path から導出しない固定のステージング名を使う。
Content は argv、error、診断に含めない。
staging は成功、失敗、キャンセルのいずれでも削除し、backend copy 中の失敗も同様である。

### 出力の契約

CLI の出力には、全量を保持する互換モードと、caller の sink へ直接流す bounded/streaming モードがある。`Exec` で `WithExecMaxBytes` を指定しない場合、`Logs`、`LogsOptions.MaxBytes` が 0 の場合は、既存呼び出しとの互換性のため全量を保持する。この経路は信頼できない出力に対しては deprecated とし、新コードでは正の `WithExecMaxBytes` / `LogsOptions.MaxBytes` または `ExecTo` / `LogsTo` を使う。

`LogsOptions.Tail` は backend の行単位の末尾(Docker は `--tail`、Apple は `-n`)で、`Since` は backend の timestamp filter である。`MaxBytes` はその後に適用される**先頭 byte の prefix 上限**であり、byte 単位の tail ではない。stdout/stderr の結合到着ストリームの先頭を保持する。同じ prefix ポリシーを `WithExecMaxBytes` と `LogsTo` にも適用する。wait 失敗時のログ診断だけは、直近の診断を残すため意図的に異なり、backend の bounded log tail(Docker は `--tail`、Apple は `-n`)の末尾 1MiB を保持する。旧 unbounded reader は stdout の後に stderr を連結する。bounded/streaming capture は観測した到着順を使う。

正の制限値は stdout と stderr の合計に適用する。保持上限に達した後も CLI 子プロセスは排水され続けるため、pipe が詰まって停止しない。bounded reader は `Truncated() bool` を提供し、保持したデータを返した後の terminal read error として `ErrOutputTruncated` を返す。`OutputStats` は破棄されたデータを含む観測バイト数と truncation を表す。`FollowLogs` は長期ストリームであり、byte 上限を意図的に設けない。sink の書き込みエラーは成功結果に変換せず、`RunTo`、`ExecTo`、`LogsTo` は CLI の終端エラーを保持する。bounded snapshot API は infrastructure、cancellation、launch、missing-container エラー時も partial reader と truncation state を返す。

wait adapter は `ForExec` の出力を `ExecTo` で `io.Discard` へ送り、失敗診断のログだけを backend の bounded tail の末尾 1MiB として redact して保持する。CLI runner は `RunTo` で直接 streaming を提供する。旧 `Run` しか実装しない runner は互換 adapter を使うが、`Run` が戻る前に大きな slice を作るため peak memory の上限は保証できない。大きな出力の integration test は bounded forwarding と draining を確認するだけで、peak host memory を測定するものではない。RSS の測定には外部 sampler が必要である。

`LogsOptions` に `Tail` と `Since` に加えて `MaxBytes` を追加した。Go の構造体にはフィールドを追加しても旧 unkeyed literal を維持する仕組みがないため、この pre-1.0 リリースでは source break を明示的に受け入れる。呼び出しは keyed `LogsOptions` literal へ移行する。微妙に異なる二重の options 表現を維持するより、この選択の方が明確である。internal CLI diagnostic はさらに別で、各失敗 invocation の `CLIError` には stderr の末尾最大 64KiB だけを保持し、`Run` の全量出力と caller-owned `RunTo` sink の動作は変えない。

## 接続エンドポイントの設計

testcontainers の Docker 実装ではコンテナポートをホストのランダムポートへ公開し、`localhost:<mapped>` へ接続する。
Apple Container ではこの方式を既定にしない。

Apple Container では、`Host` は inspect の最初の `status.networks[].ipv4Address` から CIDR サフィックスを除いたコンテナの実 IP を返し、`MappedPort` はコンテナポートをそのまま返す。Docker は代わりに公開 host endpoint を既定にする。local daemon は loopback に publish し、検出された remote daemon は remote-safe な binding を使う。Apple の direct-IP を既定とする理由は三つある。

- Apple Container にはランダムポート割り当てがない。 ホストポートを自前で確保すると「空きポートを探してから起動する」までの競合が生じる（container-rs も同じ制約を持つ）。直接 IP 接続なら ホストポートを消費しないため、この競合が起きない。
- ホストポート衝突がないので、テストの並列実行を無制限に拡張できる。
- ポート転送プロキシを経由しないため、転送実装の不具合（大きな転送が途中で切れる事例が報告されている）の影響を受けない。

ローカルの Docker daemon でクライアントが `localhost` を要求する場合、またはコンテナ IP に到達できない構成では、`WithPublishedPort("127.0.0.1:15432:5432")` のように明示的に公開する。この loopback example は local-only であり、non-loopback の `DOCKER_HOST` では loopback binding を reject する。検出した non-loopback `DOCKER_HOST` デーモンでは remote-safe な binding と endpoint を使う。公開すると `Host` は指定したホストアドレスを、`MappedPort` はホストポートを返す。`Host`、`Endpoint`、`MappedPort`、`ContainerIP` の解決は、同一性を検証した動的 inspect を短い（約 1 秒）TTL で `Shared()` handle 間でも共有し、readiness の 100ms ごとの retry で毎回 inspect しないようにする一方、stale なネットワークとポートデータは無期限には保持しない。利用可能な IP（direct-IP）や host binding（published-port）が無い不完全な結果は cache hit にしない。`State` は常に再 inspect し、lifecycle の fail-fast を遅らせない。

`MappedPort` と `Endpoint` は、`WithExposedPorts` で宣言したポートと明示的に公開したポート、または利用できる host binding がないポートを `ErrPortNotExposed` にする。
宣言は wait strategy にも使われる。`ForExposedPort` は最初の exposed TCP port を使い、TCP の exposed 宣言がなければ最初の published TCP port を選ぶ。`Target.Endpoint` の空 port も同じ TCP 宣言を選ぶ。

不変な同一性情報（UID、イメージ、ラベル）は検証後にキャッシュし、動的なネットワーク、IP、ポート束縛は上記の短い TTL で endpoint 読み取りに使い、`State` は呼び出しのたびに再 inspect する。Docker が
複数 network を報告し top-level address がない場合、network 名の辞書順で最初に見つかる
空でない IP address を使う。
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
- `wait.ForListeningPort(port)`：解決したエンドポイントへの TCP 接続と、対応する target ではコンテナ側の LISTEN 状態を確認する。`WithHostOnly` でコンテナ側の確認を省略できる。
- `wait.ForExposedPort()`：最初の exposed TCP port を使い、TCP の exposed 宣言がなければ最初の published TCP port を選ぶ。
- `wait.ForHTTP(path)`：解決したエンドポイントへ HTTP リクエストを送り、ステータスが条件（既定は 2xx、`WithStatusCodeMatcher` で変更可）を満たすまで待つ。`WithPort` と `WithMethod` で対象を指定する。現在の開発チェックアウトは `WithHeaders`、`WithHeader`、`WithBasicAuth`、`WithTLS`、`WithTLSConfig`、`WithHTTPClient` も追加している。これらは `v0.2.0` にはなかった。受理したレスポンスボディは `WithResponseMatcher` で最大 64KiB まで判定でき、false なら再試行する。
- `wait.ForHealthcheck()`：Docker の `HEALTHCHECK` が healthy になるまで待つ。`starting` の間は poll を続け、`unhealthy` は終端エラーになる。`HEALTHCHECK` を持たないイメージは `*wait.ConfigError` で即座に失敗し、Apple バックエンドはコンテナ作成前にこの戦略を拒否する。
- `wait.ForExec(cmd)`：バックエンド CLI でコマンドを起動し、終了コード（既定は 0）が条件を満たすまで待つ。各試行の timeout は poll interval から 1〜5 秒の範囲で導出し、`WithExecTimeout` で上書きできる。
- `wait.ForAll(strategies...)` / `wait.ForAny(strategies...)`：戦略を合成する。子戦略は各自の設定を保持する。現在の開発チェックアウトでは公開型 `AllStrategy` と `AnyStrategy` が `WithStartupTimeout` で合成全体を制限できるが、`WithPollInterval` は公開しない。`v0.2.0` の関数は、合成全体の timeout setter を持たない interface を返す。合成 timeout が 0 または負の場合は合成全体を無制限とし、各子の timeout を適用する。

すべての葉戦略は `WithStartupTimeout`（0 は 60 秒）と `WithPollInterval`（0 は 100 ミリ秒、`ForExec` は 250 ミリ秒）を持つ。`ForLog` の poll interval は、パターンが見つかる前にストリームが終了してから再-open するまでの待ち時間である。コンテナが stopping、stopped、paused の状態のいずれかになると、残りのタイムアウトを待たずに失敗する。created、restarting、unknown と一時的な inspect エラーは再試行し、一時的なログストリームの open と EOF は再-open する。終了コードを伴うログストリームエラーは返却する。成功マーカーは、制限時間内の最終ライフサイクル観測で `Running` が確認された場合のみ受理される。`ForLog` はワンショットジョブではなく長時間稼働するサービス向けである。`ForLog` は、再-open 時に再生されるログの共通部分を除外してから出現回数を累計する。再利用でない `Run` の待機が失敗した場合はコンテナをロールバックし、上限付き log の取得が成功した場合だけ 1MiB 上限の末尾をエラーに付ける。再利用の待機失敗では共有コンテナを残す。

`ForListeningPort` と `ForExposedPort` は TCP 専用 probe です。
`ForListeningPort` は `/udp` と不正な specification を probe 前に `*wait.ConfigError`
で拒否します。`ForExposedPort` は `WithExposedPorts` で最初に宣言した TCP port を選び、
TCP の exposed 宣言がなければ最初の TCP published port にフォールバックします。
UDP だけの宣言が TCP probe に使われることはありません。

| API | TCP | UDP |
|---|---|---|
| `WithExposedPorts` / `WithPublishedPort` | 受理 | endpoint / publish 設定では受理 |
| `wait.ForListeningPort` | `PORT` または `PORT/tcp` | probe 前に `*wait.ConfigError` |
| `wait.ForExposedPort` | 最初の TCP exposed port、なければ最初の TCP published port | skip。TCP port がなければ `ErrPortNotExposed` |

strategy の契約は次のとおりです。

```text
type Strategy interface {
    WaitUntilReady(ctx context.Context, target Target) error
}
```

`Target` は `Endpoint`、`Running`、`FollowLogs`、`ExecCommand` を公開する小さなインターフェースである。
接続 strategy は `ContainerIP` ではなく解決済み endpoint を使い、Apple Container の direct IP と Docker の published port に対応する。
`Target` は従来の `Running` メソッドをカスタム戦略との互換性のために維持する。
起動中の一時状態を区別できるターゲットは、任意の `StateTarget` インターフェースも実装する。
組み込み戦略は `State` を優先し、互換性のため `Running` にフォールバックする。
port strategy は任意の `PortListenTarget` を使ってコンテナ側の LISTEN 状態を確認し、未対応の target では host 側の接続だけで判定する。
Docker のヘルスチェック待機では、任意の `HealthTarget` インターフェースも実装する。
`ForHealthcheck` はこれを実装しないターゲットに `*wait.ConfigError` を返す。
`container.Run` が `*container.Container` をこれらのインターフェースへ適合させる。
依存方向は `container` から `wait` だけであり、逆方向を作らないため循環参照を避ける。

## クリーンアップ

ライブラリには通常の終了、作成後の rollback、create 失敗、異常終了の
別経路がある。
各経路でエラーの見え方は異なる。

**通常の cleanup**：`Cleanup(t, ctr)` が `t.Cleanup` 経由で `Terminate` を
登録する。`TerminateContainer(ctx, ctr)` は nil 安全な defer 形式である。
caller の context を使い、`Cleanup` と `StrictCleanup` は完全な teardown を
`terminateTimeout`（既定 30 秒）で制限する。
`Cleanup` の終了エラーはテストの log に残り、`TerminateContainer` は
エラーを呼び出し元へ返す。

`Container.Kill(ctx, signal string)` は signal を 1 つだけ取る。空文字列は
既定の SIGKILL で、空でない signal は backend へそのまま渡す。そのため
不正な綴りは可変長引数の検査ではなく backend が拒否する。

**作成後の rollback**：再利用でない `Run` が file copy または readiness 待ちの
途中で失敗すると、作成した handle の `Terminate` を呼ぶ。削除にも失敗した場合、
返り値には元の失敗とコンテナが残ったことを示すメッセージを含める。wait の
失敗では、上限付き log の取得が成功した場合だけ log 末尾も付ける。

**create 失敗**：backend の `run` command 自体が失敗した場合、
`cleanupFailedCreate` が別の best-effort 経路で処理する。name conflict はスキップし、
この process の managed / session label を持つコンテナだけを inspect する。
creation label は存在し、この run と一致しなければならず、label がない場合や
一致しない場合は削除しない。これは replacement を防ぐ best-effort の guard で
あり、トランザクション保証ではない。lock、inspect、delete の error は `Run` の
返り値に連結されない。

**異常終了**：親 process が終了して reaper pipe が閉じられると、外部
`/bin/sh` の watchdog が強制削除を試みる。process を終了させる未 recover の
panic、`SIGKILL`、`os.Exit` では pipe が閉じる。recover した panic では閉じない。
reaper は親が生きている間は登録を蓄積し、EOF のあとだけ削除する。reaper は
保険であり、トランザクション保証ではなく、Windows では利用できない。登録
error と各 entry の delete error は `Run` に返らない。spawn failure が retry
上限に達した場合は一度だけ log に残り、delete failure は shell が無視する。
各 backend 呼び出しには POSIX の `sleep` / `kill` による 10–30 秒の期限があり、
timer process も回収する。reap 全体にも overall budget があり、それを超えると
新規の backend 呼び出しは発行しない。worker pool は並列度を制限し、
いずれかの worker が終了すると次の entry を開始する。budget が切れて
未開始の entry が残る場合、container ID を出さず残件数を stderr に報告する。

**残骸の回収**：watchdog は Windows では使えず、CI のキャンセルや
`go test -timeout` などでローカルの process tree や runner VM が破棄された
後、または remote Docker daemon 上でローカル環境より長く残るコンテナには
動作できません。対応策は `TestMain` の前後で `Prune(ctx)` を呼ぶことです。
`Prune` は session に関係なく停止済みの managed コンテナを削除し、running と
created は残します。`WithPruneOlderThan(d)` は backend が報告する作成時刻が
now-d より前の candidate だけに絞り、作成時刻が不明な candidate も残します。
これにより共有 daemon や remote daemon で他の job が最近停止したコンテナを
保護できます。これは watchdog の代替ではなく、回収のための運用パターンです。

reaper は実 CLI コンテナを非 reuse 経路で登録したときに遅延起動する。
作成するコンテナは backend create の開始前に名前と creation generation で
pre-register される。create が成功すると、ライブラリは managed / session /
creation label を検証してから Docker の完全な 64 桁 hex immutable ID を追加の
entry として登録する。名前で address する entry（Apple は常に、Docker は
`run` が ID を返すまで）は、通常の generation-checked 削除と同じ名前単位
`flock` の下で inspect・削除され、reaper は削除前に creation label を検査する。
外部 CLI による delete / recreate はこの lock を取らず、保証外である。
Docker ID entry は name lookup なしで ID により削除される。backend が解析可能な
ID を返さない場合は、name と generation の経路が削除対象として残る。

現在の reaper は inspect output を構造化フィルタへ流し、raw JSON を disk へ
書きません。旧バージョンは namespace のない `mktemp` file を使っていたため、
inspect 中に reaper が kill されると環境 data を含む file が残る可能性が
あります。旧バージョンの file を扱う場合は、container-go と reaper の
process をすべて停止し、実効のユーザーごと `TMPDIR` でメタデータだけを列挙し、
そのユーザーと該当時間帯の通常ファイルに範囲を絞ります。ファイル内容を表示・
grep したり、シンボリックリンクを追跡したり、broad recursive delete を実行
しないでください。該当実行に確実に帰属するファイルだけを削除し、inspect output
に含まれた可能性がある認証情報はローテーションしてください。

**セッションラベル**：作成するコンテナには次のラベルを付ける。

- `com.github.hirokazumiyaji.container-go`：`true`（管理対象の印）
- `com.github.hirokazumiyaji.container-go.session`：プロセスごとのランダム ID
- 作成世代ラベルと、Reuse 時の再利用グループラベル

Apple CLI にはラベルフィルタがないため、孤児の掃除は `container ls -a --format json` をクライアント側で絞り込んで行う。`Prune(ctx)` は現在の backend の managed filter が選ぶコンテナを削除する。Apple は stopped 状態を選ぶ。Docker は exited と dead 状態を選ぶ。running と created 状態は選ばない。Apple では、各 candidate は削除前に安定した名前単位 lock の下で再 inspect され、世代、session、管理対象ラベル、および停止状態が再確認される。

package レベルの `Prune` は backend 中立の `PruneOption` だけを受け付け、Docker target の option を持たない。特定の Docker daemon や context を対象にする場合は、`WithClientDockerHost` または `WithClientDockerContext` で `Client` を作り、`Client.Prune` を使う。`Pull` は pull 固有の target option (`WithPullDockerHost`、`WithPullDockerContext`) を維持するが、`Client.Pull` は Client が target を固定するためこれらの上書きを拒否する。

`CONTAINERGO_KEEP=1` はプロセス全体の診断用フラグである。`Cleanup` と
`TerminateContainer` による自動削除、リーパー登録を省略し、作成失敗、
`WithFiles` コピー失敗、wait strategy 失敗時の自動 rollback を抑制して、
調査のためにコンテナを残す。明示的な `Container.Terminate` は抑制
されず、`Prune` や `PruneReuseGroup` は引き続きコンテナを削除できる。
`WithReuse` の停止済みコンテナ置換も変わらないため、条件に一致した stopped
reuse container は削除・再作成される。この変数をグローバルな削除ロック
として扱わない。

匿名ボリュームは `--rm` でも残るので、ライブラリは匿名ボリュームを作らない。ボリュームを使う場合は名前付きで、そのライフサイクルは呼び出し元に委ねる。bind source は host OS の path 構文を使うため、ローカル Docker daemon では Windows の host path も受け付ける。remote Docker daemon への bind mount は、daemon が source path を自分の host で解決するため `ErrUnsupportedCapability` で拒否する。

## Reuse

`WithReuse` は安定した `WithName` に対する `Run` を get-or-create にする。共有は同じ host の process 間で行う。既存コンテナは managed marker、`WithReuse` marker、有効な creation generation、および Docker では有効な immutable UID を持つ必要があり、各呼び出しは自分の wait strategy を再実行する。停止済みコンテナはこれらの ownership check と image check に合格した場合だけ削除・再作成の対象になる。image 互換性は元の mutable tag ではなく解決済み digest / image ID で判定し、Docker では要求した network identity も一致する必要がある。reuse は互換性 check の前と readiness 後、handle を返す直前に再 inspect する。Docker は immutable UID、Apple は creation generation を再確認するため、wait 中の置き換えは adopt せず error になる。port 互換性は backend ごとに異なる。Docker は `WithExposedPorts` の自動公開 binding と明示的な `WithPublishedPort` binding を比較する。Apple は明示的な published binding を比較するが、library の Apple inspect model に `WithExposedPorts` の宣言が残らないため、その宣言は比較できない。Apple の各呼び出しは自分の exposed-port 宣言を共有コンテナ IP へ使う。宣言を分離したい場合は名前を変える。`WithFiles` は attach caller を含むすべての caller でコピーされ、attach 時のコピー失敗は共有コンテナを削除せずエラーを返す。`PullAlways` は attach 前にも各 caller で実行される。`env`、`cmd`、`mounts` の差は既存コンテナへ黙って attach する。分離が必要なら異なる名前を使うか、`Exec` で状態を初期化する。`CONTAINERGO_KEEP=1` はこの reuse rule を変えない。条件に一致した stopped reuse container は削除・再作成され、`PruneReuseGroup` はその group を削除できる。

Apple では delete 前に fresh inspect を行い、inspect と delete を現在ユーザー専用の永続ディレクトリ内で、コンテナ名のハッシュを使う名前単位 `flock` で直列化する。同じ host で本ライブラリの generation-checked `Terminate` と failed-create cleanup、Apple の `Prune` と `PruneReuseGroup`、および名前で address する reaper entry は名前単位 `flock` で保護される。外部ツールによる同じ窓の delete / recreate は保証外である。Docker の handle は `docker run` が出力した、または inspect で検証した完全な不変 `Id` で削除するため、同じ名前の置き換えとは delete target の ID が一致しない。Docker のすべての backend operation はその immutable ID を使い、inspect parser は要求した ID や name に一致しない object を拒否する。これらは別の保護であり、reuse は backend CLI を直接使う信頼できない process に対する一般的な fail-closed 保証ではない。

pull flight がまとめるのは *並行* 呼び出しだけである。完了するとエントリは
削除されるため、前の `Run` が終わったあとの `Run` は再び image を inspect
する。これは安全側の既定であり、2 つの `Run` のあいだにストアから image が
消えることがある。そのため `Run` は毎回存在確認の daemon round trip を払い、
ストアが実行中安定している呼び出し側は `WithImagePresenceCache(ttl)` でそれを
省ける。返された Option がキャッシュを所有するので、呼び出し側はその
Option を `Run` 間で再利用する必要がある。新しい呼び出しは空のキャッシュを
作る。キャッシュするのは "present" の答えだけである。不在をキャッシュすると、
それが引き起こした pull で無効化する必要が出る。キーは backend、image store
（Docker では client-config env）、image、platform であり、これらのストアは
独立している。Docker の `PullNever` は常に inspect する。契約は image 不在で失敗する
ことなので、キャッシュされた答えをその確認の代わりにしてはならない。

## セキュリティ設計

外部プロセスを起動するライブラリとして、次の原則は security boundary を表す。現在の reaper は inspect output を構造化フィルタへ流し、raw JSON を staging しない。旧バージョンの staging file は後述の cleanup 手順の対象であり、それらの環境では end-to-end の no-leak 保証にはならない。

**シェルを経由しない**。
すべての CLI 呼び出しは `exec.Command` に引数配列を渡す形で行い、シェル文字列を組み立てない。
唯一の例外は watchdog リーパーのシェルスクリプトである。
ここはスクリプト本文を固定文字列とし、コンテナ ID は標準入力からデータとして渡す。
スクリプト側は `set -f`(グロブ無効)、`IFS=` と `read -r`、変数のクォートで語分割とグロブ展開を封じる。
リーパーは name-addressed entry を共有の generic `nameRE`
`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$` で検証し、Docker の 64 桁 16 進数 ID も受け付ける。
この shared guard は Apple の契約全体ではない。`appleEngine.checkConfig` がコンテナ作成前に Apple 固有の 2〜63 文字の `appleContainerNameRE` を適用する。
二重の防御により、ID 経由のコマンド注入を成立させない。

**環境変数を argv に載せない**。
`--env key=value` を使うと、値がプロセス一覧(`ps`)から他ユーザーにも見える。
データベースのパスワードなど秘密情報を渡せるよう、Unix では正規化済みの `os.UserCacheDir()` 配下に、所有者が現在のユーザーでモードが 0700 のディレクトリを作り、0600 の env ファイルを書く。
既存の symlink 祖先は一度だけ解決し、`..` と書き込み可能な信頼できない祖先は拒否する。
`TMPDIR` は信頼境界に置かない。
24 時間経過した staging ディレクトリだけを age で回収し、marker 作成後に lock を作る途中で終了した状態や tombstone 化した部分削除は自動修復する。
初期化済みディレクトリは age ではなく backend 呼び出し中ずっと保持する書き込み側ロックで生存を判断する。
不正な marker、置き換えられたディレクトリ、許可されない子がある場合は fail closed とし、利用者が内容を確認して手動で削除する。
cleanup 失敗は返し、deferred retry する。
Windows の Go `chmod` は同等の秘密性を保証しないため、env ファイルが要る操作は backend を呼ぶ前に `ErrEnvFileUnsupported` で失敗する。

**入力を検証する**。
コンテナ名は前述の名前規則、ラベルキーは CLI と同じ Docker/OCI 形式、ポートは数値範囲とプロトコル(`tcp`/`udp`)、コピー対象のパスは `/` 区切りの POSIX 絶対パスでバックスラッシュを含まず、有効な UTF-8 であることを、CLI へ渡す前に検証する。環境変数キーは空でない有効な UTF-8 で、`=`、Unicode 空白、制御文字、先頭 `#`、先頭 BOM を含まない。値は有効な UTF-8 で、Unicode 制御文字、NUL、CR/LF、U+2028、U+2029 を含まない。制御文字でない Unicode、空白、`=` は値として許可する。制御文字(タブを含む)や不正な UTF-8 を、バックエンドが受け付ける場合でも行区切り env ファイルへ書かないという意図的な互換性変更である。CLI 側にも検証はあるが、ライブラリ側で先に落とすことでエラーメッセージを明確にし、将来の CLI 側検証の変化にも依存しない。 `appleEngine.checkConfig` は image 解決前に platform、name、network、memory、publish の backend 固有制約も適用し、公開 `Pull` も同じ検証を通る。

**認証情報を扱わない**。レジストリ認証はバックエンド CLI に委ねる。Apple Container では `container registry login`、Docker では `docker login` を使う。ライブラリに認証情報を入力する経路はない。

**ログ注入 API はない**。公開 API にロガーフックはない。`CLIError` は診断用に失敗したコマンドの引数と上限付き stderr を提供するが、環境変数の値は一時 env ファイルにあり argv には現れない。watchdog は起動を繰り返し失敗した場合に標準ログへ一度だけメッセージを出す。

**旧バージョンの reaper staging ファイル**。
現在の reaper は inspect 出力を構造化フィールドフィルタへ流して処理し、raw
inspect ファイルを作成しません。変更前のバージョンは名前空間もない `mktemp`
ファイルを使っていたため、inspect 中の reaper が kill されると環境値を含む
ファイルが残る可能性があります。その名前から本ライブラリのファイルとして
安全に特定することはできません。運用者はまず container-go と reaper の
プロセスをすべて停止し、実効のユーザーごと `TMPDIR` でメタデータだけを列挙して、
そのユーザーと該当時間帯の通常ファイルに範囲を絞ります。ファイル内容を表示したり、
シンボリックリンクを追跡したりしないでください。該当実行に確実に帰属するファイル
だけを削除してください。temp ディレクトリ全体の再帰削除は安全ではありません。
inspect 出力に含まれた可能性がある認証情報はローテーションしてください。stale
ファイルを削除しても secret は失効しません。

**create 前の image identity を固定する**。
`PullMissing` と `PullAlways` は local store を準備した後にも image inspect を行い、解決した immutable identity を backend の `run` に渡す。
Docker は repository が一致する registry digest を優先し、取得できない場合は inspect した local image ID にフォールバックする。
別の repository の `RepoDigests` を持つ local alias も同じ local ID で実行し、明示的な pinned repository / digest の conflict だけを mismatch として扱う。
Apple Container は root image descriptor を run / reuse の identity として使い、指定した platform の variant は別の検証情報として保持する。
Apple が未指定の入力を custom default registry へ解決した場合、backend が報告した canonical repository を run 参照と identity 比較に保持する。
descriptor が利用可能な ID だけの record は digest 参照に正規化する。
この処理は inspect 後の local tag 再割り当てが create 対象を変えることを防ぐ。
ただし、mutable tag の pull から inspect までの操作を原子化するものではない。
呼び出し側の Apple `name@digest` と `name:tag@digest` は pinned として扱い、`WithAllowMutableImageTag` を指定しても mutable へ降格させない。
利用可能な identity を返さない backend version では既定で `ErrImageIdentityUnavailable` を返して fail closed する。
repository の provenance がない bare digest は identity として扱わず、bare Docker image ID は inspect がその local ID を検証した場合だけ受け入れる。

Apple Container の `run` に `--pull=never` はないため、pinned Apple reference を `container run` に渡す前に、すべての pull policy で local store にあるかを確認する。
存在せず選択した policy が fetch を許可する場合は exact digest を明示的に pull して再検査し、`container run` の暗黙の fetch は許さない。
local に存在しないことが確認された場合は `ErrImageIdentityNotLocal`、成功した inspect が repository-bearing digest または検証済み local ID を返さない場合は `ErrImageIdentityUnavailable` を返す。
addressability check の transport、permission、cancellation エラーは operational error として返し、mutable fallback の根拠にしない。
`WithAllowMutableImageTag` は identity が取得できない、または resolved reference が local にない mutable input の明示的な互換 fallback であり、identity の保証ではない。
呼び出し側の repository digest、bare digest、Docker image ID を mutable tag に降格させることもない。
local build の Apple image で local digest 参照がない場合は、この明示 fallback が必要になることがある。

Apple は単一 manifest の周囲に local index を合成することがある。
この root は local store の content address として存在するが、`name@digest` で registry から取得できるとは限らない。
そのため resolver は synthetic root を無条件に exact pull しない。
local に addressable な tag または alias を使うか、`WithAllowMutableImageTag` による明示的な fallback を要求する。
新規作成直後の Apple container は `container run` の後に再 inspect し、root、platform、選択された variant が異なる場合は `Run` や reuse が返す前にロールバックする。

## パフォーマンス設計

**子プロセス数を最小にする**。作成と起動はバックエンドの `run --detach` 1 回で行う。不変な UID、イメージ、ラベルの同一性は検証後にキャッシュする。`Endpoint`、`Host`、`MappedPort`、`ContainerIP` は短い（約 1 秒）動的 inspect スナップショットを再利用する。これは wait パッケージのライフサイクル照会（最大 1 秒に 1 回）に揃え、接続 readiness の 100ms ごとの retry で毎回 CLI を起動しないためである。created などで利用可能な IP や host binding が無い結果は cache hit にしない。`State` と reuse 後の検証は常に再 inspect する。これにより公開ポートを不変扱いせず、stale なネットワーク／ポートデータを無期限に保持することもなく、子プロセス数を抑える。

**接続で確認できる待機は接続で行う**。`ForListeningPort` と `ForHTTP` は解決したエンドポイントへ直接接続する。`ForExec` と状態照会はバックエンド CLI を呼び出し、`ForLog` はログストリーム API を使う。接続 probe の既定間隔は 100ms、exec probe は 250ms である。通常のライフサイクル照会を最大 1 秒間隔に制限し、ログストリームの失敗時は直ちに状態を分類する。

**並列起動を妨げない**。コンテナ作成にグローバルロックを置かない（reaper の ID 登録だけ 1 行の書き込みにミューテックスを使う）。Apple Container は既定でホストポートを消費せず、Docker はデーモンが公開ポートを原子的に割り当てる。

**要求された範囲だけスナップショットを制限する**。`Logs` と成功する `LogsWithOptions` 呼び出しは有限の CLI snapshot を完了するが、`Logs` は利用可能な全出力を buffer できる。`LogsOptions{Tail, Since}` は backend 固有で、Docker は両方を受け付けるが、確認した Apple Container CLI の各 version は Tail に `-n` を使い Since を持たない。Apple backend は Tail を `-n` に対応させ、Since を `ErrUnsupportedCapability` で拒否する。どちらの経路も byte 上限を追加しない。`FollowLogs` は意図的に上限のない stream であり、`io.ReadCloser` を閉じるか context をキャンセルすると CLI process を終了する。wait failure の診断 log 末尾は 1MiB 上限である。

**ストリームを有限に保つ**。
`FollowLogs` は `container logs --follow` の子プロセスを起動して `io.ReadCloser` として返す。
`Close` またはコンテキスト取消では、直接の CLI 子プロセスを終了して回収します。
Unix では停止状態を確認したあと CLI の process group へベストエフォートの `SIGKILL` を送り、続けて直接の子にも kill を送ります。
同じグループに残った helper は終了することがありますが、回収は保証しません。
停止状態を確認できない場合は直接の子（Linux では pidfd）だけを対象にし、古い PGID には signal を送りません。
直接の子を回収した後は、古い数値の process-group ID は使いません。
Windows では保持した process handle で直接の CLI 子だけを終了し、descendant は終了しません。
その他の対応 OS でも直接の子だけを対象とします。
ForLog が診断用に保持するログは 1MiB を上限とする。
`LogsOptions.MaxBytes`、`WithExecMaxBytes`、`ExecTo`、`LogsTo` は byte 上限（先頭 prefix のみ保持）または呼び出し元所有の sink を提供し、bounded capture は `Truncated` と `ErrOutputTruncated` で truncation を通知する。`ForExec` は `ExecTo` で出力を `io.Discard` へ送り、buffer しない。

ストリームのエラーは 2 つの段階に分かれる。
`Stream` と公開 API の `FollowLogs` は起動エラーを返す。
ストリームを返した後の CLI 終了エラーは `Read` から返るため、呼び出し側は必ず EOF まで読む必要がある。
`Close` とコンテキスト取消は意図的な終了経路なので、EOF またはコンテキストエラーになることがある。

**操作ごとに期限を決める**。inspect、copy、スナップショットログ、delete、prune などの照会的な操作、および public `Exec` は、呼び出し元に deadline がなければ通常 30 秒を既定にする。`Run` と明示的な image fetch は pull と create に 10 分の予算を使う。共有 pull のリーダーだけは、個別の呼び出し元のキャンセルから意図的に切り離す。一人の呼び出し元が他の待機者の pull を中止できないようにするためで、各待機者は自分の context がキャンセルされた時点で待機を終了する。`FollowLogs` はライブラリ側の deadline を追加せず、呼び出し元の context をそのまま渡す。`Exec` は呼び出し元に deadline がない場合 30 秒の既定 deadline を適用し、`WithExecTimeout(0)` は意図的な長時間実行 command のための明示的な escape hatch とする。`Exec` のインフラ確認には、期限付きの後続 context を使うことがある。共有 pull のリーダー以外の操作でライブラリ側の既定を適用するときには、既存の呼び出し元 deadline を保持する。timeout の分類は structured context、`Timeout() bool`、signal の証拠だけを使い、arbitrary な workload stderr を timeout として解釈しない。コンテキスト取消時は直接の CLI 子プロセスを kill して回収する。Unix は所有中の CLI process group に残った descendant へベストエフォートの `SIGKILL` を送ることがあるが、回収はせず、停止状態を確認できない場合や回収後は古い数値 PGID に signal を送らない。Windows は保持した process handle で直接の子だけを終了する。子孫の回収は保証しない。

## エラー処理

root と backend の error は `errors.Is` と `errors.As` で判別できる。`Classify` は元の CLI エラーと probe エラーを `ErrSystemNotRunning` の error chain に保持する。target に一致する entry がない successful inspect response は `ErrContainerNotFound` を返す。Docker では empty な output も not-found、malformed な output は decode error であり、要求した ID や name に一致しない valid object は parser が拒否する。built-in wait strategy は timeout / cancellation 経路で context error を保持する。

- `ErrSystemNotRunning`：CLI 呼び出しが失敗した後、バックエンド固有の liveness probe が Apple Container のシステムサービスまたは Docker daemon の停止を判定した場合に返す。元の CLI エラーと probe エラーは error chain に保持し、permission、configuration、caller cancellation はこの sentinel に分類しない。
- `ErrContainerNotFound`：backend が missing container を示す CLI failure を分類した場合、または inspect response に要求した ID / name に一致する entry がない場合。malformed な inspect output は decode error であり、この sentinel にはならない。
- `ErrImageNotFound`：Docker の `PullNever` precheck がローカルイメージを見つけなかった場合。
- `ErrPullNeverUnsupported`：backend（Apple Container）が暗黙の fetch なしに `PullNever` を守れない場合。`Run` と `Pull` は CLI 呼び出し前に拒否する。`PullMissing` または `PullAlways` を使う。
- `ErrImageIdentityUnavailable`：成功した image inspect が repository-bearing digest または検証済み local image ID を返さず、mutable tag fallback も指定されていない
- `ErrImageIdentityNotLocal`：選択した policy で pinned reference が local image store に存在しないことが確認された。Apple の `container run` に暗黙の fetch を許さない
- `ErrImageIdentityMismatch`：inspect が別 image の identity を返した。mutable tag fallback は使わない
- `ErrOutputDelivery`：caller-owned output sink が CLI output を拒否した場合。元の writer error は chain に残る
- `ErrPortNotExposed`：ポートが宣言も公開もされておらず、または宣言済みポートに利用できる host binding がなかった場合。
- `ErrGenerationReplaced`：delete 時の generation check が同じ名前の置き換えを検出した場合。reuse は readiness 後の再 inspect で置き換えを検出し、handle を返さない。
- `*CLIError`：バックエンド CLI が 0 以外で終了した場合。バイナリ、引数、終了コード、stderr（診断用の stderr は 64KiB 上限）を保持する。root の `CLIError` alias は `v0.2.0` にはない現在の開発版追加である。
- `ErrContainerNotFound` と `ErrGenerationReplaced` も現在の開発版追加である。
- `ErrEnvFileUnsupported`：空でない環境変数指定には安全な env ファイルが必要だが、現在のプラットフォームではユーザー単位の秘密性を確保できない
- `ErrCopyFileNotRegular`：Docker の copy-out 結果が regular file でない
- `ErrCopyFileFromContainerUnsupported`：選択した backend または host が型安全な copy-out を実装していない(Apple Container、Docker client/server が 29.7.0 未満、必要な open flag がない host、または Windows Go 1.23 から 1.25)
- `ErrInvalidConfig` / `*ConfigError`：作成前に拒否した backend 非互換の option 組み合わせ
- `ErrEndpointUnreachable`：リモートデーモンの loopback など、client から到達できない binding
- `*ExecTerminationError` / `ErrExecTerminationUnsupported`：context の期限切れまたは取消によって local CLI が終了した場合。backend 側 exec process は残っている可能性がある

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
├── exec.go           // Exec、ExecTo、ExecOption
├── logs.go           // Logs、LogsWithOptions、LogsTo、FollowLogs
├── output.go         // bounded reader、sink、truncation error
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

type WriterRunner interface {
    Runner
    RunTo(ctx context.Context, stdout, stderr io.Writer, args ...string) (OutputStats, error)
}
```

## テスト戦略

**ユニットテスト**では、固定 JSON を返すフェイク `Runner` を注入し、実機なしで argv 組み立て、JSON デコード、エラー分類、wait strategy のロジックを検証する。本番 code が非 nil を前提とする依存には、テストでもフェイク実装を渡す。integration tag のない `examples/compile_test.go` は、README の英語版・日本語版と design document の両方から fence された code block をすべて抽出し、対応する block をコメント除去後に比較する。`go` block は parse して一時 module 内で compile し、一時 module は現在の checkout への引用符付きローカル `replace` を使う。`text` のシグネシャ一覧は実行対象ではない。

**統合テスト**は `integration` build tag を使う。root のテストスイートには Apple Container と Docker のライフサイクル、接続、exec、copy、cleanup、Reuse、watchdog のケースがある。各バックエンドの helper は先に CLI とサービスを確認し、利用できなければ skip する。`make integration` は両バックエンドを実行して pull が重い bench と singleflight を除外し、`make integration-docker` は Docker を選択する。`make bench-integration` は pull が重いシナリオと独立したベンチマークモジュールを実行する（nested `bench/` module は Go 1.26+）。

**CI**：`.github/workflows/ci.yml` は `ubuntu-latest` で Go 1.26.0 と安定版 Go を使い、ユニットテスト、race テスト、lint、`govulncheck`、Docker 統合テストを実行する。
cross-platform job は `windows-latest` と `macos-latest` でもユニットテストと integration tag 付き vet を実行する。
Apple Container 統合テストは必要なサービスとホスト環境がないためローカルで実行する。
Windows の Docker copy-out は Go 1.26 以上と Docker client/server 29.7.0 以上を使って手動で統合テストする。
ローカルでは `CONTAINERGO_BACKEND=apple` または `CONTAINERGO_BACKEND=docker` でバックエンドを 1 つだけ実行できる。

## バックエンド（v0.2 と現在の開発版）

v0.1 は Apple Container 専用だった。
v0.2 で Docker バックエンドを追加し、Linux と Windows でも同じ API を使う。

**選択**：`CONTAINERGO_BACKEND` 環境変数を最優先し、`apple` または `docker` を受け付ける。未指定なら OS で決め、macOS は Apple Container、Linux と Windows は Docker になる。macOS で Docker Desktop を使う場合は `CONTAINERGO_BACKEND=docker` を設定する。

**方式**：Docker も `os/exec` の CLI ラッパーにする。container-rs は Docker Engine API を直接呼ぶが、本ライブラリは直接 API を使わない。直接 API クライアントは tar の生成、ログストリームの逆多重化、レジストリ認証、Windows の名前付きパイプを自前実装することになるため、CLI ラッパーを使う方が既存の runner 層（argv 実行、timeout、streaming）を共有できる。`DOCKER_HOST`、Docker context、認証の解決は docker CLI に委ねる。`DOCKER_HOST` および Docker context の remote daemon を検出し、remote endpoint として解決する。

**内部構造**：バックエンドは「引数の組み立て」と「inspect 出力の正規化」だけを担う内部インターフェースにする。プロセス実行(ランナー)、待機戦略、クリーンアップ、検証は両バックエンドで共有する。正規化した情報には、ライフサイクル状態(created、running、restarting、stopping、stopped、paused、unknown)、ラベル、image 参照、利用できる場合の backend ID、コンテナ IP、Docker network mode、公開ポートの束縛（コンテナポート → ホストアドレスとポート）を含める。backend ID が stable identity で、IP と binding は動的であり、endpoint 読み取りでは短い dynamic-inspect TTL の間だけ再利用する。

**Image 処理**：両バックエンドとも、明示的な image inspect と pull コマンドで pull policy を実装する。Docker の run argv は `--pull=never` を追加し、Apple Container も同じ明示的な policy 経路を使う。並行 pull は同一プロセス内で、backend、image、platform、操作が同じ場合だけ集約する。

**Apple capability の違い**：`appleEngine.checkConfig` は、CLI 呼び出し前に non-Linux または不正な Apple platform(bare `linux` と invalid variant を含む)、Apple 規格外の container/network name、200 MiB 未満の memory と overflow、publish port 1、publish の overlap、64 個を超える publish descriptor を拒否する。`WithPrivileged`、`WithExtraHost`、`WithHostname` は `ErrUnsupportedCapability` で拒否し、Apple の `--cpus` が整数のみを受け付けるため小数の `WithCPUs` も同じ error で拒否する。`WithShmSize`、`WithCapAdd`、`WithCapDrop`、`WithUlimit`、整数の CPU 値は Apple の `--shm-size`、`--cap-add`、`--cap-drop`、`--ulimit`、`--cpus` にそのまま渡す。`WithExposedPorts` は library 側の宣言で、Apple の `--publish` flag には変換しない。Apple には run 時の no-fetch スイッチがないため `PullNever` も拒否し、`ErrPullNeverUnsupported` と明示的な fallback(`PullMissing`/`PullAlways`)を契約とする。公開 `Pull` にも同じ platform/capability 検証を適用する。

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
`DOCKER_HOST` と Docker context（アクティブまたは明示設定）の双方が remote daemon として解決される。
ランダム割り当てはデーモンが起動時に原子的に行うため、Apple Container で避けた「空きポート確保の競合」は発生しない。
Apple Container バックエンドの既定(直接 IP)は変えない。

**クリーンアップの違い**: watchdog リーパーは削除サブコマンドをバックエンドごとに切り替える(Apple は `delete --force`、Docker は `rm --force`)。
リーパーは `/bin/sh` に依存するため Windows では動かない。
v0.2 の Windows は通常経路(`Cleanup`、ロールバック)のみとし、リーパーなしをドキュメントに明記する。
`Prune` は Docker ではデーモンのフィルタ(`--filter label=... --filter status=exited --filter status=dead`)を使える。`dead` はデーモンが回収できない停止済みコンテナで、`exited` だけだと取りこぼす。出力は `--no-trunc --format {{.ID}}` の full container ID である。

**liveness detection**：probe command は backend ごとに切り替える（Apple は `system status`、Docker は `version --format {{.Server.Version}}`）。

## スコープ外

- `container build` / `docker build` による Dockerfile ビルド。
- ネットワークの作成と管理。`WithNetwork` は既存の名前付きネットワークへ接続できるが、ライブラリはネットワークを作らない。
- ボリュームの作成とライフサイクル管理。`WithMounts` は bind、名前付きボリューム、tmpfs を使えるが、ライフサイクルは呼び出し元が管理する。remote Docker daemon への bind mount は `ErrUnsupportedCapability` で拒否し、ローカル Docker daemon では Windows の host path も受け付ける。
- testcontainers のモジュール群（postgres など）に相当する高水準パッケージ（コア安定後に検討）。
- Docker Engine API の直接クライアント。現在の transport は CLI ラッパーであり、直接クライアントは隠れた fallback ではなく将来的な決定事項である。

## 未解決の製品上の決定

現在の API が暗黙に意味を与えない事項を、ここに明記する。

- **Apple の名前ベース削除**：non-empty で一致する作成世代チェックと per-name `flock` は、同じ host で本ライブラリの generation-checked `Terminate` / failed-create cleanup、`Prune` / `PruneReuseGroup`、および名前で address する reaper entry を保護する。同じ窓で外部の CLI が delete して再作成した場合には区別できず、外部競合を閉じるには backend の不変 ID または原子的な条件付き削除が必要である。
- **remote Docker の検出**：remote endpoint は `DOCKER_HOST` および Docker context（`tcp://` および `ssh://` トランスポートをサポート）の両方から解決される。
- **Reuse の互換性**：両 backend で image を検査する。Docker は published port binding も検査するが、Apple は inspect data に `WithExposedPorts` の宣言を残さないため比較できない。`env`、`cmd`、`mounts` の差は意図的に attach する。今後の release で設定を比較すべきかは未解決の製品上の決定であり、分離が必要なら別の名前を使う。
- **logger injection**：公開 logger hook は存在しない。追加するには新しい API と、公開できるコマンドデータの範囲を決める決定が必要である。
- **remote bind mount**：remote Docker daemon への bind mount は、daemon が source path を自分の host で解決するため拒否している。remote bind mount をサポートするかは未解決である。ローカル Docker daemon では Windows の host path を受け付ける。
- **UDP readiness**：`ForListeningPort` と `ForExposedPort` は TCP-only で、UDP 宣言は endpoint 設定にだけ使う。UDP probe を追加するかは未解決である。
- **Stop timeout の精度**：公開 option は duration を受け付けるが、両 backend は whole seconds を取るため正の sub-second 値は切り上げ、backend limit を強制する。sub-second 精度を保持するかは未解決である。
- **reaper 登録**：登録は best-effort である。登録 error は無視され、通常の cleanup が fallback として残る。
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

- [apple/container](https://github.com/apple/container) v1.2.2 / v1.3.0 の command・source リファレンスおよび `ContainerResource` ソース
- [shiguredo/container-rs](https://github.com/shiguredo/container-rs)：XPC 直結方式の先行実装。watchdog リーパー、クリーンアップ契約、macOS 固有の制約(ポート競合、転送切断)の整理を参考にした
- [testcontainers-go](https://github.com/testcontainers/testcontainers-go) v0.44.0：API 形状(functional options、wait 戦略、nil 安全なクリーンアップ)の参照元
