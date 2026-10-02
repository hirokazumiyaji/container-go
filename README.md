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
| Linux | Docker | docker CLI + running daemon; copy-out requires client/server 29.7.0+ |
| Windows | Docker | docker CLI + running daemon; copy-out requires client/server 29.7.0+ (no watchdog reaper; see below) |

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
| Docker Engine / CLI | 29.x; secure copy-out requires client/server 29.7.0+ |

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

## Copy-out backend capability

`CopyFileFromContainer` is supported only by the Docker backend when the
host provides safe file-open semantics. This library verifies that the
materialized result is a regular file and does not claim that every Docker
host can represent every container file type. Apple Container's
`container cp` has no type-preserving/no-follow copy-out mode; it can
dereference or consume links and special files before host-side validation.
On Apple Container, `CopyFileFromContainer` therefore returns
`ErrCopyFileFromContainerUnsupported` without invoking the CLI. Hosts
without no-follow and nonblocking file-open support fail closed with the
same error. On Windows, this includes Go 1.23 through 1.25, whose
`os.OpenFile` does not propagate the required Windows file flags; use Go
1.26 or newer for Docker copy-out there. Use
`CONTAINERGO_BACKEND=docker` on macOS when a safe copy-out is required.
Container paths passed to the copy APIs are absolute POSIX paths using `/`;
backslashes are rejected. Docker copy-out requires both the Docker client
and server to be version 29.7.0 or newer. The method verifies both versions
before creating its private temporary directory or invoking `docker cp`, and
returns `ErrCopyFileFromContainerUnsupported` if the minimum cannot be
verified. CI unit and Docker integration jobs run on Linux only; Windows
runtime coverage is manual and requires Go 1.26+ with Docker client/server
29.7.0+.

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
(`-p 127.0.0.1::<port>`); with a remote `DOCKER_HOST` it binds all
interfaces (`-p 0.0.0.0::<port>`) so the client can reach it. Remote
means any `DOCKER_HOST` that does not name this machine:
`tcp://host` (e.g. `tcp://docker:2375` in CI), `ssh://user@host`, or a
scheme-less `host:port` / hostname, normalized the way the Docker CLI
normalizes it (`tcp://` prepended). `unix://`, `npipe://`, loopback
addresses, and an empty value stay local. Docker CLI client protocols
are `unix`, `tcp`, `npipe`, and `ssh`; other schemes are not usable
`DOCKER_HOST` endpoints. `Host` returns `127.0.0.1`
(or the remote hostname) and `MappedPort` returns the assigned port.
IPv6 bindings are canonicalized without changing address family: an
unspecified `::` endpoint resolves to `[::1]`, for example. Assignment
happens atomically in the daemon, so parallel tests do not race over
ports here either. For `ssh://` the remote hostname must be directly
dialable: the CLI's SSH session carries only the Docker API, not
published ports, so an alias reachable only through a ProxyJump or
bastion needs a manual `ssh -L` forward. With a remote daemon, an
explicit `WithPublishedPort` bound to loopback (`127.0.0.1:...`,
`[::1]:...`) is rejected, since it would only listen on the remote
machine. Reuse also rejects an existing remote-daemon loopback binding
instead of rewriting it to an unreachable host address.

Docker's `host` and `none` modes cannot create library-managed port
bindings. Externally isolated networks (`Internal: true` or an isolated
bridge gateway mode) are rejected as well. For an explicitly selected
non-default network, `Run` inspects it before pulling or creating anything
and returns a `*ConfigError` (matching `ErrInvalidConfig`) when either
`WithExposedPorts` or `WithPublishedPort` is combined with one of these
networks. `host` and `none` publish combinations are rejected before any
image or container command.

Host mode remains available without port options. `Host` returns the
client-facing daemon host, but `MappedPort` and `Endpoint` do not invent
a host-namespace service port: they require a port declared and bound by
this library. `none` mode has no reachable host, so `Host` returns an
error matching `ErrNoReachableHost`. If a Docker installation disables
host networking, the backend CLI start error is returned rather than a
fabricated endpoint. Runtime network mismatches match
`ErrNetworkMismatch`.

When `WithNetwork` is omitted, the Docker CLI is left without a
`--network` argument so the daemon chooses its platform default (`bridge`
on Linux, `nat` on native Windows). Endpoint and Host resolution uses the
actual mode and `NetworkSettings.Networks` from inspect; a pre-existing
container reporting Docker's special `default` mode is canonicalized
against that actual network. Compatibility also uses the daemon's
reported server platform to identify its authoritative default, so a
user-defined `bridge` or `nat` is not mistaken for that default. If the
identity is unavailable or ambiguous, the operation fails with
`ErrNetworkMismatch`. `WithReuse` is therefore compatible with a matching
daemon default, but never treats an omitted option as a wildcard for
`host`, `none`, or an arbitrary named network. Docker handles retain
the immutable container ID returned by `run`, and endpoint, Host, lifecycle,
and reuse operations inspect that ID; dynamic network, IP, and binding
data are refreshed on every operation rather than served from a stale
snapshot.

Only `DOCKER_HOST` is honored for host reachability; a `docker context`
pointing at a remote daemon is not used to rewrite endpoint hosts.

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

`ForListeningPort` and port declarations have different protocol support:

| API | TCP | UDP |
|---|---|---|
| `WithExposedPorts` / `WithPublishedPort` | Supported | Supported |
| `wait.ForListeningPort` | `6379` or `6379/tcp` | `*wait.ConfigError` before probing |

`ForListeningPort` also returns `*wait.ConfigError` for malformed port
specifications. Use `errors.Is(err, wait.ErrInvalidConfiguration)` to
classify either configuration error without matching its message.

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
  Docker also requires the requested network identity to match. Omitted
  `WithNetwork` means the daemon default; inspect's `default` mode is
  resolved against `NetworkSettings.Networks` (`bridge` on Linux, `nat` on
  native Windows). `host`, `none`, and named networks are not wildcards.
  Reuse re-inspects by immutable UID before compatibility checks and before
  returning the handle. `env` / `cmd` / `mounts` differences still attach
  silently by design (use distinct names when they matter).
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
