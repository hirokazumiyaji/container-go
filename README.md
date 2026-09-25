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
   returning when it can prove that cleanup is safe. If removal fails,
   the returned `*CleanupError` preserves both the original operation
   error and the cleanup error. A failed reuse create that is already
   running is never auto-deleted, because another caller may have adopted
   it; the error reports that retained generation instead. With
   `CONTAINERGO_KEEP=1`, a verified partial handle is returned alongside
   the error for explicit inspection or termination.
3. A watchdog reaper (an external `/bin/sh` child) force-deletes every
   registered container when the test process dies in any way,
   SIGKILL and panics included. Docker's full 64-hex container IDs and
   Apple names are both valid registration targets. The reaper needs
   `/bin/sh`, so it is unavailable on Windows — there, cleanup relies on
   the first two layers only. If the Apple name lock cannot be acquired,
   the reaper skips that entry rather than deleting without coordination.

Extras:

- `CONTAINERGO_KEEP=1` keeps containers around for debugging and enables
  the documented partial-handle policy for verified failed creates.
- `container.Prune(ctx)` removes stopped containers this library
  created in any previous session (they carry the
  `com.github.hirokazumiyaji.container-go` label and a valid creation
  generation). On Apple Container it
  re-inspects each list candidate under the stable name lock immediately
  before deleting it.

When a backend probe also fails, the returned error satisfies both
`errors.Is(err, container.ErrSystemNotRunning)` and
`errors.As(err, *container.CLIError)` for the original command failure.

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
- A stopped generation is deleted and recreated only after its managed,
  reuse, and valid generation labels are verified. A failed create that
  left a running generation is not auto-deleted, because a peer may have
  adopted it; `Run` returns a `CleanupError` describing the retained
  generation (or a verified partial handle with `CONTAINERGO_KEEP=1`).
  A running container that never becomes ready is likewise left alone.
- Image / port mismatches vs the existing container return a clear error.
  Existing reuse containers must carry the managed, reuse, and valid
  creation-generation labels; otherwise reuse fails closed. `PullAlways`
  refreshes the local image before reuse is resolved, and
  its pull failure is returned even when the container is already
  running. A running reused container is never recreated after a
  post-pull mismatch; the call fails instead, including when a requested
  digest differs from the container image. Mutable tag requests refresh
  the local store but do not replace an already-running shared container;
  pin a digest when the exact image identity matters. `PullMissing` and
  `PullNever` do not add an attach-only fetch. `WithPlatform` is compared
  field by field (OS, architecture, and variant), and Apple inspect
  output carries the OCI descriptor digest into the image identity.
  `WithReuseGroup` only tags a newly created/recreated generation and is
  not part of the reuse compatibility key.
- `WithFiles` is applied after the shared ensure flight for every reuse
  caller, including the create leader and attach callers. A leader's copy
  failure therefore does not poison successful waiters. Docker copies
  target the inspected immutable UID; Apple copies verify the generation
  under the stable per-name lock. A replacement between identity
  verification and copy fails closed. A failed reuse copy returns an error
  without deleting the shared generation. Copies to the same path are
  shared-state mutations, so concurrent callers should avoid conflicting
  targets. Creation-only options — `WithEnv`, `WithCmd`,
  `WithEntrypoint`, `WithLabels`, `WithMounts`, `WithCPUs`, `WithMemory`,
  `WithUser`, `WithWorkingDir`, and `WithNetwork` — are intentionally
  ignored on attach; use distinct names when they matter.
- Apple create, generation-checked delete, and prune operations use a
  stable per-name lock shared by cooperating processes on the host. Each
  prune candidate is re-inspected under that lock before deletion. A
  direct `container` CLI call or another implementation does not take the
  lock and remains outside this guarantee.
- Each creation carries a generation label; `Terminate`, stopped reuse,
  prune, and the watchdog reaper refuse to delete a replaced generation.
- `Cleanup`, `TerminateContainer`, and the watchdog reaper skip reused
  handles so other packages keep working. Explicit `ctr.Terminate` still
  removes the shared container — only do that when nothing else needs it.
- `container.PruneReuseGroup(ctx, "integration")` force-removes every
  container tagged with that group (CI teardown). Ordinary `Prune` still
  only deletes stopped managed containers, with Apple candidates
  revalidated under the lock.

This library does not reset application data between tests. Prefer a
per-test key prefix, separate DB schemas/namespaces, or an `Exec` setup
step (`FLUSHALL`, `TRUNCATE`, …) before assertions.
## Security notes

- Every CLI call is an argv vector; no shell is involved. The one shell
  script (the reaper) is a fixed string that receives validated Apple
  names or Docker's full 64-hex IDs only as stdin data. Name-addressed
  Apple reaper entries use the same stable lock as library cleanup paths.
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
