# container-go

日本語版: [README.ja.md](README.ja.md)

A [testcontainers](https://testcontainers.com/)-style Go library for
[Apple Container](https://github.com/apple/container) and Docker: run
throwaway containers from Go tests, with zero third-party dependencies.

## Version scope

The latest tagged release is `v0.2.0` (2026-09-02). This checkout is
development after that tag. The install command below is for the released
API; it does not install the development-only APIs documented below.

The current checkout requires Go 1.23 or later. The `v0.2.0` module
requires Go 1.27 or later.

| API or behavior | `v0.2.0` | Current development checkout |
|---|---|---|
| `Run`, core options, lifecycle, endpoints, `Exec`, `Logs`, `FollowLogs`, copy, pull policies, reuse, cleanup, backend selection, and the original wait strategies | Available | Available |
| `LogsOptions` and `LogsWithOptions` | Not available | Available |
| `wait.ForHTTP` header, authentication, TLS, and custom-client setters | Not available | Available |
| Exported `wait.AllStrategy` / `AnyStrategy` and composite `WithStartupTimeout` | Not available | Available |
| Root `CLIError`, `ErrContainerNotFound`, and `ErrGenerationReplaced` | Not available | Available |
| Generation-safe deletion, post-`v0.2.0` endpoint hardening, and the current reaper hardening | Not part of the `v0.2.0` contract | Available, subject to the Docker reaper prerequisite below |

Unless a section explicitly says “current development”, descriptions of
behavior describe this checkout rather than the tagged `v0.2.0` release.
The code examples are standalone files and are compile-checked by the
repository's documentation test.

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
    container.Cleanup(t, ctr) // nil-safe; removes the container at test end
    if err != nil {
        t.Fatal(err)
    }

    endpoint, err := ctr.Endpoint(ctx, "6379/tcp") // e.g. "192.168.64.3:6379"
    if err != nil {
        t.Fatal(err)
    }
    _ = endpoint // connect your client to endpoint
}
```

## Backends

| OS | Default backend | Requirement |
|---|---|---|
| macOS | Apple Container | macOS 26+, Apple Silicon, [Apple Container](https://github.com/apple/container) 1.2.x–1.3.x with `container system start` done |
| Linux | Docker | docker CLI + running daemon |
| Windows | Docker | docker CLI + running daemon (no watchdog reaper; see below) |

Set `CONTAINERGO_BACKEND=docker` to use Docker on macOS (e.g. Docker
Desktop), or `CONTAINERGO_BACKEND=apple` to insist on Apple Container.

The library shells out to the backend CLI (`container` or `docker`) — no
cgo and no daemon API client. `DOCKER_HOST`, contexts, and registry auth
are handled by the docker CLI itself. The library uses only
`DOCKER_HOST=tcp://...` to select a remote Docker endpoint; a remote
Docker context is not detected.

Verified backends for the current checkout (CLI stderr wording and inspect
JSON shapes this library matches against):

| Backend | Verified versions |
|---|---|
| Apple Container | 1.2.x–1.3.x |
| Docker Engine / CLI | 29.x |

Newer CLI releases may change error text or JSON fields; see the stderr
matchers at the top of `engine_apple.go` / `engine_docker.go` and the
fixtures under `internal/inspect/testdata/` and `testdata/`.

## Installation

For the released `v0.2.0` API:

```text
go get github.com/hirokazumiyaji/container-go@v0.2.0
```

The current development checkout is not a release. Do not assume that
`@v0.2.0` contains the development-only APIs listed in the version table.

## API stability

Pre-1.0: minor releases (0.x) may include breaking API changes. Pin an
explicit module version (`go get ...@v0.2.0`) for reproducible builds.

The root `container` package (`Run`, options, lifecycle helpers) is the
primary integration surface and is kept reasonably stable within a minor
series. The `wait` package is also public, but interfaces intended for
custom strategies — especially [`wait.Target`](wait/wait.go) — may evolve
as backends and probing needs change; prefer the built-in strategies when
possible.

See [CHANGELOG.md](CHANGELOG.md) for release notes. Its `v0.2.0` section
describes only the tagged release; its `Unreleased` section describes the
current development API.

## Connection endpoints

**Apple Container backend**: by default every container gets a real IP
on the `default` vmnet network, reachable directly from the host. By
default this library uses that IP:

- `Host` returns the container IP, `MappedPort` returns the container port
  itself, and `Endpoint` combines them.
- No host ports are consumed, so parallel tests never conflict over ports.
- On Apple, `WithExposedPorts` is a handle-side endpoint declaration; the
  backend is not given a separate `--expose` flag.

Prefer `Endpoint` (or `MappedPort` with a known host) when you care about
a specific published port: `Host` reports only the first published
binding's address when several publish host-IPs differ.

**Docker backend**: the container IP is generally not reachable from the
host (Docker Desktop), so ports declared via `WithExposedPorts` are
automatically published to daemon-assigned ports — the classic
testcontainers model. Locally this binds loopback (`-p
127.0.0.1::<port>`); with `DOCKER_HOST=tcp://host` (remote daemon, e.g.
`tcp://docker:2375` in CI) it binds all interfaces (`-p 0.0.0.0::<port>`)
so the client can reach it. `Host` returns `127.0.0.1` (or the host from a
`tcp://` `DOCKER_HOST`) and `MappedPort` returns the assigned port.
Assignment happens atomically in the daemon, so parallel tests do not race
over ports here either. With a remote daemon, an explicit
`WithPublishedPort` bound to loopback (`127.0.0.1:...`, `[::1]:...`) is
rejected, since it would only listen on the remote machine. Only
`DOCKER_HOST` is honored; a `docker context` pointing at a remote daemon
is not detected.

When a client insists on `localhost` (or the container IP is not reachable
in your setup), publish the port explicitly:

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

`MappedPort` and `Endpoint` resolve ports declared via
`WithExposedPorts` or explicitly published ports. `ContainerIP` is a
separate diagnostic/API method; it is not what the wait package uses.
Prefer `Endpoint` for clients because Docker Desktop usually does not make
a container IP reachable from the host.

## Wait strategies

Apple Container has no healthcheck or wait primitive, so readiness is
probed client-side by the `wait` package. The following example uses the
strategies available in `v0.2.0`:

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

The primitive strategies have a 60-second default startup timeout.
`ForListeningPort`, `ForExposedPort`, and `ForHTTP` poll every 100ms by
default; `ForExec` polls every 250ms by default. `ForLog` reads a
continuous `FollowLogs` stream rather than polling, so its
`WithPollInterval` setter has no effect. `ForAll` and `ForAny` in
`v0.2.0` do not expose a composite timeout setter. Connection and HTTP
strategies probe the stopped state at most once per second. `ForExec` does
not fail fast while polling; it checks the container state when its wait
deadline expires. `ForLog` reports a stopped container when its log stream
ends before the pattern appears.

The current development checkout adds the following APIs, which are not in
`v0.2.0`:

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

A failed wait on a newly created, non-reused container rolls it back
and, when the bounded log fetch succeeds, attaches a log tail capped at
1 MiB to the error. A reused container is left for the shared lifetime
described below.

## Logs

`Logs` and `LogsWithOptions` return a finite snapshot because the CLI
process finishes, but neither method imposes a byte limit by itself.
`Logs` requests all available output; `LogsWithOptions{Tail, Since}`
requests a time or line window when `Tail` or `Since` is set; the
resulting size still depends on the container's output. Use `Tail` for
a long-lived or noisy container.
`FollowLogs` returns a streaming `io.ReadCloser`; close it or cancel its
context to stop the backend CLI. `ForLog` uses `FollowLogs`, while
`Logs` does not follow new output. `LogsOptions` and `LogsWithOptions`
are current-development APIs, not part of `v0.2.0`.

This current-development example also shows the log stream, exec options,
and public error symbols added after `v0.2.0`:

```go
package docexample

import (
    "context"
    "errors"
    "time"

    container "github.com/hirokazumiyaji/container-go"
)

func DevelopmentLogOptions(ctx context.Context, ctr *container.Container) error {
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

## Image pulls

When `Run` needs to create a container, it applies an explicit pull policy
before starting it:

- `PullMissing` (the default) inspects the local image store and runs an
  explicit pull only when the image is absent.
- `PullAlways` requests an explicit pull for every new-container
  attempt.
- `PullNever` only inspects; it returns `ErrImageNotFound` before
  starting when the image is absent.

Concurrent operations in one process share a pull for the same backend,
image, platform, and operation. A cancelled waiter stops waiting, but the
shared pull continues for the remaining callers. The Docker backend
passes `--pull=never` to `docker run`, so the CLI does not pull a second
time. Apple Container uses the same explicit policy path.

```go
package docexample

import (
    "context"
    "errors"

    container "github.com/hirokazumiyaji/container-go"
)

func PullPolicy(ctx context.Context) {
    _, err := container.Run(ctx, "redis:7-alpine",
        container.WithPullPolicy(container.PullAlways))
    _ = errors.Is(err, container.ErrImageNotFound)
    _ = container.Pull(ctx, "redis:7-alpine")
}
```

`PullNever` fails before starting when the image is absent, so callers can
use `errors.Is(err, container.ErrImageNotFound)`.

## Cleanup contract

The normal, rollback, failed-create, and abnormal-exit paths have
different error visibility.

1. **Normal cleanup**: `container.Cleanup(t, ctr)` registers
   `TerminateContainer` through `t.Cleanup`. A termination error is sent to
   the test's log. `container.TerminateContainer(ctr)` is the deferred-style
   variant and returns its error to the caller. Both helpers are nil-safe.
2. **Post-create rollback**: if a non-reuse `Run` fails while copying files
   or waiting for readiness, `Run` calls `Terminate`. If that deletion
   fails, the returned error includes the original failure and a message
   saying that the container was left behind.
3. **Failed create**: if the backend `run` command itself fails, the
   best-effort `cleanupFailedCreate` path only inspects and deletes a
   container carrying this process's managed/session labels. When the
   creation label is present, it must match this run; an absent label is
   not a mismatch in this best-effort path. Its lock, inspect, and
   delete errors are not joined to the returned `Run` error. A name
   conflict is never cleaned up this way.
4. **Abnormal exit**: when the parent process exits and its reaper pipe
   closes, a best-effort watchdog attempts force-deletion. An uncaught
   process-terminating panic, `SIGKILL`, and `os.Exit` close the pipe; a recovered panic
   does not. The reaper does nothing while the parent is alive. It is not a
   transactional guarantee, and registration or per-entry deletion errors
   are not returned to `Run`. The current base accepts Apple-style names
   and creation generations for reaper entries.

The reaper is an external `/bin/sh` child, is unavailable on Windows, and
is started lazily only for a real CLI container on the non-reuse path.
The current base has an important Docker prerequisite: `docker run`
returns a full 64-hex container ID, but `reaper.register` currently accepts
only Apple-style names. The normal Docker `run --detach` path therefore
cannot register that ID with the reaper on this checkout. If a backend
returns no parseable ID, `Run` falls back to the name-and-generation path.
Issue #73 must be stacked for the Docker-ID registration path to work.
Once #73 is applied, Docker entries use the immutable ID and are deleted
with `docker rm --force`; normal Docker `Terminate`/rollback handles can
already use the ID independently.

Repeated reaper spawn failures are logged once after the retry limit;
delete failures are ignored by the shell. Do not use reaper behavior as a
cleanup acknowledgement.

`CONTAINERGO_KEEP=1` skips `Cleanup`, `TerminateContainer`, and reaper
registration so containers can be inspected during debugging. It does not
change explicit `Container.Terminate`, rollback after a post-create
failure, or the best-effort failed-create cleanup.

`container.Prune(ctx)` removes stopped containers this library created in
any previous session (they carry the
`com.github.hirokazumiyaji.container-go` label). It does not remove
running containers.

## Reuse (shared containers across tests/processes)

`WithReuse` turns `Run` into a get-or-create for a stable `WithName`.
Concurrent callers in the same process, and parallel `go test` packages
in other processes on the same host, share one container:

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
    container.Cleanup(t, ctr) // no-op for reused handles
    if err != nil {
        t.Fatal(err)
    }
    _ = ctr
}
```

Contract:

- `WithName` is required; readiness strategies always re-run.
- An existing container must have been created with `WithReuse` and have
  a compatible image. Name conflicts from a racing create are treated as
  success and attach.
- Stopped leftovers are deleted and recreated; a running container that
  never becomes ready is left alone and returns an error.
- Image compatibility is checked on both backends. Port compatibility is
  backend-specific: Docker compares auto-published exposed-port bindings
  and explicit publications. Apple compares explicit published bindings,
  but cannot compare `WithExposedPorts` declarations because the
  library's Apple inspect model does not retain them. On Apple, each new
  handle uses its own exposed-port declaration against the shared
  container IP, so use distinct names when those declarations must be
  isolated.
- `env` / `cmd` / `mounts` differences attach silently by design; use
  distinct names when they matter.
- Each creation carries a generation label; `Terminate` and the
  stopped-recreate path refuse to delete a replaced generation. The
  name-based guard is limited to processes using this library on the same
  host; an external CLI delete/recreate is outside that guarantee. Docker
  handles can use the immutable Docker ID when it is available.
- `Cleanup`, `TerminateContainer`, and the watchdog reaper skip reused
  handles so other packages keep working. Explicit `ctr.Terminate` still
  removes the shared container — only do that when nothing else needs it.
- `container.PruneReuseGroup(ctx, "integration")` force-removes every
  container tagged with that group (CI teardown). The group is a label,
  not part of the reuse key. Ordinary `Prune` still only deletes stopped
  managed containers.

This library does not reset application data between tests. Prefer a
per-test key prefix, separate DB schemas/namespaces, or an `Exec` setup
step (`FLUSHALL`, `TRUNCATE`, …) before assertions.

## Security notes

- Every CLI call is an argv vector; no shell is involved. The one shell
  script (the reaper) is a fixed string that receives validated container
  names or, after #73, Docker IDs only as stdin data.
- Environment variables are passed via a temporary `0600` env file, so
  secrets never appear in the process table (`ps`).
- Registry credentials are never handled by this library; use
  `container registry login` for Apple Container or `docker login` for
  Docker. The backend CLI owns the resulting credentials and registry
  context.

## Differences from testcontainers-go

Not supported (Apple Container has no equivalent, or out of scope):

| testcontainers-go | Here |
|---|---|
| `wait.ForHealthCheck` | No equivalent; use `ForLog`/`ForExec`/`ForHTTP` |
| Building from a Dockerfile | Out of scope (use `container build` / `docker build` yourself) |
| Ryuk reaper container | Replaced by the local watchdog reaper process, with the limitations described above |
| Random host port mapping | Apple backend connects to the container IP directly; Docker backend auto-publishes to random loopback ports |
| Network/volume creation and lifecycle management | Out of scope for now; `WithNetwork` attaches an existing network and `WithMounts` accepts mounts |
| `GenericContainerRequest.Reuse` | `WithReuse` + `WithName`: cross-process get-or-create with mandatory re-wait and no Cleanup/reaper ownership |

## Development

```text
make test                # unit tests (no backend needed)
make vet
make integration         # integration tests (skips bench/singleflight); each backend skips if unavailable
make integration-docker  # Docker-backend integration tests only
make bench-integration   # pull-heavy bench and singleflight scenarios
```

Integration tests pull library images via `public.ecr.aws/docker/library/...`
to avoid anonymous Docker Hub rate limits. Set `CONTAINERGO_BACKEND=apple`
or `docker` to skip the other backend.

GitHub Actions runs unit tests and race tests on `ubuntu-latest` with Go
1.23.0 and the stable Go release, then runs lint and `govulncheck`. It
also runs the Docker integration matrix on `ubuntu-latest`; Apple
Container integration remains a local test because the hosted runners do
not provide that service. The non-integration documentation test extracts
and compiles the standalone Go examples in both language versions of this
README and in the design document; the tagged examples under `examples/`
exercise a real backend.

Design document: [docs/design.md](docs/design.md) (日本語版:
[docs/design.ja.md](docs/design.ja.md)). The implementation phases in that
document are historical; the current API and behavior are maintained in
the source and tests.

Contributing: [CONTRIBUTING.md](CONTRIBUTING.md). Security reports:
[SECURITY.md](SECURITY.md).

## License

MIT
