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

Go 1.27 以上が必要です。

## インストール

```
go get github.com/hirokazumiyaji/container-go
```

## 接続エンドポイント

**Apple Container バックエンド**: 各コンテナは vmnet の `default`
ネットワーク上に実 IP を持ち、ホストから直接到達できます。既定ではこの IP
を使います。

- `Host` はコンテナ IP、`MappedPort` はコンテナポートそのまま、`Endpoint`
  はその組み合わせを返します。
- ホストポートを消費しないため、並列テストがポールで衝突しません。

**Docker バックエンド**: コンテナ IP にはホストから届かないことが多いため
(Docker Desktop)、`WithExposedPorts` で宣言したポートはデーモンが割り当てる
ループバックのランダムポートへ自動公開されます(testcontainers と同じ
モデル)。`Host` は `127.0.0.1`(`tcp://` の `DOCKER_HOST` 設定時はその
ホスト)、`MappedPort` は割り当てられたポートを返します。割り当ては
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
wait.ForHTTP("/health")                      // .WithPort、.WithMethod、.WithStatusCodeMatcher
wait.ForExec([]string{"pg_isready"})         // .WithExitCodeMatcher
wait.ForAll(...), wait.ForAny(...)           // 合成
```

すべての戦略は `WithStartupTimeout`(既定 60 秒)と `WithPollInterval`
(既定 100 ミリ秒)を持ちます。待機中にコンテナが停止すると即座に失敗し、
待機に失敗した場合はロールバック削除のうえ、エラーにログ末尾が添付されます。

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
- image / port が既存と不一致なら分かりやすいエラーを返す。
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
make integration         # 統合テスト一式。バックエンドがなければ各自 skip
make integration-docker  # Docker バックエンドの統合テストのみ
```

設計ドキュメント: [docs/design.md](docs/design.md)(日本語版:
[docs/design.ja.md](docs/design.ja.md))

## ライセンス

MIT
