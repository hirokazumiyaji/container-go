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

After the inspect (and after a pull, when needed), `Run` resolves the
image identity and passes that identity to the backend instead of the
caller's mutable tag. Docker prefers a matching `RepoDigests` entry and
falls back to the inspected local image `Id`. A local Docker tag whose
`RepoDigests` belong to another repository is treated as a local alias
and safely uses that `Id`; only an explicit `image@digest` whose
repository or digest conflicts is rejected as
`ErrImageIdentityMismatch`. Apple Container uses the root image
descriptor as the run/reuse identity, validates the selected platform
variant separately, and normalizes an ID-only record to a digest
reference when the descriptor is available. Reuse compares the resolved
digest/ID identity, not the original tag, and removes a newly-created
reused container if its post-create identity or port validation fails.

Apple has no runtime `--pull=never` switch. Before `Run` passes any
pinned Apple reference to `container run`, it verifies that exact
reference in the local store for `PullMissing`, `PullAlways`, and
`PullNever`. If it is missing and the selected policy permits a fetch,
the library performs an explicit exact-digest pull and verifies it again;
`container run` is never allowed to perform that implicit fetch. A
successful inspect that cannot provide a repository-bearing digest or a
verified local image ID returns `ErrImageIdentityUnavailable`; a
confirmed local absence returns `ErrImageIdentityNotLocal`. Transport,
permission, and cancellation failures from the addressability check are
returned as operational errors and do not trigger the mutable fallback.
`WithAllowMutableImageTag` is the explicit escape hatch for a mutable
input when identity is unavailable or the resolved reference is not
locally addressable:

```go
container.Run(ctx, "redis:7-alpine",
    container.WithAllowMutableImageTag())
```

With that option, a mutable input may run the original tag; this
deliberately retains the tag-replacement window and is not an identity
guarantee. Apple also treats a caller-supplied `image@sha256:...` as
a mutable alias, so that spelling is rejected by default and requires
this explicit option. The option never downgrades a bare digest or a
Docker image-ID-shaped value. Pinning cannot make a mutable registry
tag's pull-to-inspect resolution atomic when another actor can modify
the shared backend. Prefer a caller-supplied `image@sha256:...` when
the backend provides an atomic address and treat its identity metadata
as the compatibility boundary. Locally built Apple images may require
`WithAllowMutableImageTag` when no locally addressable digest reference
exists.

```go
container.Run(ctx, "redis:7-alpine",
    container.WithPullPolicy(container.PullAlways)) // pull on every Run
// container.PullNever: fail before starting when the image is absent
// (errors.Is(err, container.ErrImageNotFound)); a pinned identity that
// is not locally addressable also fails with ErrImageIdentityNotLocal

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
  Image compatibility uses the resolved digest or image ID (never the
  original mutable tag); only image and ports are compared. `env` /
  `cmd` / `mounts` differences attach silently by design (use distinct
  names when they matter). A newly-created reused container is removed
  if its post-create validation fails.
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
- `Run` passes the verified identity returned by image inspect (or a
  Docker image ID) to the backend, so a later local tag reassignment
  does not change that create. Apple descriptor-backed references are
  checked for local addressability before create; caller-supplied
  `name@digest` aliases are rejected unless `WithAllowMutableImageTag`
  explicitly accepts their mutable semantics. `container run` is never
  used as an implicit fetch. This is a backend/API guarantee, not a
  claim that every tag-to-registry operation is atomic: a mutable tag
  can still be replaced before the post-pull inspect, and an
  identity-less backend requires the explicit compatibility fallback.

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
