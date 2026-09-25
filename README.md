# container-go

日本語版: [README.ja.md](README.ja.md)

A [testcontainers](https://testcontainers.com/)-style Go library for
[Apple Container](https://github.com/apple/container) and Docker: run
throwaway containers from Go tests, with zero third-party
dependencies.

```go
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
| macOS | Apple Container | macOS 26+, Apple Silicon, [Apple Container](https://github.com/apple/container) 1.2.x with `container system start` done |
| Linux | Docker | docker CLI + running daemon |
| Windows | Docker | docker CLI + running daemon (no watchdog reaper; see below) |

Set `CONTAINERGO_BACKEND=docker` to use Docker on macOS (e.g. Docker
Desktop), or `CONTAINERGO_BACKEND=apple` to insist on Apple Container.

The library shells out to the backend CLI (`container` or `docker`) —
no cgo, no daemon API client. `DOCKER_HOST`, contexts, and registry
auth are handled by the docker CLI itself.

Go 1.23+ is required.

Verified backends (CLI stderr wording and inspect JSON shapes this library
matches against):

| Backend | Verified versions |
|---|---|
| Apple Container | 1.2.x–1.3.x |
| Docker Engine / CLI | 29.x |

Newer CLI releases may change error text or JSON fields; see the stderr
matchers at the top of `engine_apple.go` / `engine_docker.go` and the
fixtures under `internal/inspect/testdata/` and `testdata/`.

## Installation

```
go get github.com/hirokazumiyaji/container-go@v0.2.0
```

## API stability

Pre-1.0: minor releases (0.x) may include breaking API changes. Pin an
explicit module version (`go get ...@v0.2.0`) for reproducible builds.

The root `container` package (`Run`, options, lifecycle helpers) is the
primary integration surface and is kept reasonably stable within a minor
series. The `wait` package is also public, but interfaces intended for
custom strategies — especially [`wait.Target`](wait/wait.go) — may evolve
as backends and probing needs change; prefer the built-in strategies when
possible.

See [CHANGELOG.md](CHANGELOG.md) for release notes.

## Connection endpoints

**Apple Container backend**: every container gets a real IP on the
`default` vmnet network, reachable directly from the host. By default
this library uses that IP:

- `Host` returns the container IP, `MappedPort` returns the container
  port itself, and `Endpoint` combines them.
- No host ports are consumed, so parallel tests never conflict over
  ports.

Prefer `Endpoint` (or `MappedPort` with a known host) when you care
about a specific published port: `Host` reports only the first published
binding's address when several publish host-IPs differ.

**Docker backend**: the container IP is generally not reachable from
the host (Docker Desktop), so ports declared via `WithExposedPorts` are
automatically published to daemon-assigned ports — the classic
testcontainers model. Locally this binds loopback
(`-p 127.0.0.1::<port>`); with `DOCKER_HOST=tcp://host` (remote daemon,
e.g. `tcp://docker:2375` in CI) it binds all interfaces
(`-p 0.0.0.0::<port>`) so the client can reach it. `Host` returns
`127.0.0.1` (or the host from a `tcp://` `DOCKER_HOST`) and
`MappedPort` returns the assigned port. Assignment happens atomically
in the daemon, so parallel tests do not race over ports here either.
With a remote daemon, an explicit `WithPublishedPort` bound to loopback
(`127.0.0.1:...`, `[::1]:...`) is rejected, since it would only listen
on the remote machine.
Only `DOCKER_HOST` is honored; a `docker context` pointing at a remote
daemon is not detected.

When a client insists on `localhost` (or the container IP is not
reachable in your setup), publish the port explicitly:

```go
ctr, err := container.Run(ctx, "nginx:alpine",
    container.WithExposedPorts("80/tcp"),
    container.WithPublishedPort("127.0.0.1:18080:80"),
    container.WithWaitStrategy(wait.ForHTTP("/")),
)
// Host → "127.0.0.1", MappedPort("80/tcp") → 18080
```

`MappedPort` and `Endpoint` only resolve ports declared via
`WithExposedPorts` (or published ones).

## Wait strategies

Apple Container has no healthcheck or wait primitive, so readiness is
probed client-side by the `wait` package:

```go
wait.ForLog("Ready to accept connections")   // substring; .AsRegexp(), .WithOccurrence(n)
wait.ForListeningPort("6379/tcp")            // TCP dial succeeds
wait.ForExposedPort()                        // first declared port
wait.ForHTTP("/health")                      // .WithPort, .WithMethod, .WithStatusCodeMatcher, .WithHeaders/.WithHeader, .WithBasicAuth, .WithTLS/.WithTLSConfig/.WithHTTPClient
wait.ForExec([]string{"pg_isready"})         // .WithExitCodeMatcher
wait.ForAll(wait.ForExposedPort()), wait.ForAny(wait.ForExposedPort()) // composition; .WithStartupTimeout
```

The primitive strategies have a 60-second default startup timeout.
`ForListeningPort`, `ForExposedPort`, and `ForHTTP` poll every 100ms by
default; `ForExec` polls every 250ms by default. `ForLog` reads a
continuous `FollowLogs` stream rather than polling, so its
`WithPollInterval` setter has no effect. `ForAll` and `ForAny` do not
expose `WithPollInterval`; their `WithStartupTimeout` setter bounds the
whole composition, while child strategies keep their own settings.
Waiting fails fast if the container stops. A failed wait on a newly
created, non-reused container rolls it back with a tail of its logs
attached to the error; a reused container is left for the shared
lifetime described below.

## Logs

`Logs` and `LogsWithOptions` return a finite snapshot. `LogsWithOptions`
can limit the snapshot with `LogsOptions{Tail, Since}`. `FollowLogs`
returns a streaming `io.ReadCloser`; close it or cancel its context to
stop the backend CLI. `ForLog` uses `FollowLogs`, while `Logs` does not
follow new output.

## Image pulls

When `Run` needs to create a container, it applies an explicit pull
policy before starting it:

- `PullMissing` (the default) inspects the local image store and runs
  an explicit pull only when the image is absent.
- `PullAlways` requests an explicit pull for every `Run` invocation.
- `PullNever` only inspects; it returns `ErrImageNotFound` before
  starting when the image is absent.

Concurrent operations in one process share a pull for the same backend,
image, platform, and operation. A cancelled waiter stops waiting, but
the shared pull continues for the remaining callers. The Docker backend
passes `--pull=never` to `docker run`, so the CLI does not pull a second
time. Apple Container uses the same explicit policy path.

```go
container.Run(ctx, "redis:7-alpine",
    container.WithPullPolicy(container.PullAlways))
// container.PullNever: fail before starting when the image is absent
// (errors.Is(err, container.ErrImageNotFound))

container.Pull(ctx, "redis:7-alpine") // explicit fetch, shared like Run's
```

## Cleanup contract

Three layers make sure containers do not outlive your tests:

1. `container.Cleanup(t, ctr)` registers removal via `t.Cleanup`;
   `container.TerminateContainer(ctr)` is the deferred-style variant.
   Both are nil-safe, so call them before checking `Run`'s error.
2. On the non-reuse path, if `Run` fails partway, it attempts to remove
   whatever it created before returning and reports a deletion failure
   if one occurs.
3. When a real CLI container is registered, a best-effort watchdog
   reaper is started lazily as an external `/bin/sh` child. When the
   parent process closes its pipe (including after a panic or SIGKILL),
   the reaper attempts to force-delete every registered container. It
   is not available on Windows, and a failure to start or complete a
   delete is not hidden as a successful cleanup.

`CONTAINERGO_KEEP=1` skips `Cleanup`, `TerminateContainer`, and reaper
registration so containers can be inspected during debugging. An
explicit `ctr.Terminate`, and rollback after a failed `Run`, can still
remove a container.

`container.Prune(ctx)` removes stopped containers this library created
in any previous session (they carry the
`com.github.hirokazumiyaji.container-go` label). It does not remove
running containers.

## Reuse (shared containers across tests/processes)

`WithReuse` turns `Run` into a get-or-create for a stable `WithName`.
Concurrent callers in the same process, and parallel `go test` packages
in other processes on the same host, share one container:

```go
ctr, err := container.Run(ctx, "redis:7-alpine",
    container.WithName("it-redis"),
    container.WithReuse(),
    container.WithReuseGroup("integration"),
    container.WithExposedPorts("6379/tcp"),
    container.WithWaitStrategy(wait.ForListeningPort("6379/tcp")),
)
container.Cleanup(t, ctr) // no-op for reused handles
```

Contract:

- `WithName` is required; readiness strategies always re-run.
- An existing container must have been created with `WithReuse` and have
  a compatible image. Name conflicts from a racing create are treated as
  success and attach.
- Stopped leftovers are deleted and recreated; a running container that
  never becomes ready is left alone and returns an error.
- Image / port mismatches vs the existing container return a clear error.
  Only image and ports are compared; `env` / `cmd` / `mounts`
  differences attach silently by design (use distinct names when they
  matter).
- Each creation carries a generation label; `Terminate` and the
  stopped-recreate path refuse to delete a replaced generation, and the
  watchdog reaper guards deletion the same way. The name-based guard is
  limited to processes using this library on the same host; an external
  CLI delete/recreate is outside that guarantee.
- `Cleanup`, `TerminateContainer`, and the watchdog reaper skip reused
  handles so other packages keep working. Explicit `ctr.Terminate` still
  removes the shared container — only do that when nothing else needs it.
- `container.PruneReuseGroup(ctx, "integration")` force-removes every
  container tagged with that group (CI teardown). The group is a label,
  not part of the reuse key. Ordinary `Prune` still only deletes
  stopped managed containers.

This library does not reset application data between tests. Prefer a
per-test key prefix, separate DB schemas/namespaces, or an `Exec` setup
step (`FLUSHALL`, `TRUNCATE`, …) before assertions.

## Security notes

- Every CLI call is an argv vector; no shell is involved. The one shell
  script (the reaper) is a fixed string that receives container IDs
  only as validated stdin data.
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
| Ryuk reaper container | Replaced by the local watchdog reaper process |
| Random host port mapping | Apple backend connects to the container IP directly; Docker backend auto-publishes to random loopback ports |
| Network/volume creation and lifecycle management | Out of scope for now; `WithNetwork` attaches an existing network and `WithMounts` accepts mounts |
| `GenericContainerRequest.Reuse` | `WithReuse` + `WithName`: cross-process get-or-create with mandatory re-wait and no Cleanup/reaper ownership |

## Development

```
make test                # unit tests (no backend needed)
make vet
make integration         # integration tests (skips bench/singleflight); each backend skips if unavailable
make integration-docker  # Docker-backend integration tests only
make bench-integration   # pull-heavy bench and singleflight scenarios
```

Integration tests pull library images via `public.ecr.aws/docker/library/...`
to avoid anonymous Docker Hub rate limits. Set `CONTAINERGO_BACKEND=apple` or
`docker` to skip the other backend.

GitHub Actions runs unit tests and race tests on `ubuntu-latest` with Go
1.23.0 and the stable Go release, then runs lint and `govulncheck`. It
also runs the Docker integration matrix on `ubuntu-latest`; Apple
Container integration remains a local test because the hosted runners do
not provide that service. The non-integration
`examples/compile_test.go` keeps the documented API calls type-checked;
the tagged examples under `examples/` exercise a real backend.

Design document: [docs/design.md](docs/design.md) (日本語版:
[docs/design.ja.md](docs/design.ja.md)). The implementation phases in
that document are historical; the current API and behavior are
maintained in the source and tests.

Contributing: [CONTRIBUTING.md](CONTRIBUTING.md). Security reports:
[SECURITY.md](SECURITY.md).

## License

MIT
