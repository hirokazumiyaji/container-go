# container-go 設計ドキュメント

English (primary): [design.md](design.md)

作成日: 2026-08-18（v0.2 バックエンド節を 2026-08-19 追記）
最終同期: 2026-09-25
対象: Apple Container v1.2.x–1.3.x（macOS 26 以降、Apple Silicon）、Docker 29.x（Linux、Windows、macOS）、Go 1.23 以降

この文書は現在のチェックアウトにある実装を説明する。
「実装フェーズ」の節は初期設計の歴史として残るが、現在の API や今後のロードマップを約束するものではない。
現在の節で説明する公開 API と動作は、実装とテストを基準にしている。

## 目的

**container-go** は、Apple Container（[apple/container](https://github.com/apple/container)）と Docker をバックエンドとする testcontainers スタイルの Go ライブラリである。
Go のテストコードから使い捨てコンテナを起動し、接続情報を返し、作成したコンテナを通常のテスト終了時と異常終了時のベストエフォート経路で削除する。

先行事例として、Rust には [shiguredo/container-rs](https://github.com/shiguredo/container-rs) がある。
本ライブラリは同じ問題領域を Go で扱うが、実現方式は後述のとおり異なる。

設計上の制約は次の三つである。

- **依存ゼロ**：サードパーティの Go モジュールに依存せず、標準ライブラリだけで実装する。
- **セキュリティ**：外部プロセス起動と入力値の扱いでインジェクションや情報漏洩の経路を作らない。
- **パフォーマンス**：テストスイートを左右するのはコンテナ（VM）の起動時間である。ライブラリ側のオーバーヘッドをそれに対して無視できる水準に保ち、並列起動を妨げない。

## 前提とする Apple Container の仕様

設計の根拠となる Apple Container の仕様を先に整理する。
v1.2.x–1.3.x で確認し、フィクスチャは 1.2.2 と 1.3.0 を対象とする。

- ホスト要件は macOS 26 以降かつ Apple Silicon である。
- 各コンテナは軽量 VM として起動し、vmnet ブリッジ（既定ネットワークは `default`、`192.168.64.0/24`）上の実 IP を持つ。ホストはこの IP に直接到達できるため、ポート公開（`--publish`）は必須ではない。
- すべての操作は `container` CLI から行える。`ls --format json` と `inspect` は機械可読な JSON を返し、追加フィールドは `internal/inspect` が無視する。
- CLI は launchd 配下の `container-apiserver` と XPC で通信する。サービスが未起動だとコマンドは失敗し、`container system status` で状態を確認する。
- コンテナ名がそのまま ID になる。名前は `^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$` に一致し、1 文字から 63 文字までである。
- ヘルスチェック、`wait` コマンド、イベントストリーム、`ls` のラベルフィルタ、実行中コンテナへの再アタッチは存在しない。必要な動作はクライアント側で実装する。
- `--label` は存在するが、ラベルの絞り込みは JSON 出力をクライアント側で行う必要がある。ラベルキーは小文字の Docker/OCI 形式に限られる。
- `container cp` は実行中のコンテナにだけ使える。
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

```go
func Run(ctx context.Context, image string, opts ...Option) (*Container, error)
```

コンテナを新規作成する必要がある場合、`Run` は起動前に pull policy を適用してから待機戦略を完了する。
既定の `PullMissing` はローカルイメージストアを検査し、イメージがないときだけ明示的に pull する。
`PullAlways` は呼び出しごとに明示的な pull を要求し、`PullNever` は検査だけを行い、イメージがない場合は `ErrImageNotFound` を返す。
Docker の run コマンドには `--pull=never` を渡すため、CLI が pull を重複させない。
起動後の操作や待機が再利用でない経路で失敗した場合は、作成したコンテナをロールバックしてからエラーを返す。
Reuse の共有ライフタイムは後述の別契約で扱う。

オプションは functional options で提供する。
現在のオプションは次のとおりである。

- `WithExposedPorts(ports ...string)`：接続対象のコンテナポート（`"6379/tcp"` 形式）を宣言する。`MappedPort` と `Endpoint` はこの宣言を解決に使える。Docker は宣言したポートを自動公開し、Apple Container は既定でコンテナ IP から解決する。
- `WithEnv(env map[string]string)`：環境変数を設定する。値はコマンドライン引数ではなく一時ファイル経由で渡す。
- `WithCmd(cmd ...string)` / `WithEntrypoint(entrypoint string)`：コマンドとエントリポイントの上書き。エントリポイントは `docker run --entrypoint` の仕様どおり 1 トークンとする。複数トークンは `WithCmd` を使う。
- `WithWaitStrategy(s wait.Strategy)`：起動完了の判定方法を指定する。
- `WithName(name string)`：コンテナ名を指定する（省略時は `containergo-<ランダムな 16 進数>`）。
- `WithLabels(labels map[string]string)`：追加ラベルを指定する。ライブラリが管理するラベルは予約済みである。
- `WithMounts(mounts ...Mount)`：bind、名前付きボリューム、tmpfs のマウントを指定する。
- `WithFiles(files ...File)`：起動後のコンテナへファイルをコピーする。コピーに失敗すると `Run` をロールバックする。
- `WithPublishedPort(spec string)`：ホスト側ポートの明示的な公開を指定する。Apple Container は直接 IP を使い、Docker は公開ポートを自動公開するため、この指定は任意である。
- `WithPullPolicy(policy PullPolicy)`：`PullMissing`（既定）、`PullAlways`、`PullNever` を選ぶ。
- `WithReuse()`：名前付き `Run` を get-or-create にする。
- `WithReuseGroup(group string)`：再利用するコンテナにラベルを付け、`PruneReuseGroup` の対象にする。`WithReuse` が必要で、再利用キーには含まれない。
- `WithCPUs(n int)` / `WithMemory(size string)`：リソース制限を指定する。
- `WithUser(u string)` / `WithWorkingDir(dir string)`：実行ユーザーと作業ディレクトリを指定する。
- `WithNetwork(name string)`：既存の名前付きネットワークへ接続する。
- `WithPlatform(p string)`：`linux/amd64` のようなイメージプラットフォームを指定する（Apple Container では Rosetta を含む）。

バックエンド中立の CLI 面にないオプションは意図的に提供しない。
公開 API にログ注入用のフックはない。
ログを使うには `FollowLogs` でストリームを取得するか、`Logs` と `LogsWithOptions` でスナップショットを取得する。

### Container ハンドル

```go
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

`Exec` は終了コードと標準出力、標準エラーをまとめて返す。
実行中コンテナで実行したコマンドが 0 以外で終了した場合、その終了コードはインフラエラーではなく結果として返す。
停止中または到達できないコンテナの操作はエラーを返すことがある。
`WithExecEnv`、`WithExecUser`、`WithExecWorkDir` は個別の exec 呼び出しを設定する。環境変数の値は `WithEnv` と同じ一時ファイル方式を使う。
`Logs` と `LogsWithOptions` は有限のスナップショットを返し、`FollowLogs` はリーダーを閉じるか context をキャンセルするまでストリームする。
`LogsOptions` は現在 `Tail` と `Since` を公开し、スナップショットを制限する。
`Terminate` は世代ガードを持ち、別のプロセスが名前を再利用したコンテナを削除しない（後述の Reuse を参照）。

`Terminate` は Apple Container では `container delete --force`、Docker では `docker rm --force` に対応する。
コンテナが既にない場合は成功として扱い、冪等である。
`Cleanup(t, ctr)` と `TerminateContainer(ctr)` は nil 安全なヘルパーであり、`Run` のエラーを確認する前にクリーンアップを登録できる。

## 接続エンドポイントの設計

testcontainers の Docker 実装ではコンテナポートをホストのランダムポートへ公開し、`localhost:<mapped>` へ接続する。
Apple Container ではこの方式を既定にしない。

既定では、`Host` は inspect の `status.networks[0].ipv4Address` から CIDR サフィックスを除いたコンテナの実 IP を返し、`MappedPort` はコンテナポートをそのまま返す。
この方式を既定とする理由は三つある。

- Apple Container にはランダムポート割り当てがない。 ホストポートを自前で確保すると「空きポートを探してから起動する」までの競合が生じる（container-rs も同じ制約を持つ）。直接 IP 接続なら ホストポートを消費しないため、この競合が起きない。
- ホストポート衝突がないので、テストの並列実行を無制限に拡張できる。
- ポート転送プロキシを経由しないため、転送実装の不具合（大きな転送が途中で切れる事例が報告されている）の影響を受けない。

クライアントが `localhost` を要求する場合、またはコンテナ IP に到達できない構成では、`WithPublishedPort("127.0.0.1:15432:5432")` のように明示的に公開する。
公開すると `Host` は指定したホストアドレスを、`MappedPort` はホストポートを返す。

`MappedPort` と `Endpoint` は、`WithExposedPorts` で宣言したポートと明示的に公開したポートだけを解決する。
宣言は待機戦略にも使われる（`ForListeningPort` の既定ポートなど）。

## 待機戦略

Apple Container にはヘルスチェックも wait コマンドもないため、起動完了の判定はすべてクライアント側で行う。
`wait` サブパッケージは次の戦略を提供する。

- `wait.ForLog(pattern)`：`FollowLogs` のストリームを読み、部分文字列（`AsRegexp` による正規表現も可）が現れるまで待つ。照合は行単位で行い、`WithOccurrence(n)` で出現回数を指定できる。ストリーム方式なので poll 方式ではない。
- `wait.ForListeningPort(port)`：解決したエンドポイントへの TCP 接続が成功するまで poll する。
- `wait.ForExposedPort()`：`WithExposedPorts` で最初に宣言したポートを使う。
- `wait.ForHTTP(path)`：解決したエンドポイントへ HTTP リクエストを送り、ステータスが条件（既定は 2xx、`WithStatusCodeMatcher` で変更可）を満たすまで待つ。`WithPort` と `WithMethod` で対象を指定し、`WithHeaders`、`WithHeader`、`WithBasicAuth`、`WithTLS`、`WithTLSConfig`、`WithHTTPClient` で認証、TLS、HTTP クライアントを設定する。
- `wait.ForExec(cmd)`：バックエンド CLI でコマンドを起動し、終了コード（既定は 0）が条件を満たすまで待つ。
- `wait.ForAll(strategies...)` / `wait.ForAny(strategies...)`：戦略を合成する。子戦略は各自の設定を保持する。合成型は `WithStartupTimeout` で全体を制限できるが、`WithPollInterval` は公開しない。

基本戦略の起動タイムアウトは既定 60 秒である。
`ForListeningPort`、`ForExposedPort`、`ForHTTP` は既定 100 ミリ秒で poll し、`ForExec` は既定 250 ミリ秒で poll する。
現在の `ForLog` 型にも `WithPollInterval` があるが、ストリームを読むため効果がない。
接続または HTTP の待機中にコンテナが停止した場合は、タイムアウトを使い切らずに失敗する。
再利用でない `Run` の待機が失敗した場合はコンテナをロールバックし、1MiB 上限のログ末尾をエラーに付ける。再利用の待機失敗では共有コンテナを残す。

戦略インターフェースは次のとおりである。

```go
type Strategy interface {
    WaitUntilReady(ctx context.Context, target Target) error
}
```

`Target` はコンテナ IP、宣言済みポート、ログリーダー、exec、状態照会を提供する小さなインターフェースであり、`*container.Container` を `container.Run` が適合させる。
依存方向は `container` から `wait` だけであり、逆方向を作らないため循環参照を避けられる。

## クリーンアップ

ライブラリは通常の終了経路と、異常終了時のベストエフォート経路を用意する。

**通常経路**：`Cleanup(t, ctr)` が `t.Cleanup` 経由で削除を登録する。`TerminateContainer(ctr)` は nil 安全な defer 形式である。再利用でない経路で `Run` がコンテナ作成後に失敗した場合は、ロールバックで削除を試み、削除に失敗した場合はそのエラーを隠さず返す。再利用コンテナは後述の共有ライフタイム契約に従う。

**異常終了（SIGKILL、パニック、`os.Exit`）**：defer も `t.Cleanup` も実行されない。最初の実 CLI コンテナを登録したときに、ライブラリは外部 `/bin/sh` の watchdog リーパーを遅延起動し、登録済み ID をパイプへ書く。親プロセスのパイプが閉じられると、リーパーは各 ID の強制削除を試みて終了する。リーパーは保険であり、トランザクション保証ではない。起動と削除はベストエフォートで、Windows では利用できない。親プロセスが生きている間は何もせず、各バックエンド呼び出しは POSIX の `sleep` と `kill` による期限で制限される。

Docker では不変のコンテナ ID があればリーパーに渡す。登録はベストエフォートで、登録に失敗しても `Run` は失敗させない。
Apple Container には別の不変 ID がないため、作成世代と生成ラベルを保存し、名前で削除する前にラベルを検査する。
名前ベースの保証は同じホストでこのライブラリを使う協調プロセスに限られる。外部 CLI による delete と再作成は名前では区別できない。

**セッションラベル**：作成するコンテナには次のラベルを付ける。

- `com.github.hirokazumiyaji.container-go`：`true`（管理対象の印）
- `com.github.hirokazumiyaji.container-go.session`：プロセスごとのランダム ID
- 作成世代ラベルと、Reuse 時の再利用グループラベル

Apple CLI にはラベルフィルタがないため、孤児の掃除は `container ls -a --format json` をクライアント側で絞り込んで行う。`Prune(ctx)` は過去のセッションを含め、管理ラベルを持つ停止済みコンテナを削除する。Docker は同じ絞り込みをデーモン側で行える。

`CONTAINERGO_KEEP=1` を設定すると `Cleanup`、`TerminateContainer`、リーパー登録を省略する。明示的な `Container.Terminate` と `Run` のロールバック経路の動作は変えない。

匿名ボリュームは `--rm` でも残るので、ライブラリは匿名ボリュームを作らない。ボリュームを使う場合は名前付き，そのライフサイクルは呼び出し元に委ねる。

## Reuse

`WithReuse` は安定した `WithName` に対する `Run` を get-or-create にする。共有は同じホストのプロセス間で行う。既存コンテナは `WithReuse` で作成されていればよく、各呼び出しは自分の待機戦略を再実行する。互換性チェックは意図的に狭く、image 参照と宣言または公開したポートだけを比較する。`env`、`cmd`、`mounts` の差は既存コンテナへ黙って attach する。分離が必要なら異なる名前を使うか、`Exec` で状態を初期化する。

各作成には 16 桁の 16 進数 `creationLabel` 世代を付ける。
`Terminate` と停止済み再作成経路は、置き換わった世代の削除を拒否する。
Docker では `docker run` の出力または inspect が返した不変の `Id` を保持して削除するため、世代検査は不要であり、置き換え先は同じ ID を持たない。
Apple Container は名前だけでコンテナを参照するので、削除は名前ベースになる。
inspect と削除は一時ディレクトリ内の名前単位 `flock`（`containergo-<name>.lock`）で直列化する。
この保証は同じホストで本ライブラリを使う協調プロセスに限られる。
外部ツールが同じ窓で `container delete` と再作成を実行しても名前では区別できず、解決には不変 ID または Apple Container が提供しない原子的な条件付き削除が必要になる。
not-found 以外の inspect 失敗は fail closed として削除を中止し、`Run` のロールバックは結果のエラーにコンテナが残ったことを含める。
watchdog リーパーは Docker では `Id`、Apple では世代を登録し、ラベルを行頭固定の JSON フィールドとして読む。
ラベルの部分文字列では一致にせず、世代が違えば削除しない。
各バックエンド呼び出しは POSIX の `sleep` と `kill` による 10–30 秒の期限を持ち、1 つの daemon 呼び出しが後続を妨げない。
リーダーの pull と create は独立した `runTimeout` 予算を使い、`reuseAttachTimeout` は別プロセスのコンテナへの attach polling だけを制限する。

## セキュリティ設計

外部プロセスを起動するライブラリとして、次の原則を守る。

**シェルを経由しない**。すべての CLI 呼び出しは `exec.Command` に引数配列を渡し、シェル文字列を組み立てない。唯一の例外は watchdog リーパーのシェルスクリプトである。本文は固定文字列で、コンテナ ID は stdin のデータとしてだけ渡す。スクリプトは `set -f`、`IFS=`、`read -r`、変数のクォートで語分割とグロブ展開を封じる。ライブラリは Apple 形式の名前 `^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$` を検証してからパイプへ書く。リーパー登録の失敗は無視する。二重の防御により、登録された ID 経由のコマンド注入を成立させない。

**環境変数を argv に載せない**。`--env key=value` を使うと、値がプロセス一覧（`ps`）から見える。データベースパスワードなど秘密情報を環境変数で渡す用途による情報漏洩を避けるため、ライブラリは `os.MkdirTemp` の mode 0600 ファイルへ環境変数を書き、`--env-file` で渡して起動後に削除する。

**入力を検証する**。コンテナ名（前述の規則）、ラベルキー（CLI と同じ Docker/OCI 形式）、ポート（数値範囲と `tcp`/`udp`）、環境変数キー（`=` と NUL を含まない）、コンテナ内コピー先パス（絶対パス、有効な UTF-8、NUL なし）を CLI へ渡す前に検証する。ホスト側コピー元パスは絶対パスへ解決する。CLI にも検証はあるが、先にライブラリで落とすことでエラーメッセージを明確にし、将来の CLI の変化に依存しない。

**認証情報を扱わない**。レジストリ認証はバックエンド CLI に委ねる。Apple Container では `container registry login`、Docker では `docker login` を使う。ライブラリに認証情報を入力する経路はない。

**ログ注入 API はない**。公開 API にロガーフックはない。`CLIError` は診断用に失敗したコマンドの引数と上限付き stderr を提供するが、環境変数の値は一時 env ファイルにあり argv には現れない。watchdog は起動を繰り返し失敗した場合に標準ログへ一度だけメッセージを出す。

## パフォーマンス設計

**子プロセス数を最小にする**。作成と起動はバックエンドの `run --detach` 1 回で行う。設定、ラベル、image、ネットワークアドレス、公開ポートなど不変な情報は最初の inspect でキャッシュし、状態は必要なときに再照会する。

**接続で確認できる待機は接続で行う**。`ForListeningPort` と `ForHTTP` は解決したエンドポイントへ直接接続する。`ForExec` と状態照会はバックエンド CLI を呼び出し、`ForLog` はログストリーム API を使う。接続 probe の既定間隔は 100ms、exec probe は 250ms である。

**並列起動を妨げない**。コンテナ作成にグローバルロックを置かない（reaper の ID 登録だけ 1 行の書き込みにミューテックスを使う）。Apple Container は既定でホストポートを消費せず、Docker はデーモンが公開ポートを原子的に割り当てる。

**ストリームを有限に保つ**。`Logs` と `LogsWithOptions` は有限のスナップショットをバッファする。`FollowLogs` は意図的にストリームであり、`io.ReadCloser` を閉じたり context をキャンセルしたりすると CLI プロセスを終了する。待機失敗時の診断ログ末尾は 1MiB 上限である。

**操作ごとに期限を決める**。inspect、copy、スナップショットログ、delete、prune などの照会的な操作は、呼び出し元に deadline がなければ通常 30 秒を既定にする。`Run` と明示的な image fetch は pull と create に 10 分の予算を使う。共有 pull のリーダーだけは、個別の呼び出し元のキャンセルから意図的に切り離す。一人の呼び出し元が他の待機者の pull を中止できないようにするためで、各待機者は自分の context がキャンセルされた時点で待機を終了する。`Exec` のコマンド呼び出しと `FollowLogs` はライブラリ側の deadline を追加せず、呼び出し元の context をそのまま渡す。`Exec` のインフラ確認には、期限付きの後続 context を使うことがある。共有 pull のリーダー以外の操作でライブラリ側の既定を適用するときには、既存の呼び出し元 deadline を保持する。

## エラー処理

エラーは `errors.Is` と `errors.As` で判別できるようにする。

- `ErrSystemNotRunning`：CLI 呼び出しの後にバックエンドの liveness probe も失敗した場合。Apple Container のヒントは `container system start`、Docker のヒントは Docker daemon の起動である。
- `ErrContainerNotFound`：コンテナ操作でコンテナを見つけられなかった場合。
- `ErrImageNotFound`：`PullNever` の `Run` でローカルイメージが見つからなかった場合。
- `ErrPortNotExposed`：ポートが宣言も公開もされておらず、または宣言済みポートに利用できる host binding がなかった場合。
- `ErrGenerationReplaced`：世代ガード付き削除が同じ名前の置き換えを削除しなかった場合。
- `*CLIError`：バックエンド CLI が 0 以外で終了した場合。バイナリ、引数、終了コード、stderr（診断用の stderr は 64KiB 上限）を保持する。

再利用でない `Run` が待機タイムアウトで失敗した場合、返り値には上限付きコンテナログ末尾を含め、ロールバック削除を行う。ロールバック削除の失敗も返り値のエラーに含め、隠さない。再利用の場合は待機エラーを返し、共有コンテナはそのまま残す。

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

```go
type Runner interface {
    Run(ctx context.Context, args ...string) (stdout []byte, stderr []byte, err error)
}
```

## テスト戦略

**ユニットテスト**では、固定 JSON を返すフェイク `Runner` を注入し、実機なしで argv 組み立て、JSON デコード、エラー分類、待機戦略のロジックを検証する。本番コードが非 nil を前提とする依存には、テストでもフェイク実装を渡す。integration タグなしの `examples/compile_test.go` は、バックエンドを起動せずに README とこの設計文書で使う公開 API 呼び出しを型検査する。

**統合テスト**は `integration` build tag を使う。root のテストスイートには Apple Container と Docker のライフサイクル、接続、exec、copy、cleanup、Reuse、watchdog のケースがある。各バックエンドの helper は先に CLI とサービスを確認し、利用できなければ skip する。`make integration` は両バックエンドを実行して pull が重い bench と singleflight を除外し、`make integration-docker` は Docker を選択する。`make bench-integration` は pull が重いシナリオと独立したベンチマークモジュールを実行する。

**CI**：`.github/workflows/ci.yml` は `ubuntu-latest` で Go 1.23.0 と安定版 Go を使い、ユニットテスト、race テスト、lint、`govulncheck` を実行する。同じ runner で Docker 統合テストの行列も実行する。Apple Container 統合テストは意図的にローカル専用である。GitHub の Linux runner には Apple Container サービスと必要なホスト環境がない。ローカルでは `CONTAINERGO_BACKEND=apple` または `CONTAINERGO_BACKEND=docker` でバックエンドを 1 つだけ実行できる。

## バックエンド（v0.2）

v0.1 は Apple Container 専用だった。
v0.2 で Docker バックエンドを追加し、Linux と Windows でも同じ API を使う。

**選択**：`CONTAINERGO_BACKEND` 環境変数を最優先し、`apple` または `docker` を受け付ける。未指定なら OS で決め、macOS は Apple Container、Linux と Windows は Docker になる。macOS で Docker Desktop を使う場合は `CONTAINERGO_BACKEND=docker` を設定する。

**方式**：Docker も `os/exec` の CLI ラッパーにする。container-rs は Docker Engine API を直接呼ぶが、本ライブラリは直接 API を使わない。直接 API クライアントは tar の生成、ログストリームの逆多重化、レジストリ認証、Windows の名前付きパイプを自前実装することになるため、CLI ラッパーを使う方が既存の runner 層（argv 実行、timeout、streaming）を共有できる。`DOCKER_HOST`、Docker context、認証の解決は docker CLI に委ねる。ただし、このライブラリが remote endpoint として検出するのは `DOCKER_HOST` だけで、Docker context の remote daemon は検出しない。

**内部構造**：バックエンドは argv 組み立てと inspect 正規化だけを持つ内部 interface にする。プロセス実行（runner）、待機戦略、cleanup、検証は両バックエンドで共有する。正規化した記録には state（running / stopped / stopping / created / unknown へ写像）、label、image 参照、利用できる場合の不変 backend ID、コンテナ IP、host 側 port binding（コンテナポートから host アドレスとポートへ）を持つ。

**Image 処理**：両バックエンドとも、明示的な image inspect と pull コマンドで pull policy を実装する。Docker の run argv は `--pull=never` を追加し、Apple Container も同じ明示的な policy 経路を使う。並行 pull は同一プロセス内で、backend、image、platform、操作が同じ場合だけ集約する。

**接続エンドポイントの違い**：Docker Desktop（macOS / Windows）ではホストからコンテナ IP に到達できないため、Docker バックエンドは testcontainers と同じ公開ポート方式を既定にする。
`WithExposedPorts` で宣言したポートは自動公開される。
ローカルでは `-p 127.0.0.1::<port>`、リモートデーモン（`DOCKER_HOST=tcp://host`）ではクライアントが到達できるように `-p 0.0.0.0::<port>` を使う。
`Host` は `127.0.0.1`（`tcp://` の `DOCKER_HOST` ならそのホスト）、`MappedPort` は割り当てられたホストポートを返す。
loopback と unspecified の binding は `defaultHost()` に読み替えるため、リモートデーモンで観測した `127.0.0.1` binding もリモートホストとして解決する。
リモートデーモンで loopback を明示した `WithPublishedPort` は `Run` が拒否する。
Docker はリモートマシンの loopback でしか listen せず、クライアント側の書き換えでは到達できないためである。
ライブラリがリモートデーモンを検出するのは `DOCKER_HOST` だけで、リモートデーモンを指す `docker context` は検出しない。
デーモンが起動時にポートを原子的に割り当てるため、Apple Container で避けた空きポートの競合は再び起こらない。
Apple バックエンドの直接 IP の既定は変えない。

**Cleanup の違い**：watchdog reaper は backend ごとに delete subcommand を切り替える（Apple は `delete --force`、Docker は `rm --force`）。reaper は `/bin/sh` に依存するため Windows では動かず、Windows は通常の cleanup 経路（`Cleanup`、rollback）に依存する。`Prune` は Docker で daemon 側 filter（`--filter label=... --filter status=exited`）を利用できる。

**liveness detection**：probe command は backend ごとに切り替える（Apple は `system status`、Docker は `version --format {{.Server.Version}}`）。

## スコープ外

- `container build` / `docker build` による Dockerfile ビルド。
- ネットワークの作成と管理。`WithNetwork` は既存の名前付きネットワークへ接続できるが、ライブラリはネットワークを作らない。
- ボリュームの作成とライフサイクル管理。`WithMounts` は bind、名前付きボリューム、tmpfs を使えるが、ライフサイクルは呼び出し元が管理する。
- postgres などの testcontainers module に相当する高水準パッケージ（コアが安定してから再検討）。
- Docker Engine API の直接クライアント。現在の transport は CLI ラッパーであり、直接クライアントは隠れた fallback ではなく将来的な決定事項である。

## 未解決の製品上の決定

現在の API が暗黙に意味を与えない事项を、ここに明記する。

- **Apple の名前ベース削除**：作成世代チェックは同じ host で本ライブラリを使う協調プロセスを保護するが、同じ窓で外部の CLI が delete して再作成した場合には区別できない。この制約を解消するには backend の不変 ID または原子的な条件付き削除が必要である。
- **remote Docker の検出**：endpoint 選択に使うのは `DOCKER_HOST=tcp://...` だけである。remote Docker context は検出しない。
- **Reuse の互換性**：image と port は比較し、`env`、`cmd`、`mounts` の差は意図的に attach する。今後の release で設定を比較すべきかは未解決の製品上の決定であり、分離が必要なら別の名前を使う。
- **logger injection**：公開 logger hook は存在しない。追加するには新しい API と、公開できるコマンドデータの範囲を決める決定が必要である。
- **リーパー登録**：登録はベストエフォートで、現在は名前優先の検証を使う。登録に失敗しても無視され、通常の cleanup が残る。バックエンド ID の受理範囲を広げるかは未解決である。
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

- [apple/container](https://github.com/apple/container) v1.2.x–v1.3.x のコマンドリファレンスと `ContainerResource` ソース。
- [shiguredo/container-rs](https://github.com/shiguredo/container-rs)：XPC 直結方式の先行実装。watchdog reaper、cleanup contract、macOS 固有の制約（ポート競合、転送切断）の整理を参考にした。
- [testcontainers-go](https://github.com/testcontainers/testcontainers-go) v0.44.0：API 形状（functional options、wait 戦略、nil 安全な cleanup）の参照元。
