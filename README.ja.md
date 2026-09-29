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
(既定 100 ミリ秒)を持ちます(`ForAll` / `ForAny` は `WithStartupTimeout` で合成全体のタイムアウトを設定可)。
待機中にコンテナが停止すると即座に失敗し、待機に失敗した場合はログ末尾をエラーに添付します。
非 reuse の `Run` では既定でコンテナをロールバック削除しますが、`CONTAINERGO_KEEP=1` の場合は残します。
`WithReuse` の `Run` は、待機に失敗しても共有コンテナをロールバックしません。

## クリーンアップの契約

コンテナがテストより長生きしないよう、3 層の仕組みがあります。

1. `container.Cleanup(t, ctr)` は `t.Cleanup` 経由で best-effort の削除を
   登録します。`container.CleanupStrict(t, ctr)` を使うと、削除失敗を
   テスト失敗として報告します。defer 派には
   `container.TerminateContainer(ctr)` があります。いずれも nil 安全なので、
   `Run` のエラーチェックより前に呼べます。
2. 非 reuse の `Run` では、既定で途中まで作成したリソースを削除してから返ります。
   削除にも失敗した場合は、返された `*CleanupError` から元の error とクリーンアップ error の両方を取得できます。
   これは作成失敗、コピー、待機のロールバック経路に適用されます。
   `WithReuse` の `Run` は共有コンテナを強制削除しません。create の時点で
   共有コンテナは公開済みで、peer が既に引き継いでいる可能性があるためです。
   create 後の失敗と作成失敗後のクリーンアップは、再 inspect で
   「この `Run` が所有し、peer がまだ引き継いでいない」
   (`created` / `stopped`)ことを確認できた場合にだけ削除します。
   `running` など曖昧な状態はそのまま残し、拒否理由を `*CleanupError` で返します。
3. watchdog リーパー(外部の `/bin/sh` 子プロセス)が、テストプロセスが
   どのように死んでも(SIGKILL やパニックを含む)登録済みコンテナを強制
   削除します。検証済みの非 reuse 作成失敗候補は、自動削除に失敗した場合
   watchdog に登録し、削除が成功したら登録を取り下げます。
   共有コンテナと保持(CONTAINERGO_KEEP)コンテナは watchdog の所有にしません。
   リーパーは `/bin/sh` を必要とするため Windows では動かず、
   Windows では前 2 層のみでクリーンアップします。

`CONTAINERGO_KEEP=1` はプロセス全体の診断用スイッチです。
`Cleanup` / `TerminateContainer`、作成失敗後のクリーンアップ、コピーや待機のロールバックを自動的に行わず、watchdog の登録も無効にします。
`WithReuse` の `Run` は、待機に失敗しても共有コンテナをロールバックしません。
非 reuse の `Run` がコンテナを作成し、所有ラベルを検証できた場合は、失敗エラーとともに非 nil の部分的な `*Container` を返します。
自動生成した名前や copy CLI を呼ぶ前の失敗でも同じです。
`WithReuse` の create 失敗では同じ所有権検証に成功した場合だけ保持 handle を返します。
`CONTAINERGO_KEEP=1` の待機失敗では、共有 handle も返します。
この handle は共有コンテナを参照します。
同一プロセスの呼び出しは同じ handle を受け取る可能性があり、別プロセスの呼び出しも同じコンテナを参照します。
この handle は呼び出し元だけが所有する handle ではありません。
返した handle は `State`、`Logs`、`Exec`、`CopyToContainer`、明示的な `Terminate` に利用できます。
共有コンテナの明示的な終了は、そのコンテナを使うすべての呼び出し元に影響します。
保持 handle は返す直前に再検証します。generation を証明できなくなった場合は
handle を nil とし、検証失敗を元のエラーと結合して返します。
コンテナが存在しない場合、または所有権を検証できない場合は nil を返します。
明示的な `Container.Terminate`、`Prune`、`PruneReuseGroup` は引き続き削除を行います。
reuse の停止済みコンテナ置き換えでは、同じ名前ロック内で再 inspect し、
停止済みでない、または generation が一致しない場合は削除しません。

補助 API:

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
- 名前競合や Apple の create race は通常、attach または再試行へ進む。
  `CONTAINERGO_KEEP=1` の場合で、この `Run` が名前付きコンテナを自分が作成した所有ラベルから検証できるときは、元のエラーと保持した共有 handle を返す。
  所有性を検証できない peer は取得しない。
- stopped の残骸は削除して再作成する。running のまま ready にならない場合は削除せずエラーを返す。
- image / port が既存と不一致なら分かりやすいエラーを返す。互換性チェックは image と port のみが対象。`env` / `cmd` / `mounts` の差は既存へ黙って attach する仕様。
- 各作成は世代ラベルを持ち、`Terminate` と stopped 再作成経路は置き換わった世代の削除を拒否する。watchdog リーパーも同様にガードする。
- `Cleanup` / `TerminateContainer` / watchdog リーパーは reused ハンドルを削除しない。
  保持された失敗 handle を含め、すべての handle が同じ共有コンテナを参照する。
  明示的な `ctr.Terminate` はすべての利用者に影響するため、他の利用者が不要と確認できる場合だけ実行する。
- `container.PruneReuseGroup(ctx, "integration")` はそのグループのコンテナを強制削除する(CI 終了時)。通常の `Prune` は stopped のみ。

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
