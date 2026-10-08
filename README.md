# container-go

日本語版: [README.ja.md](README.ja.md)

A [testcontainers](https://testcontainers.com/)-style Go library for
[Apple Container](https://github.com/apple/container) and Docker: run
throwaway containers from Go tests, with zero third-party dependencies.

## Version scope

The latest tagged release is `v0.2.0` (2026-09-02). This checkout is
development after that tag. The install command below is for the released
API; it does not install the development-only APIs documented below.

The root library checkout requires Go 1.23 or later. The nested
`bench/` module used by `make bench-integration` requires Go 1.25 or
later. The `v0.2.0` module requires Go 1.27 or later.

| API or behavior | `v0.2.0` | Current development checkout |
|---|---|---|
| `Run`, core options, lifecycle, endpoints, `Exec`, `Logs`, `FollowLogs`, copy, pull policies, reuse, cleanup, backend selection, and the original wait strategies | Available | Available |
| `LogsOptions` and `LogsWithOptions` | Not available | Available |
| `wait.ForHTTP` header, authentication, TLS, and custom-client setters | Not available | Available |
| Exported `wait.AllStrategy` / `AnyStrategy` and composite `WithStartupTimeout` | Not available | Available |
| Root `CLIError`, `ErrContainerNotFound`, and `ErrGenerationReplaced` | Not available | Available |
| Generation-safe deletion, post-`v0.2.0` endpoint hardening, and the current reaper hardening | Not part of the `v0.2.0` contract | Partially implemented; see the follow-up issue boundaries below |

Unless a section explicitly says “current development”, descriptions of
behavior describe this checkout rather than the tagged `v0.2.0` release.
The code examples are standalone files and are compile-checked by the
repository's documentation test.

This checkout is not a merge of the follow-up issue branches that harden
backend-specific behavior. In particular:

- Apple `LogsWithOptions` currently uses Docker-style `--tail` and
  `--since` arguments. Apple Container uses `-n` for a tail and has no
  `--since` option; #82 must be applied before Apple support can be
  documented as a capability.
- Endpoint resolution caches the first successful inspect, including an IP
  address and host bindings. Those values can change and may be stale; #85
  tracks refreshing dynamic endpoint data.
- Reuse does not require every ownership/generation label on an existing
  container and does not re-check the generation after readiness. A missing
  generation can reach a name-based delete path; #83 and #84 track those
  fail-open paths.
- A Docker handle currently uses an immutable ID printed by `docker run` for
  deletion, but an ID recovered from inspect still lacks target validation;
  other backend operations address the logical name. A stale handle can
  therefore inspect or modify a same-name replacement; #74 tracks the
  operation-target fix and #103 tracks Docker inspect target validation.
- Docker `Prune` currently selects exited containers, not containers in the
  dead state; #113 tracks dead-state coverage.
- Apple `Prune` and `PruneReuseGroup` currently use a list-to-delete path
  without fresh candidate revalidation or the per-name lock; #98 tracks
  that Apple cleanup race.
- `WithReuse` attach callers currently ignore `WithFiles` and `PullAlways`;
  #94 tracks those creation-only side effects.
- Windows Docker bind sources and remote Docker bind-source semantics are
  not supported by the current validation path; #76 tracks host-path and
  remote-mount handling.
- `ForListeningPort` and `ForExposedPort` are TCP-only probes. UDP may be
  declared for endpoint configuration, but a UDP readiness request is
  currently passed to a TCP dial; #77 tracks protocol validation.
- `Stop` converts non-nil durations to whole seconds by truncation and does
  not reject negative or extreme values; #89 tracks timeout validation.
- Apple `PullNever` is currently a best-effort backend precheck, not a
  no-fetch guarantee; #112 tracks strict Apple capability handling.
- Wait timeout/cancellation errors are not uniformly preserved in the
  error chain; #92 tracks the built-in strategy error contract.
- A failed liveness probe currently flattens the original `*CLIError` into
  an `ErrSystemNotRunning` message; #104 tracks error-chain preservation.
- A successful inspect response with no matching target may return a
  generic error rather than `ErrContainerNotFound`. For Docker, empty or
  malformed inspect data can take that path, and the current parser does
  not verify that a returned object matches the requested ID or name; a
  valid but mismatched object is not a reliable no-match signal. #103
  tracks that classification gap.
- Public option validation is partial: negative log tails, zero memory,
  unknown mount types, and reuse-group grammar are not uniformly rejected;
  #102 tracks the typed validation work.
- The reaper can stage full inspect output, including environment data, in
  a temporary file; #111 tracks the staging exposure and cleanup gap.

The sections below describe these current limitations rather than the
behavior of those follow-up branches.

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
| Linux | Docker | docker CLI 29.x (29.7.2 is checked here) + running daemon |
| Windows | Docker | docker CLI 29.x (29.7.2 is checked here) + running daemon (no watchdog reaper; see below) |
| macOS | Apple Container | macOS 26+, Apple Silicon, [Apple Container](https://github.com/apple/container) 1.2.x with `container system start` done |
| Linux | Docker | docker CLI + running daemon; copy-out requires client/server 29.7.0+ |
| Windows | Docker | docker CLI + running daemon; copy-out requires client/server 29.7.0+ (no watchdog reaper; see below) |

Set `CONTAINERGO_BACKEND=docker` to use Docker on macOS (e.g. Docker
Desktop), or `CONTAINERGO_BACKEND=apple` to insist on Apple Container.

The library shells out to the backend CLI (`container` or `docker`) — no
cgo and no daemon API client. `DOCKER_HOST`, contexts, and registry auth
are handled by the docker CLI itself. The library uses only
`DOCKER_HOST=tcp://...` to select a remote Docker endpoint; a remote
Docker context is not detected.

Verified backend behavior (the CLI stderr wording and inspect JSON shapes
this library matches against):

| Backend | Evidence used by this checkout |
|---|---|
| Apple Container CLI | Source/help and inspect fixtures for 1.2.2 and 1.3.0 |
| Docker Engine / CLI | A 29.x-shaped inspect fixture; local development run 29.7.2 |
| Apple Container | 1.2.x–1.3.x |
| Docker Engine / CLI | 29.x; secure copy-out requires client/server 29.7.0+ |

These entries describe repository evidence, not an exhaustive
compatibility claim for every release in a range. Newer CLI releases may
change error text or JSON fields; see the stderr matchers at the top of
`engine_apple.go` / `engine_docker.go` and the fixtures under
`internal/inspect/testdata/` and `testdata/`.

`WithName` and name-addressed reaper entries use the shared library guard
`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$` (one to 63 characters). This is a
conservative safety rule, not a claim about the complete Docker or Apple
name grammar. Apple Container's CLI has the stricter 2–63-character rule
`^[a-zA-Z0-9][a-zA-Z0-9_.-]{1,62}$`; this checkout does not yet apply
that backend-specific preflight check (#112), so a one-character name can
pass library validation and still be rejected by Apple.

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
over ports here either. With a detected non-loopback
`DOCKER_HOST=tcp://...` daemon, an explicit `WithPublishedPort` bound to
loopback (`127.0.0.1:...`, `[::1]:...`) is rejected, since it would only
listen on the remote machine. Only
`DOCKER_HOST` is honored; a `docker context` pointing at a remote daemon
is not detected.
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

On this checkout, the first successful inspect is cached and reused by
endpoint-related methods. The cached record includes the container IP and
host-side bindings, even though both can change during the container's
lifetime. `State` performs a fresh inspect, but an endpoint result can
therefore be stale. Treat these values as a snapshot rather than as
immutable facts; issue #85 tracks the refresh fix. If Docker reports
multiple networks and no top-level address, network selection is
currently unspecified rather than a deterministic first-network
contract.

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
and fall back to `Running` for compatibility.

`ForListeningPort` and `ForExposedPort` are TCP-only readiness probes.
UDP may be declared for endpoint configuration, but the current
implementation does not reject `/udp` before probing: it is passed to a
TCP dial and may time out or reach an unrelated TCP listener. Malformed
port specifications are likewise retried until the wait ends. See #77.

| API | TCP | UDP |
|---|---|---|
| `WithExposedPorts` / `WithPublishedPort` | Accepted by the current option parser | Accepted for endpoint/publish configuration |
| `wait.ForListeningPort` | `PORT` or `PORT/tcp` | Not a UDP probe; a UDP declaration may be TCP-dialed |
| `wait.ForExposedPort` | Uses the first declared port, regardless of protocol | If that first declaration is UDP, it is still passed to a TCP dial |

### Stop timeouts

For a non-nil `Container.Stop` timeout, both current backends convert the
value to whole seconds by truncating fractional values; negative and
extreme durations are not rejected, and the duration is added to the
query budget without saturation. No library-side maximum is enforced;
backend-specific limits apply. Do not rely on sub-second, negative, or
near-maximum durations until #89 is applied.

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
option, while this
checkout still sends the Docker-style `--tail` and `--since` spellings to
Apple. `LogsWithOptions` is therefore not backend-neutral here; use `Logs`
on Apple until #82 is applied. The #82 change is intended to map `Tail` to
Apple's `-n` and reject `Since` as unsupported. A requested window can still
produce output whose size depends on the container, and `LogsWithOptions`
does not add a byte cap. `FollowLogs` returns a streaming `io.ReadCloser`; close it or
cancel its context to stop the backend CLI. `ForLog` uses `FollowLogs`, while
`Logs` does not follow new output. `LogsOptions` and `LogsWithOptions` are
current-development APIs, not part of `v0.2.0`.

Validation is not uniform in this checkout (#102): a negative
`LogsOptions.Tail` is treated as zero/all, `WithMemory("0")` is accepted
by the current parser, unknown `MountType` values are not rejected by
`WithMounts`, and `PruneReuseGroup` accepts a weaker grammar than
`WithReuseGroup`. These are current behaviors, not guarantees that the
backend will accept the resulting configuration.

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

These error checks are path-specific: a liveness-classified error may
flatten the original `*CLIError` into text (#104). For Docker, a
successful inspect response with an empty or malformed result can instead
return a generic parse error, and the current parser does not verify that
a returned object matches the requested ID or name; a valid but mismatched
object is not a reliable no-match signal (#103).

`Terminate` treats a recognized backend not-found CLI failure as success,
so its idempotence claim is limited to that case (and to a successful
delete). It does not normalize empty or malformed inspect output, and a
valid but mismatched Docker object is not rejected as a no-match by the
current parser. Neither case is covered by the idempotence claim, and
unrelated delete failures still propagate.

`ForListeningPort` and port declarations have different protocol support:

| API | TCP | UDP |
|---|---|---|
| `WithExposedPorts` / `WithPublishedPort` | Supported | Supported |
| `wait.ForListeningPort` | `6379` or `6379/tcp` | `*wait.ConfigError` before probing |

`ForListeningPort` also returns `*wait.ConfigError` for malformed port
specifications. Use `errors.Is(err, wait.ErrInvalidConfiguration)` to
classify either configuration error without matching its message.

## Image pulls

When `Run` needs to create a container, it applies an explicit pull policy
before starting it:

- `PullMissing` (the default) inspects the local image store and runs an
  explicit pull only when the image is absent.
- `PullAlways` requests an explicit pull for every new-container
  attempt. On a `WithReuse` attach, the current checkout does not run
  this side effect (#94).
- `PullNever` is backend-specific: the current checkout performs a
  best-effort precheck and returns `ErrImageNotFound` when it finds no
  image. Docker also passes `--pull=never` and therefore has the strict
  no-fetch path; Apple Container has no equivalent run-time switch, so
  its precheck is not a no-fetch guarantee (#112).

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
    if errors.Is(err, container.ErrImageNotFound) {
        return
    }
    if err != nil {
        t.Fatal(err)
    }

    _ = container.Pull(ctx, "redis:7-alpine")
}
```

The example registers cleanup before checking either `Run` error. On the
current checkout, `PullNever`'s `ErrImageNotFound` is backend-specific and
best-effort on Apple; it is not an unconditional no-fetch guarantee (#112).

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
   currently accepted in this best-effort path. That is not a general
   replacement guarantee; #83 tracks closing the missing-generation path.
   Its lock, inspect, and delete errors are not joined to the returned
   `Run` error. A name conflict is never cleaned up this way.
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

The current reaper stages the full `inspect` output in an un-namespaced
`mktemp` file and removes it on its ordinary completion or inspect-error
paths. If the reaper is killed, a file containing environment data can
remain. Before cleanup, stop container-go and reaper processes, then use a
metadata-only listing of regular files owned by the user in the effective
`TMPDIR`, restricted to the affected time window. Do not print or grep
file contents, follow symlinks, or run a broad recursive delete. Remove
only files positively tied to the affected run, and rotate credentials
that may have appeared in inspect output (#111). This is not a no-leak
guarantee.

`CONTAINERGO_KEEP=1` skips the automatic `Cleanup` and
`TerminateContainer` helpers and reaper registration. It does not suppress
explicit `Container.Terminate`, post-create rollback, or best-effort
failed-create cleanup, and `Prune` or `PruneReuseGroup` can still delete
containers. `WithReuse` also keeps its stopped-container replacement
behavior, so a matching stopped reuse container can still be deleted and
recreated. Treat the variable as a debugging aid, not a global deletion
lock.

`container.Prune(ctx)` removes containers this library created that are
selected by the active backend's filter. Apple selects managed containers
in the stopped state. Docker currently selects managed containers in the
exited state only, so a Docker container in the dead state is not removed
until #113 is applied. The filter does not select running or created
containers. On Apple, the current `Prune` and `PruneReuseGroup` list-to-delete
path does not re-inspect each candidate under the per-name lock before
deleting its name; a replacement can race that delete (#98).

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
- An existing container must carry the `WithReuse` marker and have a
  compatible image. Name conflicts from a racing create are treated as
  success and attach.
- A stopped container is considered for deletion and recreation only
  after the current reuse-marker and image checks pass. A running container
  that never becomes ready is left alone and returns an error.
- Image compatibility is checked on both backends. Port compatibility is
  backend-specific: Docker compares auto-published exposed-port bindings
  and explicit publications. Apple compares explicit published bindings,
  but cannot compare `WithExposedPorts` declarations because the
  library's Apple inspect model does not retain them. On Apple, each new
  handle uses its own exposed-port declaration against the shared
  container IP, so use distinct names when those declarations must be
  isolated.
- `env` / `cmd` / `mounts` differences attach silently by design; use
  distinct names when they matter. On the current checkout, `WithFiles`
  and `PullAlways` are applied only by the reuse creation path and are
  ignored by an attach caller (#94).
- Containers created by this checkout normally carry a generation label.
  For an existing reuse container, the current checks require the
  `WithReuse` marker and a compatible image, but do not require the
  managed or creation label. An empty generation can therefore reach a
  name-based delete path, and the current code does not perform a final
  generation re-check after readiness. Issues #83 and #84 track closing
  these fail-open paths. Until they are applied, do not treat reuse as a
  protection against an untrusted same-name replacement. A Docker handle
  that retains the full ID printed by `docker run` deletes by that ID;
  an ID recovered from inspect still depends on target validation, which
  the current Docker parser does not perform (#103). Other operations
  still use the logical name until #74 is applied.
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
- `CONTAINERGO_KEEP=1` does not change this reuse contract: a stopped
  matching reuse container can still be deleted and recreated, and
  `PruneReuseGroup` can still remove the group.
- The per-name `flock` protects the generation-checked ordinary
  `Terminate`/failed-create cleanup paths. It does not cover the current
  Apple `Prune`/`PruneReuseGroup` list-to-delete path, which lacks fresh
  candidate revalidation (#98), or the external reaper's separate
  inspect/delete window. Treat those paths as uncoordinated.
- `container.PruneReuseGroup(ctx, "integration")` force-removes every
  container tagged with that group (CI teardown). The group is a label,
  not part of the reuse key. On Apple, the current list-to-delete path has
  the same missing fresh revalidation/name-lock boundary as `Prune` (#98).
  Ordinary `Prune` uses the backend filter described above; on the current
  Docker backend that means exited containers, not dead ones.

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
| Network/volume creation and lifecycle management | Out of scope for now; `WithNetwork` attaches an existing network and `WithMounts` accepts mounts, with current Windows/remote bind-source limitations pending #76 |
| `GenericContainerRequest.Reuse` | `WithReuse` + `WithName`: cross-process get-or-create with mandatory re-wait and no Cleanup/reaper ownership |

## Development

```text
make test                # unit tests (no backend needed)
make vet
make integration         # integration tests (skips bench/singleflight); each backend skips if unavailable
make integration-docker  # Docker-backend integration tests only
make bench-integration   # pull-heavy bench and singleflight (bench module needs Go 1.25+)
```

Integration tests pull library images via `public.ecr.aws/docker/library/...`
to avoid anonymous Docker Hub rate limits. Set `CONTAINERGO_BACKEND=apple`
or `docker` to skip the other backend.

GitHub Actions runs unit tests and race tests on `ubuntu-latest` with Go
1.23.0 and the stable Go release, then runs lint and `govulncheck`. It
also runs the Docker integration matrix on `ubuntu-latest`; Apple
Container integration remains a local test because the hosted runners do
not provide that service. The non-integration documentation test extracts
all fenced code blocks from both language versions of this README and the
design document, compares paired blocks after removing comments, and
compiles the Go blocks in a temporary module with a quoted local
`replace`. The tagged examples under `examples/` exercise a real backend.

Design document: [docs/design.md](docs/design.md) (日本語版:
[docs/design.ja.md](docs/design.ja.md)). The implementation phases in that
document are historical; the current API and behavior are maintained in
the source and tests.

Contributing: [CONTRIBUTING.md](CONTRIBUTING.md). Security reports:
[SECURITY.md](SECURITY.md).

## License

MIT
