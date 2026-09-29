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
ランダムポートへ自動公開されます(testcontainers と同じモデル)。ローカルはループバック(`-p 127.0.0.1::<port>`)、リモートの `DOCKER_HOST` では全IF(`-p 0.0.0.0::<port>`)に束縛します。リモートとは、このマシンを指さないあらゆる `DOCKER_HOST` のことです。`tcp://host`(CI での `tcp://docker:2375` など)、`ssh://user@host`、スキームなしの `host:port` / ホスト名(Docker CLI と同じく `tcp://` を前置)が該当し、`unix://`、`npipe://`、ループバックアドレス、空値はローカルのままです。Docker CLI が受け付けるクライアントプロトコルは `unix`、`tcp`、`npipe`、`ssh` であり、それ以外のスキームは利用可能な `DOCKER_HOST` ではありません。`Host` は `127.0.0.1`(リモート時はそのホスト名)、`MappedPort` は割り当てられたポートを返します。リモートデーモンでは、ループバック(`127.0.0.1:...`、`[::1]:...`)を明示した `WithPublishedPort` はリモート側でしか待ち受けられないため拒否します。`ssh://` のホスト名は直接 dial 可能でなければなりません。CLI の SSH セッションが運ぶのは Docker API だけで、公開ポートは運ばれないため、ProxyJump や踏み台越しでしか届かないエイリアスは手動の `ssh -L` 転送が必要です。`docker context` 経由のリモート指定は検知しません。割り当ては
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
wait.ForExposedPort()                        // 最初に宣言した TCP ポート
wait.ForHTTP("/health")                      // 既定は最初に宣言した TCP ポート
wait.ForExec([]string{"pg_isready"})         // .WithExitCodeMatcher
wait.ForAll(...), wait.ForAny(...)           // 合成; .WithStartupTimeout
```

ポートの暗黙選択は `WithExposedPorts` を宣言順に走査し、最初の TCP
ポートを使います。公開済みの TCP ポートは、公開宣言された TCP ポートが
ない場合だけ調べます。そのため、別のポートを `WithPublishedPort` で公開
しても、公開宣言の順序は変わりません。
`/tcp` を省略したポート仕様は検証時に TCP として正規化します。
`080` と `80` のような数値として等しい表記も、同じ宣言と一致します。

各 leaf 戦略の `WithStartupTimeout` は 0 なら 60 秒、
`WithPollInterval` は 0 なら 100 ミリ秒です(`ForExec` のみ 250 ミリ秒)。
`ForLog.WithPollInterval` は、logs ストリームが正常に EOF した後の
再接続間隔です。`logs --follow` が非 0 で終了した場合は終端エラーとして
再接続せず、一致行があっても readiness を満たしません。
`WithOccurrence(n)` の `n` は 1 以上で、行単位で数えます。再接続時に
履歴の共通接頭辞を重複排除してから、新しく観測した行だけを加算します。
行末は `bufio.ScanLines` と同じ扱いとし、clean EOF に最後の行末がない場合も
完全な行として数えます。
再生状態と出現回数を確定するのは、clean EOF まで読み切った走査だけです。
この契約は `FollowLogs` が追記専用の履歴を再接続ごとに再生することを
前提とし、位置が異なる同一行は別イベントとして数えます。
再生済み接頭辞の行数と、上限を定めた行フィンガープリントのローリングウィンドウだけを保持します。
観測したログ量が増えても、待機のメモリ使用量は増えません。
`ForAll` / `ForAny` は入れ子の戦略を再帰的に検証します。合成の
`WithStartupTimeout` は正の値なら全体の上限、0 または負の値では従来の
互換契約どおり無制限です。

`Run` は、宣言・公開したポートを含めて待機戦略ツリー全体を、
イメージの照会・pull より前に検証します。不正な設定は
`ErrInvalidConfiguration`(`container.ErrInvalidConfiguration`
としても参照可能)、未宣言ポートと存在しないコンテナはそれぞれ
`ErrPortNotExposed` と `ErrContainerNotFound` で返ります。恒久的な設定・
対象エラーは即座に失敗し、transient な probe の原因、呼び出し元の
`context.Canceled`、または `context.DeadlineExceeded` は同じエラーチェーン
に残ります。EOF と最後の状態照会が呼び出し元・起動予算を超えて待つこと
はありません。各 leaf 戦略は成功を返す前に、既存予算内の最後の状態照会を
1回行います。待機に失敗した場合はロールバック削除し、ログ末尾をエラーに
添付します。

### ログのストリーミング

`FollowLogs` はバックエンド CLI の起動後に `io.ReadCloser` を返します。
EOF まで `Read` してください。CLI の起動後に異常終了した場合、終了エラーと
`CLIError` の詳細は `FollowLogs` ではなく `Read` から返ります。
`Close` またはコンテキスト取消は意図的な終了です。
その後の `Read` が EOF またはコンテキストエラーを返すことがあります。

ライブラリは直接の CLI 子プロセスを必ず待って回収します。
Unix 系ではその子のプロセスグループに残る子孫の終了も最大限試しますが、子孫を回収することはありません。
子孫がゾンビになった場合の回収は、OS の init または subreaper の責務です。
直接の子が既に終了して回収済みなら、`Close` は元のプロセスグループに信号を送らないため、子孫が生き残ることがあります。
デタッチされた子孫や、再親付けされた子孫はプロセスグループの保証外です。
Windows では保持したプロセスハンドルで直接の子だけを終了させ、子孫への境界は提供しません。
CLI がヘルパーをデタッチする設計の場合は、子孫の終了を保証できません。

## クリーンアップの契約

コンテナがテストより長生きしないよう、3 層の仕組みがあります。

1. `container.Cleanup(t, ctr)` は `t.Cleanup` 経由で削除を登録します。
   defer 派には `container.TerminateContainer(ctr)` があります。どちらも
   nil 安全なので、`Run` のエラーチェックより前に呼べます。
2. `Run` が途中で失敗した場合は、`Run` 自身が作成済みリソースを削除して
   から返ります。
3. watchdog リーパー(外部の `/bin/sh` 子プロセス)が、テストプロセスが
   どのように死んでも(SIGKILL やパニックを含む)登録済みコンテナを強制
   削除します。リーパーは `/bin/sh` を必要とするため Windows では動かず、
   Windows では前 2 層のみでクリーンアップします。

補足:

- `CONTAINERGO_KEEP=1` でコンテナを残せます(デバッグ用)。
- `container.Prune(ctx)` は過去セッションを含め、本ライブラリが作成した
  停止済みコンテナ(`com.github.hirokazumiyaji.container-go` ラベル付き)
  を削除します。

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
- stopped の残骸は削除して再作成する。running のまま ready にならない
  場合は削除せずエラーを返す。
- image / port が既存と不一致なら分かりやすいエラーを返す。互換性チェックは image と port のみが対象。`env` / `cmd` / `mounts` の差は既存へ黙って attach する仕様。
- 各作成は世代ラベルを持ち、`Terminate` と stopped 再作成経路は置き換わった世代の削除を拒否する。watchdog リーパーも同様にガードする。
- `Cleanup` / `TerminateContainer` / watchdog リーパーは reused ハンドルを
  削除しない。明示的な `ctr.Terminate` だけが共有コンテナを消し得る。
- `container.PruneReuseGroup(ctx, "integration")` はそのグループの
  コンテナを強制削除する(CI 終了時)。通常の `Prune` は stopped のみ。

ライブラリはテスト間のアプリケーションデータを自動初期化しません。
キー接頭辞、スキーマ分離、`Exec` による reset(`FLUSHALL` 等)を使って
ください。

## セキュリティ上の注意

- すべての CLI 呼び出しは argv 配列で行い、シェルを経由しません。唯一の
  シェルスクリプト(リーパー)は固定文字列で、コンテナ ID は検証済みの
  stdin データとしてのみ渡ります。
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
