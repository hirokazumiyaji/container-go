# container-go 設計ドキュメント

English (primary): [design.md](design.md)

作成日: 2026-08-18(v0.2 バックエンド節を 2026-08-19 追記)
対象: Apple Container v1.2.x(macOS 26 以降、Apple Silicon)、Docker(Linux、Windows、macOS)、Go 1.23 以降

## 目的

**container-go** は、Apple Container([apple/container](https://github.com/apple/container))を実行基盤とする、testcontainers スタイルの Go ライブラリである。
Go のテストコードから使い捨てのコンテナを起動し、接続情報を取得し、テスト終了時に確実に破棄することを目的とする。

先行事例として、Rust には [shiguredo/container-rs](https://github.com/shiguredo/container-rs) がある。
本ライブラリは同じ問題領域を Go で扱うが、実現方式は後述のとおり異なる。

設計上の制約は次の三つである。

- **依存ゼロ**：サードパーティの Go モジュールに依存しない。標準ライブラリのみで実装する。
- **セキュリティ**：外部プロセス起動と入力値の扱いにおいて、インジェクションと情報漏洩の経路を作らない。
- **パフォーマンス**：テストスイートの実行時間を支配するのはコンテナ(VM)の起動時間である。ライブラリ側のオーバーヘッドをそれに対して無視できる水準に保ち、並列起動を妨げない。

## 前提とする Apple Container の仕様

設計の根拠となる Apple Container(v1.2.x–1.3.x で確認。フィクスチャは 1.2.2 と 1.3.0)の仕様を先に整理する。

- ホスト要件は macOS 26 以降かつ Apple Silicon である。
- 各コンテナは軽量 VM として起動し、vmnet ブリッジ(既定は `default`、`192.168.64.0/24`)上の実 IP を持つ。ホストはこの IP に直接到達できるため、ポート公開(`--publish`)は必須ではない。
- すべての操作は `container` CLI から行える。`ls --format json` と `inspect` は機械可読な JSON を返す(追加フィールドは `internal/inspect` が無視する)。
- CLI は launchd 配下の `container-apiserver` と XPC で通信する。サービスが未起動だとコマンドは失敗する。起動状態は `container system status` で確認できる。
- コンテナ名がそのまま ID になる。名前は `^[a-zA-Z0-9][a-zA-Z0-9_.-]+$` かつ 63 文字以内でなければならない。
- Docker にある次の機能が存在しない：ヘルスチェック、`wait` コマンド、イベントストリーム、`ls` のラベルフィルタ、実行中コンテナへの再アタッチ。これらに相当する挙動はクライアント側で実装する必要がある。
- `--label` はあるがフィルタは JSON 出力をクライアント側で絞り込むしかない。ラベルキーは小文字英数字とハイフン、ドット区切りの Docker/OCI 形式に限られる。
- `container cp` は実行中のコンテナに対してのみ使える。
- `--rm` で削除しても匿名ボリュームは残る。
- エラー分類は `engine_apple.go` が持つ CLI stderr 部分文字列に依存する(名前衝突、image/container missing)。ライブ CLI に対する回帰は `cli_compat_integration_test.go` で確認する。

## 実現方式の選定

実現方式には二つの候補がある。

- **CLI ラッパー方式**：`os/exec` で `container` CLI を子プロセスとして起動し、JSON 出力を解釈する。
- **XPC 直結方式**：container-rs が採る方式で、`container-apiserver` の XPC サービスを C ブリッジ経由で直接呼ぶ。

本ライブラリは CLI ラッパー方式を採用する。
理由は次の三点である。

第一に、依存ゼロ要件との整合である。
XPC 直結方式は cgo と自前の C ブリッジを必要とし、ビルドに macOS SDK が絡む。
CLI ラッパー方式は純 Go・標準ライブラリのみで完結し、`CGO_ENABLED=0` でもビルドできる。

第二に、安定性である。
XPC のルート名やメッセージ構造は Apple Container の内部実装であり、互換性の保証がない。
CLI はユーザー向けの公開インターフェースであり、JSON スキーマも Swift の公開ソースで確認できる。

第三に、性能上の差が問題にならないことである。
子プロセス起動のコストは 1 呼び出しあたり数十ミリ秒程度で、コンテナ(VM)起動の数秒に対して十分小さい。
テスト用途では XPC 直結による短縮効果は体感できない。

CLI ラッパー方式の弱点は、CLI のバージョン間で出力形式が変わりうることと、CLI が公開していない機能(ラベルフィルタなど)を使えないことである。
前者は照会系をすべて `--format json` に限定し、テキスト出力のパースを行わないことで影響を局所化する。
後者はクライアント側フィルタで代替する。

## 公開 API

API の形は testcontainers-go(v0.44 系)の新 API に寄せる。
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
    ...
}
```

### Run とオプション

```go
func Run(ctx context.Context, image string, opts ...Option) (*Container, error)
```

`Run` はイメージの取得(未取得なら CLI が自動 pull する)、コンテナ作成、起動、待機戦略の完了までを行い、失敗時は作成済みリソースをロールバック削除してからエラーを返す。

オプションは functional options で提供する。
初期リリースで提供するものを挙げる。

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
- `WithNetwork(name string)`：接続先ネットワーク
- `WithPlatform(p string)`：`linux/amd64` 指定(Rosetta 利用)など

`WithHostname` や `WithPrivileged` などのオプションは、Apple Container CLI に対応するフラグが存在しないため意図的に提供しない(両バックエンド共通でサポート可能な機能に限定する方針)。ログ転送には `FollowLogs` を直接利用する。

### Container ハンドル

```go
type Container struct { ... }

func (c *Container) ID() string
func (c *Container) Host(ctx context.Context) (string, error)
func (c *Container) MappedPort(ctx context.Context, port string) (int, error)
func (c *Container) Endpoint(ctx context.Context, port string) (string, error)
func (c *Container) ContainerIP(ctx context.Context) (string, error)
func (c *Container) State(ctx context.Context) (State, error)
func (c *Container) Exec(ctx context.Context, cmd []string, opts ...ExecOption) (int, io.Reader, error)
func (c *Container) Logs(ctx context.Context) (io.ReadCloser, error)
func (c *Container) CopyToContainer(ctx context.Context, hostPath, containerPath string) error
func (c *Container) CopyFileFromContainer(ctx context.Context, containerPath string) (io.ReadCloser, error)
func (c *Container) Stop(ctx context.Context, timeout *time.Duration) error
func (c *Container) Terminate(ctx context.Context) error
```

`Terminate` は `container delete --force` に対応し、冪等である(既に存在しない場合も成功扱い)。
`Cleanup(t, ctr)` と `TerminateContainer(ctr)` は nil 安全なヘルパーで、testcontainers-go と同じく「エラーチェックの前に defer できる」使い方を保証する。

## 接続エンドポイントの設計

testcontainers の Docker 実装では、コンテナポートをホストのランダムポートへ publish し、`localhost:<mapped>` へ接続する。
Apple Container ではこの方式を既定にしない。

既定では、`Host` はコンテナの実 IP(inspect の `status.networks[0].ipv4Address` から CIDR サフィックスを除いたもの)を返し、`MappedPort` はコンテナポートをそのまま返す。
この方式を既定とする理由は三つある。

- Apple Container にはランダムポート割り当てがなく、ホストポートを自前で確保すると「空きポートを探してから起動するまで」の競合が避けられない(container-rs も同じ競合を既知の制約として抱えている)。直接 IP 接続ならホストポートを一切消費しないため、この競合が存在しない。
- ホストポートの衝突がないため、テストの並列実行を無制限にスケールできる。
- ポート転送プロキシを経由しないため、転送実装の不具合(大きな転送が途中で切断される事例が報告されている)の影響を受けない。

`localhost` 固定の接続先が必要な場合(コンテナ IP へ到達できない環境や、接続文字列に localhost を要求するクライアント)に限り、`WithPublishedPort("127.0.0.1:15432:5432")` で明示的に公開する。
公開した場合、`Host` は指定したホストアドレスを、`MappedPort` はホストポートを返す。

`MappedPort` は `WithExposedPorts` で宣言されていないポートに対してエラーを返す。
宣言は待機戦略(ForListeningPort の既定ポートなど)にも使う。

## 待機戦略

Apple Container にはヘルスチェックも wait コマンドもないため、起動完了の判定はすべてクライアント側で行う。
`wait` サブパッケージに次の戦略を実装する。

- `wait.ForLog(s string)`：`container logs --follow` の出力に部分文字列(または `AsRegexp` で正規表現)が現れるまで待つ。`WithOccurrence(n)` で出現回数を指定できる
- `wait.ForListeningPort(port string)`：コンテナ IP の対象ポートへ `net.DialTimeout` が成功するまで待つ
- `wait.ForHTTP(path string)`：`net/http` で対象ポートへリクエストし、ステータスコード(既定 2xx、`WithStatusCodeMatcher` で変更可)を満たすまで待つ
- `wait.ForExec(cmd []string)`：`container exec` の終了コード(既定 0)を満たすまで待つ
- `wait.ForAll(ss ...Strategy)` / `wait.ForAny(ss ...Strategy)`：合成。`WithStartupTimeout` で合成全体のタイムアウトも設定可能

すべての戦略は `WithStartupTimeout`(既定 60 秒)と `WithPollInterval`(既定 100 ミリ秒)を持つ。
待機中にコンテナが停止状態へ遷移した場合は、タイムアウトを待たずに失敗とし、診断用にログ末尾(上限 1MiB)を添えてエラーを返す。

戦略のインターフェースは次のとおり。

```go
type Strategy interface {
    WaitUntilReady(ctx context.Context, target Target) error
}
```

`Target` はコンテナ IP、宣言済みポート、ログリーダー、exec、状態照会を提供する小さなインターフェースで、`*container.Container` が実装する。
`wait` パッケージが `container` パッケージへ依存しない向きに保ち、循環参照を避ける。

## クリーンアップ

テストプロセスの終了パターンごとに、コンテナが確実に削除される経路を用意する。

**正常経路**：`Cleanup(t, ctr)` が `t.Cleanup` 経由で `Terminate` を呼ぶ。
`Run` の途中失敗時は `Run` 自身がロールバック削除を行う。

**異常終了経路(SIGKILL、パニック、`os.Exit`)**：Go の defer も t.Cleanup も走らないため、外部プロセスによる**watchdog リーパー**を用意する。
ライブラリ初期化時に `/bin/sh` の子プロセスを一つ起動し、標準入力のパイプ越しにコンテナ ID を登録する。
親プロセスがどのような形で死んでもパイプは EOF になるので、リーパーはそれを契機に登録済み ID へ `container delete --force` を実行して自身も終了する。
テストプロセス生存中はリーパーは何もしない(削除は通常経路が担い、リーパーは保険である)。
この方式は container-rs の watchdog と同じで、シグナルハンドラでは捕捉できない SIGKILL にも対応できる。

**セッションラベル**：作成する全コンテナに次のラベルを付与する。

- `com.github.hirokazumiyaji.container-go`：`true`(本ライブラリ管理下の印)
- `com.github.hirokazumiyaji.container-go.session`：プロセスごとの乱数 ID

CLI にラベルフィルタがないため、孤児の掃除は `container ls -a --format json` をクライアント側でフィルタして行う。
この掃除を行うヘルパー `Prune(ctx)` (自セッション以外も含め、本ライブラリのラベルを持つ停止済みコンテナを削除する)を提供する。

環境変数 `CONTAINERGO_KEEP=1` を設定した場合、`Cleanup` とリーパーは削除を行わない(デバッグ用)。

匿名ボリュームは `--rm` でも残る仕様のため、本ライブラリは匿名ボリュームを作らない。
ボリュームが必要な場合は名前付きで作らせ、ライフサイクルは利用者に委ねる。

## Apple Container の名前ロック

Apple Container ではコンテナ ID が名前であるため、`inspect` と `delete` の間で同じ名前が再生成される可能性がある。
生成ラベルの検証と削除は、名前ごとの `flock` の下で実行する。
名前が同じで、世代がないハンドルや `inspect` 結果の世代欠落は name ベース削除をしない。
create、inspect、prune、delete は、検証済みの固定ユーザー state 名前ロックを使う。
Docker は検証済み immutable ID でのみ削除し、名前ロックを取得しない。
新しい実装は、親リビジョンの `TMPDIR` ロック、UserCacheDir を使う初版ハードニングのロック、namespace maintenance ロック、アカウント情報から導いた固定 state ディレクトリのロックを、この順で取得する。
`XDG_STATE_HOME` や `HOME` で同じアカウントの lock namespace を分けたりはしない。
移行用ロックの解決または取得に失敗した場合は、臨界領域へ入らず互換性エラーを返す。
古い実装が異なる `TMPDIR` を使う場合、旧実装と新しい実装の historical path は一致しないため、mixed-revision の保証はその組み合わせには適用されない。rollout は段階的に行う。
state のファイル名は名前の SHA-256 ダイジェストであり、`TMPDIR` や cache が異なっても新しい実装彼此は同じ state inode を使う。
すべてのロックファイルは `O_NOFOLLOW` で開き、ファイル種別、所有者、`0600`、path と open fd の inode 一致を `flock` の前後で確認する。
reaper 登録時には 4 つの barrier すべてに durable な hard-link lease を作り、age/cap cleanup から保護する。shell は cleanup で置換可能な元 path ではなく、検証済み lease path の device/inode を `lockf` でロックする。
state ディレクトリは所有者と置換可能性を検索し、sticky bit を持つ標準の temporary root は sticky 規則で保護されるため受け入れる。

最近使用した state ロックファイルは保持し、他の協力プロセスが保持する inode は unlink しない。
namespace の maintenance `flock` は cleanup の sweep が終わるまで保持し、maintenance を保持しながら state lock を待つ順位逆転を防ぐ。
cleanup は 1 回の取得ごとに最大 258 件（name-lock 256 件と maintenance 1 件）を調べ、最大 32 件だけを削除する。
7 日より古いファイルは削除対象であり、256 件の上限を超えた場合は他のプロセスが使用していないファイルなら早く削除できる。
reaper lease がある inode は age/cap cleanup から除外し、lease が壊れていれば安全側 fail closed とする。
非 blocking exclusive `flock` を取得できない候補は skip するため、保持中の inode は cleanup 対象にならない。
旧実装と併存できるあいだは、移行用ロックファイルを cleanup しない。

外部の `container delete` と再作成は、この lock を使わず、名前だけでは検出できない。
`Prune` と `PruneReuseGroup` は list 時の generation、managed label、group、state を保存し、name lock 内で fresh inspect して再検証する。
watchdog reaper は Apple の inspect/delete 間 동안 legacy、transitional、maintenance、durable の 4 つの lease lock を `lockf` で同じ順序に保持し、inspect 前後に device/inode を再確認する。helper、lease、barrier のいずれかがない場合は fail closed して削除しない。Docker の ID 経路は lock-free。
`Terminate` と `TerminateContainer` は、各段階へ 30 秒ずつ割り当てるのではなく、lock 取得、inspect、delete を一つの既定 30 秒の aggregate budget で実行する。
`cleanupFailedCreate` も lock 取得と backend 処理に一つの 30 秒 budget を使う。
呼び出し元が指定した短い deadline は、この aggregate budget を上書きしない。
lock や directory の失敗は `Run` のエラーへ join し、rollback の cleanup error も `%w` で保持する。

## セキュリティ設計

外部プロセス起動を伴うライブラリとして、次の原則を守る。

**シェルを経由しない**。
すべての CLI 呼び出しは `exec.Command` に引数配列を渡す形で行い、シェル文字列を組み立てない。
唯一の例外は watchdog リーパーのシェルスクリプトである。
ここはスクリプト本文を固定文字列とし、コンテナ ID、4 つの lease path、device/inode identity は検証済みの標準入力データとして渡す。
スクリプト側は `set -f`(グロブ無効)、`IFS=` と `read -r`、変数のクォートで語分割とグロブ展開を封じ、ライブラリ側は ID を Apple Container の名前規則 `^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$` で検証してからパイプへ書く。
二重の防御により、ID 経由のコマンド注入を成立させない。

**環境変数を argv に載せない**。
`--env key=value` を使うと、値がプロセス一覧(`ps`)から他ユーザーにも見える。
データベースのパスワードなど秘密情報が環境変数で渡される用途が主であるため、環境変数は `os.MkdirTemp` 配下にパーミッション 0600 で書いた一時ファイルに格納し、`--env-file` で渡して起動後に削除する。

**入力を検証する**。
コンテナ名は前述の名前規則、ラベルキーは CLI と同じ Docker/OCI 形式、ポートは数値範囲とプロトコル(`tcp`/`udp`)、環境変数キーは `=` と NUL を含まないこと、コピー対象のパスは絶対パスかつ有効な UTF-8 であることを、CLI へ渡す前に検証する。
CLI 側にも検証はあるが、ライブラリ側で先に落とすことでエラーメッセージを明確にし、将来の CLI 側検証の変化にも依存しない。

**認証情報を扱わない**。
レジストリ認証は `container registry login`(資格情報は macOS Keychain に保存される)に委ね、本ライブラリは資格情報の入力経路を持たない。

**ログに秘密を書かない**。
デバッグログ(`WithLogger` で注入)に CLI の argv を出す場合、env-file の中身は出力しない。

## パフォーマンス設計

**子プロセス数を最小にする**。
作成と起動は `container run --detach` の 1 回で行う。
起動後に不変な情報(設定、ラベル、公開ポート)は初回の inspect 結果をキャッシュし、状態(`status.state`)のみ毎回取得する。

**待機を接続確認で行う**。
ForListeningPort と ForHTTP は CLI を呼ばず、コンテナ IP へ直接 TCP/HTTP 接続する。
ポーリングのたびに子プロセスを起動するのは ForExec と状態照会だけで、これらも 100 ミリ秒間隔のポーリングで問題ない程度に軽い。

**並列起動を妨げない**。
ライブラリ内にグローバルロックを置かない(watchdog リーパーへの ID 登録のみミューテックスで直列化するが、書き込みは 1 行で済む)。
ホストポートを消費しない既定設計により、並列数の上限はホストのリソースだけで決まる。

**ストリームを有限に保つ**。
`Logs` は `container logs --follow` の子プロセスを起動して `io.ReadCloser` として返し、`Close` またはコンテキスト取消で確実にプロセスを終了させる。
ForLog が診断用に保持するログは 1MiB を上限とする。

**すべての CLI 呼び出しに期限を付ける**。
各呼び出しは `context` を尊重し、既定タイムアウト(照会系 30 秒、pull を伴う run は 10 分)を持つ。
コンテキスト取消時は子プロセスへ SIGKILL を送って回収し、ゾンビとハングを残さない。

## エラー処理

エラーは `errors.Is`/`errors.As` で判別できる形で返す。

- `ErrSystemNotRunning`：CLI 呼び出しが失敗した際に `container system status` を追加で照会し、サービス未起動と判定できた場合に返す。メッセージに `container system start` の実行を促す文言を含める
- `ErrContainerNotFound`：inspect などの not found
- `ErrNameLockCompatibility`：移行用 lock namespace を確立できないため，名前指定の処理を実行しなかった
- `ErrPortNotExposed`：`WithExposedPorts` 未宣言のポート照会
- `*CLIError`：上記以外の CLI 失敗。実行したサブコマンド、終了コード、stderr(上限 64KiB)を保持する

`Run` が待機戦略のタイムアウトで失敗した場合は、コンテナのログ末尾を含むエラーを返してから、ロールバック削除を行う。

システムサービスの自動起動(`container system start` の代行)は行わない。
カーネルインストールの対話プロンプトを伴う場合があり、テストライブラリが暗黙に実行してよい操作ではないためである。

## パッケージ構成

```
container-go/
├── container.go      // Run、Container、Option
├── options.go        // functional options
├── cleanup.go        // Cleanup、TerminateContainer、Prune
├── reaper.go         // watchdog リーパー
├── exec.go           // Exec
├── logs.go           // Logs
├── copy.go           // CopyToContainer、CopyFileFromContainer
├── errors.go         // エラー型
├── internal/cli/     // CLI ランナー(コマンド組み立て、実行、タイムアウト)
├── internal/inspect/ // inspect JSON モデルとデコード
└── wait/             // 待機戦略
```

`internal/cli` のランナーはインターフェースとして定義し、テストではフェイク実装を注入する。

```go
type Runner interface {
    Run(ctx context.Context, args ...string) (stdout []byte, stderr []byte, err error)
}
```

## テスト戦略

**ユニットテスト**：`Runner` のフェイク実装(固定 JSON を返す)を注入し、コマンド組み立て、JSON デコード、エラー分類、待機戦略のロジックを実機なしで検証する。
本番コードが非 nil を前提とする依存には、テストでも必ず実体(フェイク)を渡す。

**統合テスト**：ビルドタグ `integration` で分離し、実機(macOS 26、Apple Container 起動済み)でのみ実行する。
起動、接続、exec、コピー、クリーンアップ、watchdog(子プロセスを SIGKILL してリーパーの動作を確認)を通しで検証する。
テスト冒頭で `container system status` を確認し、未起動なら skip する。

**CI**：ユニットテストと `go vet` はプッシュごとに GitHub Actions(macos ランナーで可、Apple Container 不要)で実行する。
統合テストは GitHub ホストランナーの macOS バージョンと nested virtualization の制約により動かない可能性が高いため、当面はローカル実行を前提とし、`make integration` として手順化する。

## バックエンド(v0.2)

v0.1 は Apple Container 専用だった。
v0.2 で Docker バックエンドを追加し、Linux と Windows でも同じ API でテストコンテナを使えるようにする。

**選択ルール**：環境変数 `CONTAINERGO_BACKEND` が最優先で、`apple` または `docker` を指定できる。
未指定の場合は OS で決まる(macOS は Apple Container、Linux と Windows は Docker)。
macOS で Docker Desktop を使いたい場合は `CONTAINERGO_BACKEND=docker` を設定する。

**実現方式**：Docker も CLI ラッパーとする(`docker` コマンドを `os/exec` で呼ぶ)。
container-rs は Docker Engine API を直接叩くが、本ライブラリでは採らない。
API 直叩きは tar 生成、ログストリームの逆多重化、レジストリ認証、Windows named pipe を自前実装する必要があり、CLI ラッパーで統一すれば既存のランナー層(引数配列実行、タイムアウト、ストリーミング)をそのまま共有できるためである。
`DOCKER_HOST` やコンテキスト、認証の解決は docker CLI 自身に委ねられる。

**内部構造**：バックエンドは「引数の組み立て」と「inspect 出力の正規化」だけを担う内部インターフェースにする。
プロセス実行(ランナー)、待機戦略、クリーンアップ、検証は両バックエンドで共有する。
正規化した情報は、状態(running / stopped / stopping / unknown への写像)、ラベル、コンテナ IP、公開ポートの束縛(コンテナポート → ホストアドレスとポート)の 4 つである。

**接続エンドポイントの違い**：Docker Desktop(macOS / Windows)ではコンテナ IP にホストから到達できないため、Docker バックエンドは testcontainers と同じ公開ポートモデルを既定とする。
`WithExposedPorts` で宣言したポートは自動的にランダムポートへ公開する(ローカルは `-p 127.0.0.1::<port>`、リモートデーモン(`DOCKER_HOST=tcp://host`)では `-p 0.0.0.0::<port>`)。`Host` は `127.0.0.1`(`DOCKER_HOST` が `tcp://` のときはそのホスト)、`MappedPort` は割り当てられたホストポートを返す。loopback/unspecified の束縛は `defaultHost()` に読み替える。リモートデーモンでループバックを明示した `WithPublishedPort` は、リモート側のループバックでしか待ち受けられずクライアント側の読み替えでは届かないため `Run` が拒否する。`docker context` 経由のリモート指定は検知できない。
ランダム割り当てはデーモンが起動時に原子的に行うため、Apple Container で避けた「空きポート確保の競合」は発生しない。
Apple Container バックエンドの既定(直接 IP)は変えない。

**クリーンアップの違い**:watchdog リーパーは削除サブコマンドをバックエンドごとに切り替える(Apple は `delete --force`、Docker は `rm --force`)。
リーパーは `/bin/sh` に依存するため Windows では動かない。
v0.2 の Windows は通常経路(`Cleanup`、ロールバック)のみとし、リーパーなしをドキュメントに明記する。
`Prune` は Docker ではデーモンのフィルタ(`--filter label=... --filter status=exited`)を使える。

**システム未起動の検出**:probe コマンドをバックエンドごとに切り替える(Apple は `system status`、Docker は `info`)。

## スコープ外

- `container build` / `docker build` による Dockerfile ビルド
- ネットワークの作成と管理(既定ネットワークのみ使う)
- ボリュームの作成と管理
- testcontainers のモジュール群(postgres など)に相当する高水準パッケージ(コア安定後に検討)
- Docker Engine API の直接クライアント(CLI ラッパーで足りなくなったら再検討)

## 実装フェーズ

実装は次の順に進める。
各フェーズを GitHub Issue として起票し、進捗を管理する。

1. プロジェクト基盤：go.mod、CI、Makefile
2. CLI ランナー層：`internal/cli`、タイムアウト、エラー分類、`ErrSystemNotRunning` 判定
3. inspect JSON モデル：`internal/inspect`
4. コア API：`Run`、オプション、`Container` のライフサイクル(起動、Stop、Terminate、ロールバック)
5. 接続情報 API：`Host`、`MappedPort`、`Endpoint`、`ContainerIP`、`WithPublishedPort`
6. 待機戦略：`wait` パッケージ一式
7. Exec、Logs、Copy
8. クリーンアップ：`Cleanup`、`Prune`、セッションラベル、watchdog リーパー
9. セキュリティ仕上げ：env-file 経由の環境変数、入力検証の網羅、ログのマスキング
10. 統合テストと手順化
11. ドキュメントとサンプル：README、使用例

v0.2(Docker バックエンド)は次の順に進める。

12. バックエンド抽象の導入：引数組み立てと inspect 正規化のインターフェース化、既存テストを green のまま Apple 実装へ切り出し
13. Docker エンジン実装：run / inspect / lifecycle / exec / logs / copy の引数と JSON パース
14. Docker の接続情報：ランダム公開ポート、`Host` / `MappedPort`、`DOCKER_HOST` 対応
15. バックエンド選択と周辺：`CONTAINERGO_BACKEND`、OS 既定、リーパーと Prune と probe の切替
16. Docker 統合テストとドキュメント更新

## 参考資料

- [apple/container](https://github.com/apple/container) v1.2.2 コマンドリファレンスおよび `ContainerResource` ソース
- [shiguredo/container-rs](https://github.com/shiguredo/container-rs)：XPC 直結方式の先行実装。watchdog リーパー、クリーンアップ契約、macOS 固有の制約(ポート競合、転送切断)の整理を参考にした
- [testcontainers-go](https://github.com/testcontainers/testcontainers-go) v0.44.0：API 形状(functional options、wait 戦略、nil 安全なクリーンアップ)の参照元
