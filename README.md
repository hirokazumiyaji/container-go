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

    endpoint, _ := ctr.Endpoint(ctx, "6379/tcp") // e.g. "192.168.64.3:6379"
    // ... connect your client to endpoint
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
wait.ForHTTP("/health")                      // .WithPort, .WithMethod, .WithStatusCodeMatcher, .WithHeaders, .WithBasicAuth, .WithTLS/.WithTLSConfig/.WithHTTPClient
wait.ForExec([]string{"pg_isready"})         // .WithExitCodeMatcher
wait.ForAll(...), wait.ForAny(...)           // composition; .WithStartupTimeout
```

Every strategy accepts `WithStartupTimeout` (default 60s) and
`WithPollInterval` (default 100ms; `ForAll` / `ForAny` accept `WithStartupTimeout` to bound the composition). Waiting fails fast if the container
stops, and a failed wait rolls the container back with a tail of its
logs attached to the error.

## Exec timeout and long-running commands

`Container.Exec` is a finite, buffered operation. When the caller's
context has no deadline, Exec applies a 30-second default so a hung
backend cannot block a test indefinitely. For a positive
`WithExecTimeout(d)`, the effective deadline is the earlier of `d` and
the caller's deadline. `WithExecTimeout(0)` only disables the library
default; it does not remove a caller deadline. Use zero only for a
deliberately long-running command and pair it with a cancellable
context (prefer a deadline). Long-lived processes should normally be
the container's main command (`WithCmd`) with `FollowLogs` for output
rather than a long-lived `Exec` call.

Exec keeps the existing `(exitCode, output, error)` contract: a
command's non-zero exit is a result, while a backend, timeout, or
cancellation error is returned with the classified error. A CLI-reported
exit status, when available, is retained in `exitCode` even alongside
that error. In either error case, read `output` to retain partial stdout
and stderr produced before the failure.

Cancellation is owned by the local command lifecycle. On Unix-like systems a
best-effort process-group termination is attempted only on targets with a
stable process identity and while the direct child handle still owns the
process; the direct child is also killed after the group signal because it may
have changed process groups. Other Unix targets conservatively use the direct
child handle. Once that child is reaped, no former numeric process-group ID is
used. On Windows, a lifecycle-owned Job Object handle provides the descendant
boundary, with direct-child fallback when assignment is unavailable. Job
assignment happens after `Start`; descendants created during that short
post-Start attachment window are outside the job boundary. Neither boundary
claims remote container-process termination.
Windows-specific lifecycle tests are build-constrained. The development
environment cross-compiles and vets the Windows packages but cannot execute
Windows Job Object runtime tests, including the exit-259 active-child case.

Neither supported backend CLI exposes a common kill operation for an exec
instance. When active-child evidence is available and a context error actually
races with a launched command, Exec returns an `*ExecTerminationError`
(`errors.Is(err, ErrExecTerminationUnsupported)`) instead of claiming that
the container-side process stopped. A conservative direct-handle fallback may
return the context error without that remote-termination claim. A successful
empty-job or already-finished child termination is not active-process
evidence. Classification does not depend on the local exit status: in
particular, Windows `Process.Kill` may report the killed process as exit code
`1`, and that status remains visible without suppressing the typed error. A
deadline consumed later by a verification inspect does not by itself produce
`ExecTerminationError`. The backend-side process may still be running; callers
must terminate the container or use a backend-specific cleanup path.

`FollowLogs` returns startup failures directly. After a stream is returned,
read it to EOF: terminal CLI failures (including a CLI status racing context
cancellation) are reported by `Read`. `Close` and context cancellation are
intentional termination paths and may instead produce EOF or a context error.
If a descendant retains stdout/stderr after the direct child exits, the stream
uses a bounded drain and then closes its endpoints so EOF cannot wait forever.
`ForLog` observes terminal stream errors before accepting a match, preserves
terminal/context errors with `errors.Join`/`%w`, and does not detach a state
probe after cancellation. Timeout classification uses structured context,
`Timeout() bool`, signal, or `CLIError.OperationTimeout` evidence; arbitrary
workload stderr is treated as application output.

## Image pulls

`Run` checks the image before starting and fetches it when missing
(`PullMissing`, the previous implicit behavior). Concurrent `Run` calls
in one process share the pull: the first caller fetches, the rest wait
for it. The Docker backend passes `--pull=never` so pulling happens
only through this aggregated path.

```go
container.Run(ctx, "redis:7-alpine",
    container.WithPullPolicy(container.PullAlways)) // pull on every Run
// container.PullNever: fail before starting when the image is absent
// (errors.Is(err, container.ErrImageNotFound))

container.Pull(ctx, "redis:7-alpine") // explicit fetch, shared like Run's
```

## Cleanup contract

Three layers make sure containers do not outlive your tests:

1. `container.Cleanup(t, ctr)` registers removal via `t.Cleanup`;
   `container.TerminateContainer(ctr)` is the deferred-style variant.
   Both are nil-safe, so call them before checking `Run`'s error.
2. If `Run` fails partway, it removes whatever it created before
   returning.
3. A watchdog reaper (an external `/bin/sh` child) force-deletes every
   registered container when the test process dies in any way,
   SIGKILL and panics included. The reaper needs `/bin/sh`, so it is
   unavailable on Windows — there, cleanup relies on the first two
   layers only.

Extras:

- `CONTAINERGO_KEEP=1` keeps containers around for debugging.
- `container.Prune(ctx)` removes stopped containers this library
  created in any previous session (they carry the
  `com.github.hirokazumiyaji.container-go` label).

## Reuse (shared containers across tests/processes)

`WithReuse` turns `Run` into a get-or-create for a stable `WithName`.
Concurrent callers in the same process, and parallel `go test` packages
in other processes, share one container:

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
- Name conflicts from a racing create are treated as success and attach.
- Stopped leftovers are deleted and recreated; a running container that
  never becomes ready is left alone and returns an error.
- Image / port mismatches vs the existing container return a clear error.
  Only image and ports are compared; `env` / `cmd` / `mounts`
  differences attach silently by design (use distinct names when they
  matter).
- Each creation carries a generation label; `Terminate` and the
  stopped-recreate path refuse to delete a replaced generation, and the
  watchdog reaper guards deletion the same way.
- `Cleanup`, `TerminateContainer`, and the watchdog reaper skip reused
  handles so other packages keep working. Explicit `ctr.Terminate` still
  removes the shared container — only do that when nothing else needs it.
- `container.PruneReuseGroup(ctx, "integration")` force-removes every
  container tagged with that group (CI teardown). Ordinary `Prune` still
  only deletes stopped managed containers.

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
  `container registry login`, which stores them in the macOS Keychain.

## Differences from testcontainers-go

Not supported (Apple Container has no equivalent, or out of scope):

| testcontainers-go | Here |
|---|---|
| `wait.ForHealthCheck` | No equivalent; use `ForLog`/`ForExec`/`ForHTTP` |
| Building from a Dockerfile | Out of scope (use `container build` / `docker build` yourself) |
| Ryuk reaper container | Replaced by the local watchdog reaper process |
| Random host port mapping | Apple backend connects to the container IP directly; Docker backend auto-publishes to random loopback ports |
| Network/volume management APIs | Out of scope for now |
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

Design document: [docs/design.md](docs/design.md) (日本語版:
[docs/design.ja.md](docs/design.ja.md))

Contributing: [CONTRIBUTING.md](CONTRIBUTING.md). Security reports:
[SECURITY.md](SECURITY.md).

## License

MIT
