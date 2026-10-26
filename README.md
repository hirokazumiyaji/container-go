# container-go

日本語版: [README.ja.md](README.ja.md)

A [testcontainers](https://testcontainers.com/)-style Go library for
[Apple Container](https://github.com/apple/container) and Docker: run
throwaway containers from Go tests, with no runtime dependency on a
third-party container client. The library's only non-standard Go module
is `golang.org/x/sys`, used for platform process and file handling; the
rest of the implementation uses the standard library.

## Version scope

The latest tagged release is `v0.3.0` (2026-10-26). This checkout is
development after that tag. The install command below installs the
released API; `v0.3.0` includes the APIs and fixes documented in the
`0.3.0` section of [CHANGELOG.md](CHANGELOG.md).

The root library checkout requires Go 1.26 or later. The nested
`bench/` module used by `make bench-integration` requires Go 1.26 or
later. The `v0.3.0` module requires Go 1.26 or later.

The code examples are standalone files and are compile-checked by the
repository's documentation test.

The current checkout still has these deliberate constraints:

- Windows has no watchdog reaper: the reaper needs `/bin/sh`. Explicit
  `Terminate`, `Cleanup`, `TerminateContainer`, `Prune`, and the rollback
  paths remain available there. Operations that need a secret environment file fail with
  `ErrEnvFileUnsupported` on Windows, and Docker copy-out requires Go 1.26
  or newer.
- Network creation and lifecycle management are out of scope;
  `WithNetwork` attaches an existing network. Volume creation and lifecycle
  management are also out of scope; `WithMounts` accepts bind, named-volume,
  and tmpfs mounts, and the library never creates anonymous volumes.
- A bind mount against a remote Docker daemon is rejected with
  `ErrUnsupportedCapability`: the daemon resolves the source path on its own
  host, so a client path cannot be validated. Windows host paths are
  accepted for a local Docker daemon.
- Apple Container cannot honor `PullNever`
  (`ErrPullNeverUnsupported`), has no type-preserving copy-out
  (`CopyFileFromContainer` returns
  `ErrCopyFileFromContainerUnsupported`), and has no `--since` log option
  (`LogsWithOptions` rejects `Since` with `ErrUnsupportedCapability` while
  mapping `Tail` to `-n`). Apple Container also has no healthcheck or wait
  primitive, so readiness is probed client-side.
- Dockerfile builds are out of scope; use `container build` or
  `docker build` directly.

The sections below describe these constraints in context.

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
| macOS | Apple Container | macOS 26+, Apple Silicon, [Apple Container](https://github.com/apple/container) CLI 1.2.2 or 1.3.0 with `container system start` done |
| Linux | Docker | docker CLI 29.x (29.7.2 is checked here) + running daemon; copy-out requires client/server 29.7.0+ |
| Windows | Docker | docker CLI 29.x (29.7.2 is checked here) + running daemon; copy-out requires client/server 29.7.0+ (no watchdog reaper; use `Prune` in `TestMain`, see Cleanup contract) |

Set `CONTAINERGO_BACKEND=docker` to use Docker on macOS (e.g. Docker
Desktop), or `CONTAINERGO_BACKEND=apple` to insist on Apple Container.

On Windows the watchdog reaper is unavailable, and a remote daemon
outlives the local test environment. Schedule `Prune` from `TestMain`
before and after the suite, as described in the Cleanup contract, rather
than relying on the reaper to remove leftovers.

The library shells out to the backend CLI (`container` or `docker`) — no
cgo and no daemon API client. `DOCKER_HOST`, contexts, and registry auth
are handled by the docker CLI itself. The library resolves remote endpoints from
`DOCKER_HOST` or the active/configured Docker context (supporting `tcp://` and `ssh://` transports).

Go 1.26+ is required for the root module. The optional nested
`bench/` benchmark module requires Go 1.26+ because it depends on the
root module and pins a newer testcontainers-go toolchain; see
[bench/README.md](bench/README.md).

Verified backend behavior (the CLI stderr wording and inspect JSON shapes
this library matches against):

| Backend | Evidence used by this repository |
|---|---|
| Apple Container CLI | Source/help and inspect fixtures for 1.2.2 and 1.3.0 |
| Docker Engine / CLI | 29.x; a 29.x-shaped inspect fixture and a local 29.7.2 run; secure copy-out requires client/server 29.7.0+ |

The Apple API-server version is read at runtime from `container system
version` or `container system status`; it is not inferred from the CLI
version. The opt-in live matrix skips when that server version is unavailable
or differs from the CLI version, so this repository does not claim an
unobserved API-server compatibility range. These entries describe repository
evidence, not an exhaustive compatibility claim. Newer CLI releases may change
error text or JSON fields; see the stderr matchers at the top of
`engine_apple.go` / `engine_docker.go` and the fixtures under
`internal/inspect/testdata/` and `testdata/`.

`WithName` and name-addressed reaper entries use the shared library guard
`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$` (one to 63 characters). This is a
conservative safety rule, not a claim about the complete Docker or Apple
name grammar. Apple Container's CLI has the stricter 2–63-character rule
`^[a-zA-Z0-9][a-zA-Z0-9_.-]{1,62}$`, which the Apple backend applies before
resolving an image, so a one-character name is rejected up front rather than
after a pull.

## Installation

To install the latest tagged release:

```text
go get github.com/hirokazumiyaji/container-go@v0.3.0
```

This installs the released API from `v0.3.0`. This checkout may contain
changes after that tag; pin the module version as above for reproducible
builds.

## API stability

Pre-1.0: minor releases (0.x) may include breaking API changes. Pin an
explicit module version (`go get ...@v0.3.0`) for reproducible builds.

The root `container` package (`Run`, options, lifecycle helpers) is the
primary integration surface and is kept reasonably stable within a minor
series. The `wait` package is also public, but interfaces intended for
custom strategies — especially [`wait.Target`](wait/wait.go) — may evolve
as backends and probing needs change; prefer the built-in strategies when
possible.

See [CHANGELOG.md](CHANGELOG.md) for release notes. Its `0.3.0` section
describes the tagged release; its `Unreleased` section describes changes
after that tag.

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
verified. CI unit tests run on Linux, macOS, and Windows; Docker integration
jobs run on Linux. Real Windows Docker copy-out requires manual integration
testing with Go 1.26+ and Docker client/server 29.7.0+.

## Output contract

New CLI output paths capture through bounded or caller-owned sinks so a
container cannot make the host allocate an unbounded response:

- `LogsWithOptions{MaxBytes: n}` retains at most `n` combined bytes and
  returns a reader implementing `Truncated() bool`. `Tail` is a
  backend **line-based tail** (`--tail` on Docker, `-n` on Apple); it is applied before the byte
  cap. `MaxBytes` is a **prefix** cap: it retains the first bytes of the
  combined stdout/stderr arrival stream, not the trailing bytes. A
  positive `WithExecMaxBytes(n)` applies the same prefix policy to
  `Exec`. Legacy unbounded readers retain their historical
  stdout-then-stderr concatenation; bounded captures use arrival order
  when the runner implements streaming `RunTo` (a legacy-only runner is
  adapted in that order).
- `ExecTo` and `LogsTo` stream directly to an `io.Writer`; use
  `WithExecMaxBytes` or `LogsOptions.MaxBytes` to drain and discard
  bytes beyond a caller-selected prefix. `OutputStats.Bytes` is the
  total observed, including discarded bytes. These streaming methods
  merge stdout and stderr in arrival order. A writer error is returned
  even when the child exits successfully; it is joined with a terminal
  CLI error when both occur. Use `errors.Is(err,
  container.ErrOutputDelivery)` for the category while retaining the
  original writer error in the chain.
- A bounded reader returns `ErrOutputTruncated` as its terminal read
  error, while still returning all retained bytes. Check
  `errors.Is(readErr, container.ErrOutputTruncated)` or the
  `Truncated()` method; truncation is never silent. On a backend,
  cancellation, launch, or missing-container error, bounded APIs still
  return the partial reader and its truncation state.
- `FollowLogs` is intentionally a long-lived stream and has no
  automatic byte limit. The caller owns its lifetime and backpressure.
- `wait.ForExec` discards command output for every poll, and wait-failure
  diagnostics use a fixed 1 MiB trailing log buffer. That diagnostic tail
  is separate from the public prefix cap. The internal ExecRunner
  `CLIError` keeps at most the trailing 64 KiB of stderr per failed
  invocation; this diagnostic limit does not bound `Run` output or a
  caller-owned sink.

`Logs`, `Exec` without `WithExecMaxBytes`, and `MaxBytes == 0` retain
the historical full-output behavior for source compatibility. That
compatibility path is deprecated for untrusted or long-lived output;
new code should select a positive limit or use a streaming method.
The legacy path will be reconsidered in a future minor release, but
this change does not silently discard existing output.

`LogsOptions` now has a `MaxBytes` field. Go cannot add a field to a
struct without breaking old unkeyed literals, so this release
intentionally requires keyed `LogsOptions` literals; update existing
`LogsOptions{tail, since}` calls to `LogsOptions{Tail: tail, Since:
since}`. The repository is pre-1.0 and documents this narrow source
compatibility tradeoff rather than hiding it behind a second option
representation. The large-output integration check verifies bounded
forwarding and draining; it does not measure peak host memory, which
would require an external RSS sampler.

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
127.0.0.1::<port>`); with a remote `DOCKER_HOST` it binds all interfaces
(`-p 0.0.0.0::<port>`) so the client can reach it. Remote means any
`DOCKER_HOST` that does not name this machine: `tcp://host` (e.g.
`tcp://docker:2375` in CI), `ssh://user@host`, or a scheme-less
`host:port` / hostname, normalized the way the Docker CLI normalizes it
(`tcp://` prepended). `unix://`, `npipe://`, loopback addresses, and an
empty value stay local. Docker CLI client protocols are `unix`, `tcp`,
`npipe`, and `ssh`; other schemes are not usable `DOCKER_HOST` endpoints.
Both `DOCKER_HOST` and Docker contexts (active or explicitly configured
via `WithDockerContext` / `WithClientDockerContext`) are detected and
resolved for remote daemons. `Host` returns `127.0.0.1` (or the remote
hostname) and `MappedPort` returns the assigned port. IPv6 bindings are
canonicalized without changing address family: an unspecified `::`
endpoint resolves to `[::1]`, for example. Assignment happens atomically
in the daemon, so parallel tests do not race over ports here either. For
`ssh://` the remote hostname must be directly dialable: the CLI's SSH
session carries only the Docker API, not published ports, so an alias
reachable only through a ProxyJump or bastion needs a manual `ssh -L`
forward. With a remote daemon, an explicit `WithPublishedPort` bound to
loopback (`127.0.0.1:...`, `[::1]:...`) is rejected, since it would only
listen on the remote machine. Reuse also rejects an existing
remote-daemon loopback binding instead of rewriting it to an unreachable
host address.

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
and reuse operations inspect that ID. Endpoint reads can share the short-lived
dynamic snapshot described below; lifecycle and reuse checks refresh it.

For parallel tests that need the same logical name on separate networks,
create a user-defined network per test (for example with `docker network
create`), remove it in `t.Cleanup`, and pass `WithNetworkAliases("db")`
with that `WithNetwork`. Each container then resolves as `db` inside its
own network without colliding with other tests or with container names.
Network aliases require an explicitly selected user-defined network:
Docker's default bridge, `host`, `none`, and `nat` networks do not
resolve them, so `Run` rejects those combinations with a `*ConfigError`
before any backend call. The aliases are part of the reuse compatibility
check, so a reused container that does not carry them is not adopted.
The library attaches to existing networks; it does not create or delete
them.

Both `DOCKER_HOST` and Docker contexts are honored for host reachability;
endpoint hosts resolve to the detected remote endpoint.

For a local Docker daemon, when a client insists on `localhost` (or the
container IP is not reachable in your setup), publish the port
explicitly. The loopback example below is local-only; a non-loopback
`DOCKER_HOST` rejects loopback bindings.

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
`WithExposedPorts` or explicitly published ports. They return
`ErrPortNotExposed` when a port is undeclared or a declared port has no
usable host binding. `ContainerIP` is a separate diagnostic/API method; it
is not what the wait package uses. Prefer `Endpoint` for clients because
Docker Desktop usually does not make a container IP reachable from the host.

Endpoint-related methods reuse a short (~1s), identity-checked dynamic
inspect snapshot shared with `Shared()` handles, so readiness polling does
not spawn a backend CLI process on every retry. The cached network, IP, and
binding data expires after that TTL; `State` always re-inspects. If Docker
reports multiple networks and no top-level address, the IP comes from the
first network with a non-empty address in lexicographic network-name order.

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

Every leaf strategy accepts `WithStartupTimeout` (zero means 60s) and
`WithPollInterval` (zero means 100ms, except `ForExec`, which defaults to
250ms). For `ForLog`, the poll interval is the delay before reopening a
stream that ends before the pattern is found. `ForAll` and `ForAny` have
no composition-wide timeout by default; a positive `WithStartupTimeout`
bounds the whole composition, while zero or a negative value leaves it
unbounded and lets each child strategy's timeout apply.

The current development checkout also adds `wait.ForHealthcheck()` for
Docker's `HEALTHCHECK` status and `HTTPStrategy.WithResponseMatcher` for
bounded response-body checks. The healthcheck strategy is Docker-only; the
Apple backend rejects it before creating a container.

Waiting fails fast when the container is stopping, stopped, or paused.
Created, restarting, unknown, and transient inspect states retry under the
timeout; transient log stream open/EOF failures are reopened, while a
terminal log-stream error is returned. A successful marker is accepted only
after a bounded final lifecycle observation reports `Running`; `ForLog` is for
long-lived services, not one-shot job completion. `ForLog` counts occurrences
across reconnects after de-duplicating the replayed log prefix. A failed wait rolls
the container back with a tail of its logs attached to the error.

For custom strategies, `wait.Target` retains its original `Running` method.
Implement the optional `wait.StateTarget` interface when the target can
distinguish transient startup states; built-in strategies use it automatically
and fall back to `Running` for compatibility. Docker healthcheck waits also
use the optional `wait.HealthTarget` interface; `ForHealthcheck` returns a
`*wait.ConfigError` for targets that do not implement it.

On Docker, a successful TCP connection to a published port does not prove
that the container process listens: `docker-proxy` or Docker Desktop's port
forwarder completes the handshake first. `ForListeningPort` and
`ForExposedPort` therefore follow a successful host connection with a
container-side check that the declared container port is in `LISTEN` state,
read from `/proc/net/tcp` and `/proc/net/tcp6` through `exec`. When that check
cannot run — Apple Container (which reaches the VM endpoint directly), an
image without `cat` such as distroless, or another unsupported target — the
strategy falls back to the host connection alone. That fallback is
best-effort: it restores the historical host-only behavior because the
container-side check is unavailable, not because the port is known to be
ready. Use `WithHostOnly()` to skip the container-side check explicitly.

`ForListeningPort` and `ForExposedPort` are TCP-only readiness probes.
`ForListeningPort` rejects `/udp` and malformed port specifications with a
`*wait.ConfigError` before probing. `ForExposedPort` selects the first TCP
port declared via `WithExposedPorts`, falling back to the first TCP
published port when there is no exposed TCP declaration. A UDP-only
declaration is therefore never passed to a TCP dial.

| API | TCP | UDP |
|---|---|---|
| `WithExposedPorts` / `WithPublishedPort` | Accepted | Accepted for endpoint/publish configuration |
| `wait.ForListeningPort` | `PORT` or `PORT/tcp` | `*wait.ConfigError` before probing |
| `wait.ForExposedPort` | First TCP exposed port, else first TCP published port | Skipped; no TCP declaration yields `ErrPortNotExposed` |

### Stop timeouts

A nil `Container.Stop` timeout uses the backend CLI's default grace period.
A non-nil timeout must be non-negative; a negative value returns a
`*container.ValidationError` before any CLI call. Both backends accept whole
seconds, so a positive sub-second timeout is rounded up (never down), zero
requests immediate termination, and a value whose rounded seconds exceed the
backend limit (`math.MaxInt32` seconds for both checked CLIs) is rejected
before the CLI is called. The timeout is also added to the operation's
context budget.

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

`Logs` and a supported `LogsWithOptions` call return a finite snapshot
because the CLI process finishes, but neither method imposes a byte limit
by itself. `Logs` requests all available output. `LogsOptions` is
backend-specific: Docker supports both `Tail` and `Since`. The checked
Apple Container CLI versions use `-n` for a tail and have no `--since`
option, so the Apple backend maps `Tail` to `-n` and rejects `Since` with
`ErrUnsupportedCapability`. A requested window can still produce output
whose size depends on the container, and `LogsWithOptions` does not add a
byte cap. `FollowLogs` returns a streaming `io.ReadCloser`; close it or
cancel its context to stop the backend CLI. `ForLog` uses `FollowLogs`,
while `Logs` does not follow new output. `LogsOptions` and
`LogsWithOptions` are current-development APIs, not part of `v0.2.0`.
`LogsOptions.Tail` and `MaxBytes` are validated as non-negative, and
`PruneReuseGroup` uses the same reuse-group grammar as `WithReuseGroup`.

The example below is Docker-specific. It also shows the log stream, exec
options, and public error symbols added after `v0.2.0`; it is not an Apple
`LogsWithOptions` example.

```go
package docexample

import (
    "context"
    "errors"
    "time"

    container "github.com/hirokazumiyaji/container-go"
)

func DockerLogOptions(ctx context.Context, ctr *container.Container) error {
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

These error checks are path-specific: a liveness-classified error keeps the
original `*CLIError` in the `ErrSystemNotRunning` chain, so `errors.As`
still finds it. For Docker, an inspect response that contains no entry
matching the requested ID or name returns `ErrContainerNotFound`; empty
output is also treated as not-found, while malformed output is a decode
error rather than a silent no-match.

`Terminate` treats a recognized backend not-found CLI failure as success,
including an inspect result with no matching target, so its idempotence
claim covers not-found and successful deletion. A malformed inspect response
is an error rather than a silent success, and unrelated delete failures
still propagate.

`ForListeningPort` and port declarations have different protocol support:

| API | TCP | UDP |
|---|---|---|
| `WithExposedPorts` / `WithPublishedPort` | Supported | Supported |
| `wait.ForListeningPort` | `6379` or `6379/tcp` | `*wait.ConfigError` before probing |

`ForListeningPort` also returns `*wait.ConfigError` for malformed port
specifications. Use `errors.Is(err, wait.ErrInvalidConfiguration)` to
classify either configuration error without matching its message.

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
process. On Linux, a numeric group signal additionally requires a
`waitid(WSTOPPED|WNOWAIT)` stopped-state check; if that ownership proof is lost,
termination uses the pidfd directly and never signals the former PGID. The
direct child is also killed after a successful group signal because it may have
changed process groups. Other Unix targets conservatively use the direct child
handle. Once that child is reaped, no former numeric process-group ID is used.
On Windows, the lifecycle retains an `OpenProcess` handle and terminates only
the direct child with `TerminateProcess`. Detached or reparented descendants
are outside that guarantee. Neither Unix nor Windows claims remote
container-process termination.
Windows-specific lifecycle tests are build-constrained. The development
environment cross-compiles and vets the Windows packages but cannot execute
Windows process-handle runtime tests, including the exit-259 active-child case.

Neither supported backend CLI exposes a common kill operation for an exec
instance. When active-child evidence is available and a context error actually
races with a launched command, Exec returns an `*ExecTerminationError`
(`errors.Is(err, ErrExecTerminationUnsupported)`) instead of claiming that
the container-side process stopped. A conservative direct-handle fallback may
return the context error without that remote-termination claim. A successful
already-finished child termination is not active-process evidence. Classification does not depend on the local exit status: in
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

### Streaming logs

`FollowLogs` returns an `io.ReadCloser` after the backend CLI starts. Read
until EOF; if the CLI later exits unsuccessfully, the terminal error (and
its `CLIError` details) is returned by `Read`, not by `FollowLogs`. Calling
`Close` or cancelling the context is an intentional termination; a
subsequent `Read` may return EOF or the context error instead. `wait.ForLog`
checks the optional terminal status before treating a matching line as
readiness, so a merged CLI diagnostic cannot mask a terminal failure.

The library always waits for and reaps the direct CLI child. On Unix-like
systems, Close and context cancellation make a best-effort attempt to
`SIGKILL` the CLI's process group after a stopped-state check, then kill
the direct child as well. Helpers that remain in that group can therefore
be terminated; they are not reaped. If the stopped-state check fails,
termination uses the direct child (pidfd on Linux) and never signals the
former PGID. Once the child is reaped, no former numeric process-group ID
is used. On Windows, Close and context cancellation use the retained
process handle for the direct CLI child only; descendants are not
terminated here. Other supported platforms cover only the direct child.
Do not rely on descendant cleanup when a CLI deliberately detaches helpers.

## Image pulls

When `Run` needs to create a container, it applies an explicit pull policy
before starting it:

- `PullMissing` (the default) inspects the local image store and runs an
  explicit pull only when the image is absent.
- `PullAlways` requests an explicit pull for every Run attempt,
  including a `WithReuse` attach before the shared container is returned.
- `PullNever` is a strict no-fetch policy on Docker: it passes
  `--pull=never` and returns `ErrImageNotFound` when the image is absent.
  Apple Container has no equivalent run-time switch, so `Run` rejects the
  policy there with `ErrPullNeverUnsupported`.

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
    "testing"

    container "github.com/hirokazumiyaji/container-go"
)

func PullPolicy(ctx context.Context, t testing.TB) {
    always, err := container.Run(ctx, "redis:7-alpine",
        container.WithPullPolicy(container.PullAlways))
    container.Cleanup(t, always)
    if err != nil {
        t.Fatal(err)
    }

    never, err := container.Run(ctx, "redis:7-alpine",
        container.WithPullPolicy(container.PullNever))
    container.Cleanup(t, never)
    // Docker reports an absent image as ErrImageNotFound; Apple Container
    // rejects PullNever with ErrPullNeverUnsupported.
    if errors.Is(err, container.ErrImageNotFound) ||
        errors.Is(err, container.ErrPullNeverUnsupported) {
        return
    }
    if err != nil {
        t.Fatal(err)
    }

    _ = container.Pull(ctx, "redis:7-alpine")
}
```

`PullNever` is a strict no-fetch contract only on the Docker backend. Apple
Container's `container run` has no switch that prevents its own image
resolution, so this library rejects the policy before any CLI call rather than
silently allowing a registry fetch. Use `PullMissing` (the default) or
`PullAlways` as the documented Apple fallback; `PullMissing` may still pull an
absent image and is not a no-network guarantee.

The image-existence check is a daemon round trip on every `Run`. When the
backend's store is known to be stable for the length of the test run,
`WithImagePresenceCache` reuses a "present" answer for a TTL so later
`Run`s of the same image skip it. Create the option once and reuse it —
each call to `WithImagePresenceCache` owns a distinct cache:

```go
package docexample

import (
    "context"
    "time"

    container "github.com/hirokazumiyaji/container-go"
)

func ImagePresenceCache(ctx context.Context) {
    presence := container.WithImagePresenceCache(5 * time.Minute)
    _, _ = container.Run(ctx, "redis:7-alpine", presence)
    _, _ = container.Run(ctx, "redis:7-alpine", presence) // skips image inspect
}
```

It is off by default, and only "present" answers are cached: caching an
absence would have to be invalidated by the pull it triggered. An image
removed out of band is not re-pulled until the entry expires, so callers
who need certainty on every `Run` should leave it off. On Docker,
`PullNever` always inspects regardless, since its contract is to fail when
the image is absent.

## Run options

`WithCPUs(v float64)` accepts a fractional CPU quota such as `0.5` or
`1.25` and passes it to Docker as `--cpus`. Non-positive and non-finite
values are rejected while options are evaluated. Apple Container's
`--cpus` takes an integer, so a fractional value is rejected with
`ErrUnsupportedCapability` while whole numbers keep working.

`WithMemory(size string)` and `WithShmSize(size string)` share the same
integer with an optional `K`/`M`/`G`/`T`/`P` suffix grammar and reject
zero, malformed, and overflowing values before any backend call.
`WithShmSize` passes Docker and Apple Container `--shm-size`.

The other options map directly to run flags: `WithPrivileged`
(`--privileged`), `WithCapAdd` / `WithCapDrop` (`--cap-add` /
`--cap-drop`; names are uppercase with an optional `CAP_` prefix or
`ALL`, the caller's spelling is passed through, and duplicates are
ignored regardless of prefix),
`WithUlimit(name, soft, hard)` (`--ulimit name=soft:hard` with Docker's
lowercase name set; `-1` means unlimited), `WithExtraHost(host, ip)` (`--add-host host:ip`,
with `host-gateway` accepted and duplicate or conflicting hosts
rejected), and `WithHostname` (`--hostname`, an RFC 1123 hostname of at
most 63 bytes). String values that start with `-` or contain control characters
are rejected.

Apple Container supports `--shm-size`, `--cap-add`, `--cap-drop`, and
`--ulimit` and receives the same flags. It has no equivalent for
privileged containers, extra hosts, or `--hostname`, so `WithPrivileged`,
`WithExtraHost`, and `WithHostname` return `ErrUnsupportedCapability`
before any image or container command.

These options are part of the `WithReuse` compatibility identity: a
reused container must report matching CPU, shm, privilege, ulimit,
extra-host, and hostname settings. Capabilities use a requested-subset
rule: every capability the caller asked to add or drop must be present,
while a caller that requests no capability changes accepts any inspected
set. `Run` refuses to attach when these checks fail.

### Apple capability validation

The Apple backend checks the CLI's static limits before it resolves an image or
creates a container. Apple runs Linux images only. `WithPlatform` must use
Apple's `os/arch[/variant]` grammar: bare `linux` and variants not defined for
the selected architecture are rejected. A bare `armel` selector means
`v6`, matching `linux/armel/v6`; bare `arm`/`armhf` mean `v7`. When no platform option is supplied,
`CONTAINER_DEFAULT_PLATFORM` is resolved into the effective platform and used
consistently for image inspection, pull flights, pulls, and `run`; an explicit
`WithPlatform` value takes precedence. This is Apple CLI configuration, so
Docker does not consume it; the Apple-only grammar is not applied there either.

The opt-in live matrix requires both `CONTAINERGO_BACKEND=apple` and
`CONTAINERGO_APPLE_LIVE=1`; it uses unique names and ownership labels and
never deletes a container whose label does not match. Run it with:

```sh
CONTAINERGO_BACKEND=apple CONTAINERGO_APPLE_LIVE=1 \
  go test -tags integration -run TestIntegrationAppleCapabilityMatrix ./...
```

Apple container names must be 2–63 characters and match
`^[a-zA-Z0-9][a-zA-Z0-9_.-]{1,62}$`. Network names are lowercase
1–63-character names (the public `WithNetwork` option
intentionally does not accept comma-separated MAC/MTU properties). Memory is at
least 200 MiB and uses the same integer with an optional `K`/`M`/`G`/`T`/`P`
suffix as `WithMemory`; invalid units and overflow are rejected. Published
ports must be 2–65535, and Apple accepts at most 64 published-port descriptors.
`WithExposedPorts` remains a library-side declaration and is not turned into an
Apple `--publish` flag.

Apple supports `--shm-size`, `--cap-add`, `--cap-drop`, `--ulimit`, and
integer `--cpus`, so `WithShmSize`, `WithCapAdd`, `WithCapDrop`,
`WithUlimit`, and whole-number `WithCPUs` values are passed through.
`WithPrivileged`, `WithExtraHost`, `WithHostname`, and a fractional
`WithCPUs` value are rejected with `ErrUnsupportedCapability` before the
CLI runs.

## Image resolution and pinning

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
reference when the descriptor is available. When Apple resolves an
unqualified input to a custom default registry, `Run` preserves that
canonical repository in the pinned reference and reuse comparison.
Reuse compares the resolved digest/ID identity, not the original tag,
and removes a newly-created reused container if its post-create identity
or port validation fails.

Apple has no runtime `--pull=never` switch. Before `Run` passes any
pinned Apple reference to `container run`, it verifies that exact
reference in the local store for `PullMissing` and `PullAlways`
(`PullNever` is rejected earlier with `ErrPullNeverUnsupported`). If it is
missing, the library performs an explicit exact-digest pull and verifies it again;
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
package docexample

import (
    "context"

    container "github.com/hirokazumiyaji/container-go"
)

func AllowMutableImageTag(ctx context.Context) {
    _, _ = container.Run(ctx, "redis:7-alpine",
        container.WithAllowMutableImageTag())
}
```

With that option, a mutable input may run the original tag; this
deliberately retains the tag-replacement window and is not an identity
guarantee. Apple `image@sha256:...` (including `image:tag@sha256:...`)
is a pinned address and is never downgraded by this option. The option
never downgrades a bare digest or a Docker image-ID-shaped value.
Pinning cannot make a mutable registry
tag's pull-to-inspect resolution atomic when another actor can modify
the shared backend. Prefer a caller-supplied `image@sha256:...` when
the backend provides an atomic address and treat its identity metadata
as the compatibility boundary. Locally built Apple images may require
`WithAllowMutableImageTag` when no locally addressable digest reference
exists.

## Cleanup contract

The normal, rollback, failed-create, and abnormal-exit paths have
different error visibility.

1. **Normal cleanup**: `container.Cleanup(t, ctr)` registers
   `TerminateContainer` through `t.Cleanup`. A termination error is sent to
   the test's log. `container.TerminateContainer(ctx, ctr)` is the
   deferred-style variant and returns its error to the caller. It uses the
   caller's context; `Cleanup` and `StrictCleanup` bound the complete
   teardown, including the name-lock wait, with a `terminateTimeout`
   context (30 seconds by default). Both helpers are nil-safe.
2. **Post-create rollback**: if a non-reuse `Run` fails while copying files
   or waiting for readiness, `Run` calls `Terminate`. If that deletion
   fails, the returned error includes the original failure and a message
   saying that the container was left behind. If a post-create
   environment-file cleanup still fails, `Run` returns the usable handle
   together with the joined error; the caller must inspect or explicitly
   terminate that handle.
3. **Failed create**: if the backend `run` command itself fails, the
   best-effort `cleanupFailedCreate` path only inspects and deletes a
   container carrying this process's managed/session labels. The creation
   label must be present and match this run; an absent or mismatched label
   is never deleted. That is a best-effort replacement guard, not a general
   transactional guarantee. Its lock, inspect, and delete errors are not
   joined to the returned `Run` error. A name conflict is never cleaned up
   this way.
4. **Abnormal exit**: when the parent process exits and its reaper pipe
   closes, a best-effort watchdog attempts force-deletion. An uncaught
   process-terminating panic, `SIGKILL`, and `os.Exit` close the pipe; a
   recovered panic does not. The reaper does nothing while the parent is
   alive. It is not a transactional guarantee, and registration or per-entry
   deletion errors are not returned to `Run`.

The reaper is an external `/bin/sh` child, is unavailable on Windows, and
is started lazily only for a real CLI container on the non-reuse path. Every
created container is pre-registered under its name and creation generation
before the backend create starts; after a successful create, the library
verifies the managed/session/creation labels and then retains Docker's full
64-hex immutable ID as an additional reaper entry. Name-addressed entries
(Apple always, and a Docker create until `run` returns its ID) are inspected
and deleted under the same per-name `flock` as ordinary generation-checked
deletion. Docker ID entries are deleted by ID without a name lookup. If a
backend returns no parseable ID, the name-and-generation path remains the
deletion target.

Repeated reaper spawn failures are logged once after the retry limit;
delete failures are ignored by the shell. Do not use reaper behavior as a
cleanup acknowledgement.

`CONTAINERGO_KEEP=1` is a process-wide diagnostic switch. It disables
automatic deletion by `Cleanup` and `TerminateContainer`, skips reaper
registration, and suppresses automatic rollback on failed creates, `WithFiles`
copies, and wait strategy failures, leaving containers in place for
inspection. It does not suppress explicit `Container.Terminate`, and `Prune`
or `PruneReuseGroup` can still delete containers. `WithReuse` also keeps its
stopped-container replacement behavior, so a matching stopped reuse container
can still be deleted and recreated. Treat the variable as a debugging aid,
not a global deletion lock.

`container.Prune(ctx)` removes containers this library created that are
selected by the active backend's filter. Apple selects managed containers
in the stopped state. Docker selects managed containers in the exited and
dead states. The filter does not select running or created containers. On
Apple, each candidate is re-inspected and re-verified (generation, session,
managed label, and stopped state) under the stable per-name lock before
deletion.

Package-level `Prune` intentionally takes only backend-neutral
`PruneOption` values and has no Docker target options. To prune a specific
Docker daemon or context, construct a `Client` with `WithClientDockerHost`
or `WithClientDockerContext` and call `Client.Prune`. `Pull` is different:
package-level `Pull` accepts its own `WithPullDockerHost` and
`WithPullDockerContext` options for a single fetch, while `Client.Pull`
rejects them because the Client pins its target.

`Container.Kill(ctx, signal string)` takes exactly one signal. An empty
string selects the default SIGKILL. A non-empty signal is passed to the
backend unchanged; multiple signals are now rejected at compile time.

```go
package docexample

import (
    "context"
    "testing"

    container "github.com/hirokazumiyaji/container-go"
)

func Lifecycle(ctx context.Context, t testing.TB, ctr *container.Container) {
    // The caller's context bounds TerminateContainer.
    defer container.TerminateContainer(ctx, ctr)

    if err := ctr.Kill(ctx, "SIGTERM"); err != nil {
        t.Fatal(err)
    }
    if err := ctr.Kill(ctx, ""); err != nil { // empty string means SIGKILL
        t.Fatal(err)
    }
}
```

`container.WithPruneOlderThan(d)` narrows that selection to candidates the
backend reports as created before now-d. It is useful for a sweep on a
shared or remote daemon because a concurrent job's recently stopped
containers are left alone. A candidate whose creation time the backend
does not report is also left in place. Ordinary `Prune` still selects only
stopped managed containers, so a running leftover is not removed until it
stops.

### Windows and remote-daemon cleanup

The reaper is a local `/bin/sh` child. It is unavailable on Windows, and it
cannot act after the local process tree or runner VM is destroyed, for
example when CI cancels a job or `go test -timeout` kills the process.
Containers on a remote Docker daemon also outlive that local environment.
Registration and deletion are best effort even when the reaper runs; do not
treat the reaper as a cleanup acknowledgement.

For those environments, sweep stopped managed containers from `TestMain`
before and after the suite. Call the helper below from `TestMain` and exit
with its result (`func TestMain(m *testing.M) { os.Exit(pruneAndRun(m)) }`):

```go
package docexample

import (
	"context"
	"testing"
	"time"

	container "github.com/hirokazumiyaji/container-go"
)

func pruneAndRun(m *testing.M) int {
	ctx := context.Background()
	// Reap leftovers that are old enough not to belong to a concurrent run.
	_, _ = container.Prune(ctx, container.WithPruneOlderThan(6*time.Hour))
	code := m.Run()
	// Remove this run's stopped containers.
	_, _ = container.Prune(ctx)
	return code
}
```

`Prune` removes only stopped managed containers, from any session; running
and created containers are left alone. On a shared or remote daemon, keep
the age filter for the pre-suite sweep so another job's recently stopped
containers survive, and use plain `Prune` after the suite only when the
daemon is dedicated to this job. The same calls work against a remote
daemon through the process environment or a `Client` pinned to that daemon.

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
- An existing container must carry the managed marker, the `WithReuse`
  marker, a valid creation generation, and (on Docker) a valid immutable
  UID. Its image must be compatible. Name conflicts from a racing create
  are treated as success and attach.
- A stopped container is considered for deletion and recreation only after
  those ownership checks and the image check pass. A running container that
  never becomes ready is left alone and returns an error.
- Image compatibility uses the resolved digest or image ID (never the
  original mutable tag). Docker also requires the requested network identity
  to match; omitted `WithNetwork` means the daemon default, and inspect's
  `default` mode is resolved against `NetworkSettings.Networks` (`bridge` on
  Linux, `nat` on native Windows). `host`, `none`, and named networks are
  not wildcards. Reuse re-inspects before compatibility checks and again
  after readiness before returning the handle: Docker uses the immutable
  UID and Apple re-checks the creation generation, so a replacement during
  the wait returns an error instead of being adopted.
- Port compatibility is backend-specific: Docker compares auto-published
  exposed-port bindings and explicit publications. Apple compares explicit
  published bindings, but cannot compare `WithExposedPorts` declarations
  because the library's Apple inspect model does not retain them. On Apple,
  each new handle uses its own exposed-port declaration against the shared
  container IP, so use distinct names when those declarations must be
  isolated.
- `env` / `cmd` / `mounts` differences attach silently by design; use
  distinct names when they matter. `WithFiles` is copied for every reuse
  caller, including attach callers; a failed attach copy returns an error
  without deleting the shared container. `PullAlways` fetches the image for
  every caller before attach.
- Each creation carries a generation label; `Terminate`, the
  stopped-recreate path, and the watchdog reaper refuse to delete a replaced
  generation. Docker handles delete by the immutable ID printed by
  `docker run` (or verified from inspect), so a same-name replacement is not
  the deletion target. Apple name-addressed deletion requires the generation
  and runs inspect plus delete under the per-name `flock`.
- `Cleanup`, `TerminateContainer`, and the watchdog reaper skip reused
  handles so other packages keep working. Explicit `ctr.Terminate` still
  removes the shared container — only do that when nothing else needs it.
- `CONTAINERGO_KEEP=1` does not change this reuse contract: a stopped
  matching reuse container can still be deleted and recreated, and
  `PruneReuseGroup` can still remove the group.
- The per-name `flock` protects the generation-checked ordinary
  `Terminate`/failed-create cleanup paths, Apple `Prune` and
  `PruneReuseGroup`, and reaper entries that address a name. Docker reaper
  entries that use the full immutable ID are not name-addressed and need no
  lock. Treat external uncoordinated CLI operations as uncoordinated.
- `container.PruneReuseGroup(ctx, "integration")` force-removes every
  container tagged with that group (CI teardown). The group is a label,
  not part of the reuse key. On Apple Container, it applies the same fresh
  candidate inspection (generation, session, managed/reuse/group labels,
  running/stopped state) under the per-name lock before deletion.
  Ordinary `Prune` uses the backend filter described above; on Docker that
  means exited and dead containers.

This library does not reset application data between tests. Prefer a
per-test key prefix, separate DB schemas/namespaces, or an `Exec` setup
step (`FLUSHALL`, `TRUNCATE`, …) before assertions.

## Security notes

- Every CLI call is an argv vector; no shell is involved. The one shell
  script (the reaper) is a fixed string that receives container IDs only as
  validated stdin data. Name-addressed entries use the shared `nameRE` token
  rule, while Docker's full 64-hex immutable IDs are accepted separately;
  Apple's stricter 2–63-character name check runs before container creation.
- `CopyToContainer` opens regular files and directories without following
  final links, then snapshots the opened handles into a per-user private
  staging path before invoking the backend. `WithFiles` is regular-file-only
  and rejects directories, symlinks/reparse points, and special files when the
  option is applied, before any backend call. Snapshots are
  bounded by `MaxCopyToContainerSize` (64 MiB of file bytes),
  `MaxCopyToContainerEntries` (10,000 entries), `MaxCopyToContainerDepth`
  (128), and `MaxCopyToContainerMetadataSize` (8 MiB of visited source
  paths). The snapshot intentionally preserves names, bytes, and Unix
  permission bits only; timestamps, ownership, setuid/setgid bits, ACLs,
  extended attributes, alternate data streams, sparse layout, and hardlink
  identity are not preserved.
- `WithFiles` accepts exactly one of `HostPath` or a non-nil `Content` byte
  slice; setting both or neither is a `ValidationError` when the option is
  applied. An empty non-nil slice copies an empty file. `File.Mode` overrides
  the Unix permission bits on the private staged copy: zero preserves host
  permissions and defaults content to `0600`, while a non-zero value must
  contain only permission bits (type, setuid, setgid, and sticky bits are
  rejected). `CopyContentToContainer` applies the same staging and mode rules
  to an existing handle. Content never appears in argv, errors, or
  diagnostics, and its staging file is removed with the staging directory on
  success, failure, and cancellation.
- On Unix, environment variables are passed through a `0600` file in a
  validated, current-user-owned `0700` directory under the canonical
  `os.UserCacheDir()` path. The library resolves existing symlinked ancestors
  once, rejects `..` and untrusted writable ancestors, and never uses
  `TMPDIR`. The root marker is created with an exclusive, locked hand-off;
  a partially written marker is repaired only while the private root is empty.
- Stale cleanup removes only exact, owned library children. A staging
  directory is eligible after `24h`; marker-before-lock and tombstoned
  partial-removal states are self-healing. An initialized directory is never
  aged out: its unlocked writer lock is the liveness proof. If an entry is
  unmarked, replaced, or contains an unexpected child, cleanup fails closed
  and leaves it for manual inspection and removal. Cleanup failures are
  returned and retried before `Run`, reuse creation, or `Exec` returns.
- Windows cannot provide the claimed per-user secrecy with Go `chmod`, so
  `Run`/`Exec` fail with `ErrEnvFileUnsupported` when a non-empty environment
  map requires an env file. Other Windows operations remain supported.
- Env keys must be valid, non-empty UTF-8 without `=`, Unicode whitespace or
  controls, a leading `#`, or a leading BOM. Values must be valid UTF-8
  without Unicode controls, NUL, CR/LF, U+2028, or U+2029. Other non-control
  Unicode, spaces, and `=` remain valid values. Rejecting controls (including
  tab) and invalid UTF-8 is an intentional compatibility change: a backend may
  accept a value that the library refuses to put in a line-delimited env file.
- Registry credentials are never handled by this library; use
  `container registry login` for Apple Container or `docker login` for
  Docker. The backend CLI owns the resulting credentials and registry
  context.
- `Run` passes the verified identity returned by image inspect (or a
  Docker image ID) to the backend, so a later local tag reassignment
  does not change that create. Apple descriptor-backed references are
  checked for local addressability before create and the resulting
  container identity is verified before return. Caller-supplied
  `name@digest` references remain pinned; `WithAllowMutableImageTag`
  applies only to mutable tags and explicitly authorized compatibility
  fallbacks. `container run` is never
  used as an implicit fetch. This is a backend/API guarantee, not a
  claim that every tag-to-registry operation is atomic: a mutable tag
  can still be replaced before the post-pull inspect, and an
  identity-less backend requires the explicit compatibility fallback.

### Legacy reaper staging files

The current watchdog streams inspect output through a structural filter and
never writes the raw output to disk. Older versions used an un-namespaced
`mktemp` file, so a reaper killed during inspect can leave a file containing
container environment data. Those legacy names cannot be safely attributed
to this library. To investigate or clean them up, first stop all
container-go and watchdog processes, inspect the per-user temporary directory
(the effective `TMPDIR`, not an assumed `/tmp`) using a metadata-only listing
restricted to regular files owned by that user and the affected time window.
Do not grep file contents into a terminal, follow symlinks, or run a broad
recursive delete. Remove only files that you can positively attribute to the
affected run. Treat any credentials that may have appeared in inspect output
as exposed and rotate them; deleting a stale file does not revoke a secret.


## Differences from testcontainers-go

Not supported (Apple Container has no equivalent, or out of scope):

| testcontainers-go | Here |
|---|---|
| `wait.ForHealthCheck` | Docker only (`wait.ForHealthcheck`); Apple Container has no healthcheck |
| Building from a Dockerfile | Out of scope (use `container build` / `docker build` yourself) |
| Ryuk reaper container | Replaced by the local watchdog reaper process, with the limitations described above |
| Random host port mapping | Apple backend connects to the container IP directly; Docker backend auto-publishes to random loopback ports |
| Network/volume creation and lifecycle management | Out of scope for now; `WithNetwork` attaches an existing network and `WithMounts` accepts bind, named-volume, and tmpfs mounts. A bind mount against a remote Docker daemon is rejected with `ErrUnsupportedCapability`; Windows host paths are accepted for local Docker. |
| `GenericContainerRequest.Reuse` | `WithReuse` + `WithName`: cross-process get-or-create with mandatory re-wait and no Cleanup/reaper ownership |

## Development

```text
make test                # unit tests (no backend needed)
make vet
make integration         # integration tests (skips bench/singleflight); each backend skips if unavailable
make integration-docker  # Docker-backend integration tests only
make bench-integration   # pull-heavy bench and singleflight (bench module needs Go 1.26+)
```

Integration tests pull library images via `public.ecr.aws/docker/library/...`
to avoid anonymous Docker Hub rate limits. Set `CONTAINERGO_BACKEND=apple`
or `docker` to skip the other backend.

GitHub Actions runs unit tests and race tests on `ubuntu-latest` with Go
1.26.0 and the stable Go release, then runs lint and `govulncheck`. It
also runs the Docker integration matrix on `ubuntu-latest`; Apple
Container integration remains a local test because the hosted runners do
not provide that service. The non-integration documentation test extracts
all fenced code blocks from both language versions of this README and the
design document, compares paired blocks after removing comments, and
compiles the Go blocks in a temporary module with a quoted local
`replace`. The tagged examples under `examples/` exercise a real backend.

The cross-platform job also runs unit tests and tagged vet on macOS and
Windows, including Windows-specific environment-file and process tests.

Design document: [docs/design.md](docs/design.md) (日本語版:
[docs/design.ja.md](docs/design.ja.md)). The implementation phases in that
document are historical; the current API and behavior are maintained in
the source and tests.

Contributing: [CONTRIBUTING.md](CONTRIBUTING.md). Security reports:
[SECURITY.md](SECURITY.md).

## License

MIT
