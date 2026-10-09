# container-go Design Document

日本語版: [design.ja.md](design.ja.md)

Created: 2026-08-18 (v0.2 backend section added 2026-08-19)
Last synchronized: 2026-09-25
Targets: Apple Container CLI 1.2.2 and 1.3.0 (macOS 26+, Apple Silicon;
the versions represented by repository evidence), Docker 29.x (29.7.2
local run and a v29 inspect fixture; Linux, Windows, macOS), root module
Go 1.23+; nested `bench/` module Go 1.25+

This document describes the current implementation in this checkout.
The **Implementation phases** section is retained as a historical plan;
it is not a promise that every phase is still a current API or roadmap.
The public API and behavior described in the current sections come from
the implementation and its tests.

This checkout is not a merge of the follow-up issue branches that harden
backend-specific behavior. Apple log options depend on #82, dynamic
endpoint refreshes depend on #85, reuse ownership and final generation
verification depend on #83 and #84, stale Docker operation targeting
depends on #74, and Docker inspect target validation depends on #103.
Docker dead-state pruning depends on #113; missing-inspect
classification depends on #103; error-chain preservation depends on
#104; and Apple PullNever capability handling depends on #112. Windows and remote bind-source handling depend
on #76, TCP-only readiness validation on #77, Stop timeout validation on
#89, wait error-chain normalization on #92, public option validation on
#102, and reaper staging cleanup on #111. The current sections describe
the behavior before those changes; they do not promise the follow-up
contracts.

The latest tagged release is `v0.2.0` (2026-09-02). This checkout is
development after that tag. The `v0.2.0` module requires Go 1.27 or
later; the root checkout requires Go 1.23 or later, while the nested
`bench/` module requires Go 1.25 or later. The released API and the
current development API are not interchangeable: `LogsOptions` /
`LogsWithOptions`, the additional `wait.ForHTTP` setters, exported
`wait.AllStrategy` / `AnyStrategy` with composite `WithStartupTimeout`,
and the root `CLIError`, `ErrContainerNotFound`, and
`ErrGenerationReplaced` symbols are development additions after
`v0.2.0`. The core `Run`, options, lifecycle, endpoints, `Exec`, `Logs`,
`FollowLogs`, copy, pull policies, reuse, cleanup, backend selection, and
original wait strategies were present in `v0.2.0`.
Targets: Apple Container v1.2.x (macOS 26+, Apple Silicon), Docker (Linux, Windows, macOS; copy-out requires client/server 29.7.0+), Go 1.23+

## Purpose

**container-go** is a testcontainers-style Go library backed by Apple
Container ([apple/container](https://github.com/apple/container)).
It starts throwaway containers from Go tests, hands out connection
endpoints, and provides normal and best-effort orphan cleanup for the
containers it creates.

[shiguredo/container-rs](https://github.com/shiguredo/container-rs) is
prior art for Rust. This library covers the same problem space in Go,
but with a different implementation strategy, described below.

Three constraints shape the design.

- **Zero dependencies**: no third-party Go modules; the standard
  library only.
- **Security**: subprocess argv prevents injection; reaper staging still has
  an environment-data exposure on abnormal termination (#111), so the
  no-leak property is not a current guarantee.
- **Performance**: container (VM) startup dominates test suite time;
  the library's own overhead must stay negligible against that, and it
  must never serialize parallel startups.

## Apple Container facts the design relies on

The design decisions below rest on properties of Apple Container's CLI and
source. The repository's inspect fixtures cover 1.2.2 and 1.3.0; this is
not an exhaustive compatibility claim for every 1.2.x or 1.3.x release.

- Host requirement: macOS 26 or later on Apple Silicon.
- By default, each container boots as a lightweight VM with a real IP on
  a vmnet bridge (network `default`, `192.168.64.0/24`). The host can
  reach that IP directly, so port publishing (`--publish`) is optional.
- Everything is operable through the `container` CLI. `ls --format json`
  and `inspect` emit machine-readable JSON (additive fields are ignored
  by `internal/inspect`).
- The CLI talks XPC to `container-apiserver` under launchd. Commands
  fail while the service is down; `container system status` reports
  its state.
- The container name is the container ID. The shared library guard is
  `^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$` (one to 63 characters). Apple
  Container's CLI has a stricter 2–63-character rule,
  `^[a-zA-Z0-9][a-zA-Z0-9_.-]{1,62}$`; this checkout does not apply that
  backend-specific preflight check (#112).
- Several Docker features do not exist: healthchecks, a `wait`
  command, an event stream, label filters on `ls`, and re-attaching to
  a running container. Their behavior must be reproduced client-side.
- `--label` exists, but filtering by label means filtering the JSON
  output client-side. Label keys are restricted to lowercase
  Docker/OCI-style keys.
- `container cp` only works on running containers. Its copy-out operation
  has no type-preserving/no-follow mode: observed versions can dereference
  or consume symlinks, FIFOs, and device nodes before the host can inspect
  the result. `CopyFileFromContainer` therefore fails closed on Apple
  Container with `ErrCopyFileFromContainerUnsupported`; Docker requires
  client and server versions >=29.7.0 for the type-preserving extractor,
  then retains the host-side Lstat/open checks where the host supports the
  required flags, and otherwise fails closed.
- `--rm` removal leaves anonymous volumes behind.
- Error classification depends on CLI stderr substrings owned by
  `engine_apple.go` (name conflict, image/container missing). Those
  matchers are regression-tested against a live CLI in
  `cli_compat_integration_test.go`.

## Choosing the implementation strategy

Two candidate strategies exist.

- **CLI wrapper**: spawn the `container` CLI via `os/exec` and decode
  its JSON output.
- **Direct XPC**: what container-rs does — call the
  `container-apiserver` XPC services through a C bridge.

This library adopts the CLI wrapper, for three reasons.

First, the zero-dependency constraint. Direct XPC needs cgo and a
hand-written C bridge, dragging the macOS SDK into the build. The CLI
wrapper is pure Go on the standard library and builds with
`CGO_ENABLED=0`.

Second, stability. XPC route names and message shapes are Apple
Container's internal implementation with no compatibility promise. The
CLI is the user-facing public interface, and its JSON schema can be
verified against the public Swift sources.

Third, the performance difference does not matter. Spawning a child
process costs tens of milliseconds per call against seconds of
container (VM) startup; for test workloads the XPC saving is
imperceptible.

The CLI wrapper's weaknesses are output-format drift across CLI
versions and features the CLI does not expose (label filters, for
example). The implementation uses JSON for inspect and Apple list data, and
parses the documented machine-oriented Docker name and run-ID output
where the backend provides it. It never parses human-readable tables.
Features that the CLI does not expose are worked around with
client-side filtering.

## Public API

The API shape follows testcontainers-go (v0.44 line) so that existing
testcontainers users need no relearning.

Module path `github.com/hirokazumiyaji/container-go`, root package
`container`.

### Basic usage

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
        container.WithEnv(map[string]string{"REDIS_ARGS": "--appendonly yes"}),
        container.WithWaitStrategy(wait.ForListeningPort("6379/tcp")),
    )
    container.Cleanup(t, ctr) // nil-safe; registers Terminate via t.Cleanup
    if err != nil {
        t.Fatal(err)
    }

    endpoint, err := ctr.Endpoint(ctx, "6379/tcp") // e.g. "192.168.64.3:6379"
    if err != nil {
        t.Fatal(err)
    }
    _ = endpoint
}
```

### Run and options

The following is a signature inventory, not an executable source file:

```text
func Run(ctx context.Context, image string, opts ...Option) (*Container, error)
```

When `Run` needs to create a container, it applies the pull policy
before starting it and then completes the wait strategy. The default
`PullMissing` policy inspects the local image store and issues an
explicit pull only when the image is absent. `PullAlways` issues an
explicit pull for every new-container attempt. `PullNever` is
backend-specific: on the current checkout it performs a best-effort
precheck and returns `ErrImageNotFound` when that precheck finds no
image. Docker also passes `--pull=never`, so Docker has the strict
no-fetch path; Apple Container has no equivalent run-time switch, so
its precheck is not a no-fetch guarantee (#112). If a post-start
operation or wait fails on the non-reuse path, `Run` rolls back the
container it created before returning the error. Reuse has the separate
shared-lifetime contract described below.

Options use the functional options pattern. The following inventory
describes the current development checkout. The core options listed here
were already available in `v0.2.0`; the additional `LogsOptions` /
`LogsWithOptions` API and the additional HTTP and composite-wait setters
described below are not.

- `WithExposedPorts(ports ...string)`: declare the container ports
  (`"6379/tcp"` form) that `MappedPort` and `Endpoint` may resolve.
  Docker automatically publishes these ports; Apple Container resolves
  them through the container IP by default. On Apple, this is a
  handle-side endpoint declaration rather than a separate backend
  `--expose` flag.
- `WithEnv(env map[string]string)`: environment variables, passed
  through a temporary file rather than command-line values.
- `WithCmd(cmd ...string)` / `WithEntrypoint(entrypoint string)`:
  command and entrypoint overrides. Entrypoint is a single token per
  `docker run --entrypoint` semantics; pass multi-token commands via
  `WithCmd`.
- `WithWaitStrategy(s wait.Strategy)`: readiness detection.
- `WithName(name string)`: container name (default
  `containergo-<random hex>`). It uses the shared 1–63-character guard;
  Apple's stricter 2–63-character rule is not enforced by the current
  checkout's backend preflight (#112).
- `WithLabels(labels map[string]string)`: extra labels. The library's
  managed labels are reserved.
- `WithMounts(mounts ...Mount)`: bind, named-volume, and tmpfs mounts.
  Current bind-source validation is Unix-style; Windows host paths and
  remote Docker bind-source semantics are pending #76.
- `WithFiles(files ...File)`: files copied into the running container
  after start; a copy failure rolls back `Run`. On `WithReuse`, files
  are copied for every caller, including attach callers; an attach copy
  failure returns an error without deleting the shared container.
- `WithPublishedPort(spec string)`: explicit host-side port publishing.
  Apple normally uses the container's direct IP, while Docker
  auto-publishes ports declared with `WithExposedPorts` to
  daemon-assigned host ports. Use an explicit binding when a caller
  needs a specific host port.
- `WithPullPolicy(policy PullPolicy)`: choose `PullMissing` (default),
  `PullAlways`, or `PullNever`. `PullAlways` fetches before attach on
  `WithReuse` as well as on create.
- `WithReuse()`: make a named `Run` a get-or-create operation. Attach
  callers apply `WithFiles` and `PullAlways`. Other creation-only
  options are ignored on attach.
- `WithReuseGroup(group string)`: label a reused container for
  `PruneReuseGroup`; it requires `WithReuse` and is not part of the
  reuse key. `PruneReuseGroup` currently uses a weaker validation
  grammar (#102).
- `WithCPUs(n int)` / `WithMemory(size string)`: resource limits.
  `WithMemory` currently accepts zero and does not enforce backend
  capability limits (#102).
- `WithUser(u string)` / `WithWorkingDir(dir string)`: process user
  and working directory.
- `WithNetwork(name string)`: attach to an existing named network.
- `WithPlatform(p string)`: select an image platform such as
  `linux/amd64` (including Rosetta use on Apple Container).
  and working directory
- `WithNetwork(name string)`: target network; when omitted, Docker keeps
  its daemon-selected default (`bridge` on Linux, `nat` on native Windows)
- `WithPlatform(p string)`: e.g. `linux/amd64` (via Rosetta)

Options outside the backend-neutral CLI surface are intentionally
omitted. There is no public logger-injection option. For log consumers,
`FollowLogs` returns a stream and `Logs`/`LogsWithOptions` return
snapshots.

### Public input validation

Public calls reject invalid inputs before starting a backend operation. An
input that is known to be invalid returns an error matching
`ErrInvalidOption`; callers that need details can declare a
`*container.ValidationError` and pass its address to `errors.As`.
`ValidationError.Option` identifies the public option or operation, and
`Field` identifies the exact
field such as `key`, `value`, `hostPath`, or `containerPath`. `Value` is
populated only for values that are safe to expose. Environment and label
values are never copied into the typed error or rendered diagnostic when
they are rejected.

`ValidationError.Err` is the source of truth for the rendered message and
unwrap chain. The exported `Message` field remains for source compatibility
but does not override `Err`. `Run` validates its image before invoking any
`Option`; `WithFiles` resolves and stats each host path before image or
container work; Docker applies its complete local-volume-name grammar in
`checkConfig` while Apple keeps its backend-specific name rules. Copy paths
are interpreted as POSIX paths with the `path` package, and a negative
`Container.Stop` timeout is rejected before the CLI is called.

### The Container handle

The following signature inventory includes current-development methods. It is
not an executable source file:

```text
type Container struct {
    // unexported fields omitted
}

func (c *Container) ID() string
func (c *Container) Host(ctx context.Context) (string, error)
func (c *Container) MappedPort(ctx context.Context, port string) (int, error)
func (c *Container) Endpoint(ctx context.Context, port string) (string, error)
func (c *Container) ContainerIP(ctx context.Context) (string, error)
func (c *Container) State(ctx context.Context) (State, error)
func (c *Container) Exec(ctx context.Context, cmd []string, opts ...ExecOption) (int, io.Reader, error)
func (c *Container) Logs(ctx context.Context) (io.ReadCloser, error)
func (c *Container) LogsWithOptions(ctx context.Context, opts LogsOptions) (io.ReadCloser, error)
func (c *Container) FollowLogs(ctx context.Context) (io.ReadCloser, error)
func (c *Container) CopyToContainer(ctx context.Context, hostPath, containerPath string) error
func (c *Container) CopyFileFromContainer(ctx context.Context, containerPath string) (io.ReadCloser, error)
func (c *Container) Stop(ctx context.Context, timeout *time.Duration) error
func (c *Container) Terminate(ctx context.Context) error
```

`Stop` accepts a duration, but the current backends convert non-nil values
to whole seconds by truncating fractional values. Negative and extreme
values are not rejected, and the duration is added to the query budget
without saturation. No library-side maximum is enforced; backend limits
apply, and #89 tracks the missing validation.

`Exec` returns the exit code and combined stdout+stderr. A command
that runs in a running container and exits non-zero is a result rather
than an infrastructure error; a stopped or unreachable container can
still return an error. `WithExecEnv`, `WithExecUser`, and
`WithExecWorkDir` configure an individual exec invocation; environment
values use the same temporary-file mechanism as `WithEnv`. `Logs` and
a supported `LogsWithOptions` call return a finite snapshot because the
backend command finishes, but neither imposes a byte limit by itself.
`Logs` requests all output. `LogsOptions` is backend-specific: Docker
supports `Tail` and `Since`, while the checked Apple Container CLI
versions use `-n` for a tail and have no `--since` option. This checkout
still sends Docker-style `--tail` and `--since` arguments to Apple, so
`LogsWithOptions` is not backend-neutral here; #82 must be applied before
Apple support can be documented as a capability. The #82 change is intended
to map `Tail` to Apple's `-n` and reject `Since` as unsupported. A requested
window's size still depends on the container output. `FollowLogs` is intentionally
an unbounded stream and continues until its reader is closed or its
context is cancelled. `LogsOptions` and `LogsWithOptions` are
current-development APIs, not part of `v0.2.0`. `Terminate` is
generation-guarded for handles with a generation, but the current reuse
and empty-generation paths have limitations described under Reuse and
tracked by #83 and #84.

Validation is not uniform in this checkout (#102): a negative
`LogsOptions.Tail` is treated as zero/all, `WithMemory("0")` is accepted
by the current parser, unknown `MountType` values are not rejected by
`WithMounts`, and `PruneReuseGroup` uses a weaker grammar than
`WithReuseGroup`. These are current behaviors, not backend acceptance
guarantees.

`Terminate` maps to `container delete --force` on Apple Container and
`docker rm --force` on Docker. It treats a recognized backend not-found
CLI failure as success, so its idempotence claim is limited to that case
(and to a successful delete). It does not normalize empty or malformed
inspect output, and the current Docker parser does not reject a valid
object whose ID or name differs from the requested target. Neither case
is covered by the idempotence claim; unrelated delete failures still
propagate. `Cleanup(t, ctr)` and `TerminateContainer(ctr)` are nil-safe
helpers preserving the testcontainers-go idiom of registering cleanup
before checking `Run`'s error.

`CopyFileFromContainer` is a Docker-only copy-out operation that requires
safe host file-open semantics. The method verifies the materialized result
is a regular file, but does not claim that every Docker host can represent
every container file type. Apple Container's CLI cannot preserve or reject
all source file types before the host opens the result, so the method
returns `ErrCopyFileFromContainerUnsupported` without invoking `container
cp`. Docker copy-out requires both the client and server to be at least
29.7.0; the method verifies those versions before creating its private
temporary directory or invoking `docker cp`, and fails closed with the same
error if verification fails. Hosts without no-follow/nonblocking file-open
support fail closed with the same error. On Windows, Go 1.23 through 1.25 do
not propagate the required Windows file flags through `os.OpenFile`, so
Docker copy-out requires Go 1.26 or newer. `CopyToContainer` remains
available on both backends.

## Connection endpoints

testcontainers' Docker implementation publishes container ports to
random host ports and connects to `localhost:<mapped>`. On Apple
Container this is not the default.

For Apple Container, `Host` returns the container's real IP (from
inspect's first `status.networks[].ipv4Address`, CIDR suffix stripped),
and `MappedPort` returns the container port unchanged. Docker uses a
published host endpoint by default instead: local daemons publish on
loopback, while detected remote daemons use a remote-safe binding.
Three reasons for the Apple direct-IP default:

- Apple Container has no random port assignment; grabbing a free host
  port up front races between "find free port" and "start container"
  (container-rs documents the same race as a known limitation). Direct
  IP connection consumes no host ports, so the race does not exist.
- With no host-port collisions, parallel test runs scale without
  limit.
- No port-forwarding proxy is involved, avoiding its failure modes
  (silently truncated large transfers have been reported).

For a local Docker daemon, when a client demands a `localhost` endpoint
(or the container IP is unreachable in a given setup), publish explicitly
with `WithPublishedPort("127.0.0.1:15432:5432")`. This loopback example
is local-only; a non-loopback `DOCKER_HOST` rejects loopback bindings.
For a detected non-loopback `DOCKER_HOST` daemon, use a remote-safe
binding and endpoint instead.

`MappedPort` and `Endpoint` return `ErrPortNotExposed` for ports that
are neither declared via `WithExposedPorts` nor explicitly published, or
for a declared port with no usable host binding. The declarations also
feed wait strategies: `ForExposedPort` uses the first declared port, and
an empty `Target.Endpoint` port selects that same first declaration.

The current implementation caches the first successful inspect used by
endpoint-related methods. That record includes the container IP and
host-side bindings, although either can change during the container's
lifetime. `State` uses a fresh inspect, but an endpoint result can
therefore be stale. This is a snapshot behavior, not an immutability
guarantee; #85 tracks refreshing dynamic endpoint data. For Docker with
multiple networks and no top-level address, current network selection is
unspecified rather than a deterministic first-network contract.
Docker's `host` and `none` network modes cannot create
library-managed port bindings.
Externally isolated networks (`Internal: true` or an isolated bridge
gateway mode) are rejected for the same endpoint contract.
For an explicitly selected non-default network, `Run` inspects it before
any image or container command and returns `*ConfigError` when either
`WithExposedPorts` or `WithPublishedPort` is combined with an incompatible
network. `host` and `none` publish combinations are rejected before
startup.

Host mode without port declarations remains available.
`Host` returns the client-facing daemon host, while `MappedPort` and
`Endpoint` refuse to infer a service port from the host namespace.
None mode has no reachable host, so `Host` returns an error matching
`ErrNoReachableHost`. Endpoint resolution verifies both the requested and
actual network mode and the inspected binding instead of trusting the
publish string; mismatches match `ErrNetworkMismatch`.

When `WithNetwork` is omitted, Docker receives no synthesized
`--network bridge` flag. The daemon chooses its platform default
(`bridge` on Linux, `nat` on native Windows). Inspect's special
`HostConfig.NetworkMode == "default"` is matched against the concrete
names in `NetworkSettings.Networks`, so a pre-existing default container
can be reused without treating omission as a wildcard for `host`, `none`,
or arbitrary named networks. The daemon server OS is also queried for the
authoritative platform default; a user-defined `bridge` on Windows or
`nat` on Linux is rejected, and an unavailable identity fails closed with
`ErrNetworkMismatch`.

Docker handles retain the immutable ID printed by `docker run --detach`.
Inspect for Host, Endpoint, lifecycle operations, and reuse targets that
ID and validates the returned identity. Network, IP, and port-binding
fields are dynamic and are refreshed for every operation; only immutable
identity (UID, image, and labels) is cached.

IP addresses are canonicalized with `netip`, so expanded IPv6 loopback
and `::` compare correctly with Docker inspect output.
An unspecified IPv6 bind resolves to `::1`, preserving its address
family.
On a remote daemon, an inspected loopback binding returns
`ErrEndpointUnreachable`; rewriting it to the remote host would not
reach the daemon's loopback listener.

`MappedPort` and `Endpoint` error with `ErrPortNotExposed` for ports
not declared via `WithExposedPorts` or `WithPublishedPort`, and for
declared ports without a usable host binding.
The declarations also feed wait strategies (the default port of
ForListeningPort, for example).

## Wait strategies

Apple Container has neither healthchecks nor a wait command, so
readiness is decided entirely client-side. The `wait` subpackage
provides:

- `wait.ForLog(pattern)`: read the `FollowLogs` stream until a
  substring (or a regular expression via `AsRegexp`) appears. Matching
  is per line; `WithOccurrence(n)` changes the required count. This
  strategy is stream-based rather than poll-based.
- `wait.ForListeningPort(port)`: poll a TCP connection to the resolved
  endpoint until it succeeds.
- `wait.ForExposedPort()`: use the first port declared by
  `WithExposedPorts`.
- `wait.ForHTTP(path)`: send an HTTP request to the resolved endpoint
  until its status matches (2xx by default; change it with
  `WithStatusCodeMatcher`). `WithPort` and `WithMethod` select the
  target. The current development checkout also adds `WithHeaders`,
  `WithHeader`, `WithBasicAuth`, `WithTLS`, `WithTLSConfig`, and
  `WithHTTPClient`; these setters are not in `v0.2.0`.
- `wait.ForExec(cmd)`: run a command through the backend CLI until it
  exits with an accepted code (0 by default).
- `wait.ForAll(strategies...)` / `wait.ForAny(strategies...)`:
  composition. Child strategies keep their own settings. In the current
  development checkout, the exported `AllStrategy` and `AnyStrategy`
  types expose `WithStartupTimeout` to bound the whole composition;
  they do not expose `WithPollInterval`. The `v0.2.0` functions return
  an interface without a composite timeout setter.
- `wait.ForLog(s string)`: wait until a substring (or regexp via
  `AsRegexp`) appears in `container logs --follow` output;
  `WithOccurrence(n)` for repeat counts
- `wait.ForListeningPort(port string)`: wait until a TCP connection to
  the container endpoint succeeds. The specification may be `PORT` or
  `PORT/tcp`; UDP and malformed specifications return a typed
  `*wait.ConfigError` before any target probe. The error matches
  `wait.ErrInvalidConfiguration`.
- `wait.ForHTTP(path string)`: wait until an HTTP request via
  `net/http` matches the status predicate (2xx by default,
  `WithStatusCodeMatcher` to change). `WithPort` / `WithMethod` select
  the target; `WithHeaders` / `WithBasicAuth` / `WithTLS` /
  `WithTLSConfig` / `WithHTTPClient` cover auth, TLS, and custom
  transports without breaking the default plain-HTTP probe.
- `wait.ForExec(cmd []string)`: wait until `container exec` exits with
  an accepted code (0 by default)
- `wait.ForAll(ss ...Strategy)` / `wait.ForAny(ss ...Strategy)`:
  composition. Each child keeps its own `WithStartupTimeout`; a positive
  composition `WithStartupTimeout` bounds the whole sequence/concurrent
  wait. With no positive composition timeout, zero or a negative value
  leaves the composition unbounded and the child timeouts apply.

Every leaf strategy carries `WithStartupTimeout` (zero means 60s) and
`WithPollInterval` (zero means 100ms, except `ForExec`, which defaults to
250ms). For `ForLog`, the poll interval is the delay before reopening a
stream that ends before the pattern is found. A stopping, stopped, or
paused container fails the wait without burning the remaining timeout.
Created, restarting, unknown, and transient inspect states remain
retryable; transient log-stream open and EOF failures reopen the stream,
while a terminal log-stream error is returned. A marker match is accepted
only after a bounded final lifecycle observation reports `Running`; `ForLog`
therefore targets long-lived services rather than one-shot job completion.
`ForLog` counts occurrences across reconnects after de-duplicating the
replayed log prefix. A failed
non-reuse `Run` wait rolls the container back and, when the bounded log
fetch succeeds, attaches a log tail capped at 1MiB to the error; a reuse
wait leaves the shared container in place.

`ForListeningPort` and `ForExposedPort` are TCP-only probes. UDP can be
declared for endpoint configuration, but the current implementation does
not reject `/udp` before probing: it is passed to a TCP dial and may
time out or reach an unrelated TCP listener. Malformed specifications are
retried until the wait ends. See #77.

| API | TCP | UDP |
|---|---|---|
| `WithExposedPorts` / `WithPublishedPort` | Accepted by the current option parser | Accepted for endpoint/publish configuration |
| `wait.ForListeningPort` | `PORT` or `PORT/tcp` | Not a UDP probe; a UDP declaration may be TCP-dialed |
| `wait.ForExposedPort` | Uses the first declared port, regardless of protocol | If that first declaration is UDP, it is still passed to a TCP dial |

The strategy contract is:

```text
type Strategy interface {
    WaitUntilReady(ctx context.Context, target Target) error
}
```

`Target` is a small interface exposing `Endpoint`, `Running`,
`FollowLogs`, and `ExecCommand`. `*container.Container` is adapted to
it. `Target` does not expose `ContainerIP`; connection strategies use the
resolved endpoint, which is the right abstraction for both direct-IP
Apple Container and published-port Docker. `Target` keeps its original `Running`
method for custom-strategy compatibility. Targets that distinguish startup
transitions also implement the optional `StateTarget` interface; built-in
strategies prefer `State` and fall back to `Running`. `container.Run` adapts
`*container.Container` to both surfaces. The dependency points from `container`
to `wait`, never back, avoiding an import cycle.

## Cleanup

The library has separate normal, rollback, failed-create, and
best-effort abnormal-exit paths. Their error visibility is intentionally
different.

**Normal path**: `Cleanup(t, ctr)` registers `Terminate` via
`t.Cleanup`. `TerminateContainer(ctr)` is the nil-safe deferred-style
helper. A termination error from `Cleanup` is sent to the test log;
`TerminateContainer` returns its error to the caller.

**Post-create rollback**: when a non-reuse `Run` fails while copying
files or waiting for readiness, it calls `Terminate` for the handle it
created. If that deletion fails, the returned error includes the
original failure and a message saying that the container was left
behind. A wait error also includes a log tail when the bounded log fetch
succeeds.

**Failed create**: if the backend `run` command itself fails,
`cleanupFailedCreate` makes a separate best-effort attempt. It skips
name conflicts, and only inspects and deletes a container carrying this
process's managed and session labels. When the creation label is present,
it must match this run; an absent label is not a mismatch in this
best-effort path. This is not a general replacement guarantee; #83 tracks
closing the missing-generation path. Errors from its lock, inspect, or delete
operations are not joined to the returned `Run` error.

**Abnormal exit**: when the parent process exits and its reaper pipe
closes, an external `/bin/sh` watchdog attempts force-deletion. An
uncaught process-terminating panic, `SIGKILL`, and `os.Exit` close the
pipe; a recovered panic does not. The reaper does nothing while the
parent is alive. It is insurance, not a transactional guarantee, and it
is unavailable on Windows. Registration and per-entry delete errors are
not returned to `Run`; repeated spawn failures are logged once after the
retry limit, and delete failures are ignored by the shell. Each backend
call is bounded by a POSIX `sleep`/`kill` timeout.

The reaper is started lazily for a real CLI container on the non-reuse
path. Apple Container has no separate immutable ID, so its entries use
the container name and creation generation. The reaper checks the
creation label before deleting by name, but it does not take the
per-name lock. Its inspect/delete window can therefore race a
same-name replacement; an external CLI delete/recreate cannot be
distinguished by name either. This reaper coordination gap is a separate
cleanup limitation.

The Docker branch is prepared to use the immutable 64-hex ID returned by
`docker run`, but this checkout does not yet accept that ID in
`reaper.register`; it accepts only Apple-style names. The normal Docker
`run --detach` path therefore cannot register that ID with the reaper on
this base. If a backend returns no parseable ID, `Run` falls back to the
name-and-generation path. Issue #73 must be stacked for the Docker-ID
registration path to work. After #73, Docker entries use the immutable ID
and `docker rm --force`; normal Docker `Terminate` and rollback handles can
use the ID independently.

**Reaper staging exposure**: the current reaper stages full `inspect`
output in an un-namespaced `mktemp` file and removes it on its ordinary
completion or inspect-error paths. If the reaper is killed, a file
containing environment data can remain. Before cleanup, stop container-go
and reaper processes, then use a metadata-only listing of regular files
owned by the user in the effective `TMPDIR`, restricted to the affected
time window. Do not print or grep file contents, follow symlinks, or run a
broad recursive delete. Remove only files positively tied to the affected
run, and rotate credentials that may have appeared in inspect output
(#111). This is not a no-leak guarantee.

**Session labels**: every created container carries

- `com.github.hirokazumiyaji.container-go`: `true` (managed-by marker)
- `com.github.hirokazumiyaji.container-go.session`: a per-process
  random ID
- a creation generation label, and, for reuse, the reuse/group labels

The Apple CLI has no label filter, so orphan sweeps filter
`container ls -a --format json` client-side. `Prune(ctx)` removes the
containers selected by the active backend's managed filter. Apple selects
the stopped state. The current Docker filter selects the exited state
only, so Docker dead-state containers remain until #113 is applied;
running and created containers are not selected. On Apple, each
candidate is re-inspected and re-verified (generation, session,
managed/reuse/group labels, stopped state) under the stable per-name lock
before deletion.

Setting `CONTAINERGO_KEEP=1` is a process-wide diagnostic switch. It disables
automatic deletion by `Cleanup` and `TerminateContainer`, skips reaper
registration, and suppresses automatic rollback on failed creates, `WithFiles`
copies, and wait strategy failures, leaving containers in place for
inspection. It does not suppress explicit `Container.Terminate`, and `Prune`
or `PruneReuseGroup` can still delete containers. `WithReuse` also keeps its
stopped-container replacement behavior, so a matching stopped reuse container
can still be deleted and recreated. Treat the variable as a debugging aid,
not a global deletion lock.

Anonymous volumes survive `--rm`, so the library never creates one;
volumes must be named, and their lifecycle belongs to the caller.

## Reuse

`WithReuse` turns `Run` into a get-or-create for a stable `WithName`
(shared across processes on the same host). An existing container
must carry the current `WithReuse` marker, and every caller reruns its
readiness strategy. The current check does not require every ownership
or generation label; #83 tracks the stricter boundary. Image compatibility
is checked on both backends.
Port compatibility is backend-specific. Docker compares auto-published
bindings for `WithExposedPorts` and checks explicit `WithPublishedPort`
bindings. Apple checks explicit published bindings, but the library's
Apple inspect model does not retain `WithExposedPorts` declarations, so
those declarations cannot be compared. Each Apple caller uses its own
exposed-port declaration against the shared container IP; use distinct
names when those declarations must be isolated. `WithFiles` is
copied for every caller, and `PullAlways` fetches the image before
attach; a failed attach copy returns an error without deleting the
shared container. Other creation-only differences such as `env`, `cmd`,
and `mounts` attach silently to the existing container by design;
callers needing isolation should use distinct names or reset state via
`Exec`.
(shared across processes).
The compatibility check compares the image reference, declared and
published ports, and the Docker network identity.
An omitted Docker `WithNetwork` means the daemon-selected default. Docker's
special `default` inspect mode is resolved against the actual network names
(`bridge` on Linux, `nat` on native Windows), rather than being assumed to
be `bridge`; omission is not a wildcard for `host`, `none`, or a named
network.
Reuse re-inspects by immutable Docker UID before compatibility checks and
again before returning the handle.
A loopback binding inspected on a remote daemon fails with
`ErrEndpointUnreachable`.
`env`, `cmd`, and `mounts` differences attach silently to the existing
container by design; callers needing isolation should use distinct
names or reset state via `Exec`.

Each creation by this checkout normally carries a `creationLabel`
(16-hex). The current reuse path is narrower than that label suggests:
`checkReuseOwned` requires the reuse marker and a compatible image, but
does not require the managed or creation label. A stopped container with
an empty inspected generation can therefore reach a name-based delete
path. `Terminate` performs a fresh generation check only when its handle
has a non-empty generation; an empty-generation handle takes the legacy
name-delete path. `reuseRun` also does not re-inspect after its readiness
strategy returns, so a replacement can occur during the wait. Issues #83
and #84 track the ownership checks and final generation verification.
`CONTAINERGO_KEEP=1` does not change these reuse rules: a matching stopped
reuse container can still be deleted and recreated, and
`PruneReuseGroup` can still remove the group.

For a handle with a valid non-empty generation, the Apple path checks a
fresh inspect and runs inspect plus delete under a per-name `flock` in the
temp directory (`containergo-<name>.lock`). That protects the ordinary
generation-checked `Terminate` and failed-create cleanup paths from
cooperating library processes on the same host. That same per-name lock
covers Apple `Prune` and `PruneReuseGroup` candidate deletion. The external
reaper does not take this lock on the current checkout, which is a separate
cleanup limitation. An external
`container delete` plus re-create in the same window also remains outside
the guarantee. A Docker handle that retains the immutable `Id` printed by
`docker run` deletes by that ID, so a same-name replacement does not share
the deletion target. An `Id` recovered through inspect depends on target
validation, which the current Docker parser does not perform (#103).
Other Docker operations on the current base still address the logical
name; #74 tracks using the immutable ID for those operations as well.
These are different protections; the current base does not make reuse a
general fail-closed guarantee. The watchdog reaper registers Apple
containers by name and generation, reads the label as a line-anchored
JSON field (`"key": "value"`, never a substring), and skips deletion on
mismatch. The current base does not register Docker's immutable ID with
the reaper; issue #73 is required before Docker reaper entries can use
`Id`. Each backend call carries a 10-30s timeout via POSIX `sleep`/`kill`
(no `timeout(1)` dependency) so one hung daemon call cannot wedge the
rest. The leader's own pull/create uses an independent `runTimeout`
budget; `reuseAttachTimeout` bounds only attach polling for another
process's container.

The pull flight collapses *concurrent* callers only: an entry is removed
when it completes, so a `Run` that happens after the previous one has
finished inspects the image again. That is the safe default, since an
image can be removed from the store between two `Run`s. `Run` therefore
pays one daemon round trip for the existence check every time, which
`WithImagePresenceCache(ttl)` can remove for callers whose store is
stable for the length of the run. The returned Option owns the cache, so
callers must reuse that Option across `Run`s; a fresh call creates an
empty cache. The cache holds only "present" answers - a cached absence
would have to be invalidated by the pull it triggered - and is keyed by
backend, image store (Docker client-config env for Docker), image, and
platform, since those stores are independent.
`PullNever` always inspects: its contract is to fail when the image is
absent, so a cached answer must not stand in for the check.

## Security design

As a library that spawns subprocesses, these rules describe the intended
security boundary. The reaper staging exception in #111 means that
"no information leak" is not a current end-to-end guarantee.

**No shell involvement**. Every CLI call passes an argv array to
`exec.Command`; no shell string is ever assembled. The single
exception is the watchdog reaper's shell script. Its body is a fixed
string; container IDs enter only as stdin data. The script defeats
word splitting and globbing (`set -f`, `IFS=`, `read -r`, quoted
expansions). On this base, name-addressed reaper entries use the shared
library guard `^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$` (one to 63
characters) before they are written to the pipe. This is not the
complete Apple Container contract: Apple's CLI requires 2–63 characters
using `^[a-zA-Z0-9][a-zA-Z0-9_.-]{1,62}$`, and the current checkout does
not apply that backend-specific preflight check (#112). After issue #73,
the reaper will separately accept a full lowercase 64-hex Docker ID.
Reaper registration failures are ignored. The two layers together leave
no command injection through registered IDs.
expansions), and the library validates each target before writing it to
the pipe. Apple Container names must match
`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`; full Docker IDs must be lowercase
64-character hexadecimal strings matching `^[0-9a-f]{64}$`. The two
layers together leave no command injection through IDs.

**No environment variables on argv**. `--env key=value` exposes values
to every user via `ps`. Because environment variables are the main
channel for secrets, Unix writes them to a 0600 file in a 0700,
current-user-owned directory under the canonical `os.UserCacheDir()` path
and passes `--env-file`. Existing symlinked ancestors are resolved once;
`..`, symlinked storage roots, and writable untrusted ancestors are
rejected, and `TMPDIR` is not trusted. A version marker, owner/mode
checks, an expected-child allowlist, and a writer lock held for the
whole backend call bound crash cleanup. A staging directory becomes
eligible for cleanup after 24 hours. Marker-before-lock and tombstoned
partial-removal states are self-healing, while an unmarked, replaced, or
unexpected entry fails closed and requires manual inspection. Cleanup
errors are returned and retried. Windows has no equivalent secrecy
guarantee through Go `chmod`, so operations requiring an env file return
`ErrEnvFileUnsupported` before invoking the backend.

**Reaper staging**. The current reaper writes the full `inspect` output
to an un-namespaced `mktemp` file before removing it. A killed reaper can
leave environment data on disk; see the #111 mitigation above.
**Validate inputs**. Container names (name rule above), label keys
(the CLI's Docker/OCI form), ports (numeric range and `tcp`/`udp`),
and copy paths (absolute POSIX paths with `/` separators, no backslashes,
valid UTF-8) are validated before reaching the CLI. Env keys are valid
non-empty UTF-8 without `=`, Unicode whitespace/controls, a leading `#`, or
a leading BOM. Env values are valid UTF-8 without Unicode controls, NUL,
CR/LF, U+2028, or U+2029; other non-control Unicode, spaces, and `=` remain
valid. Rejecting controls (including tab) and invalid UTF-8 is intentional
even if a particular backend accepts such a value, because the library will
not place it in a line-delimited env file. The CLI validates too, but
first-party validation gives clearer errors and independence from future CLI
changes.

**Validate inputs**. The shared `WithName` guard and name-addressed
reaper check use the rule above; label keys (the CLI's Docker/OCI form),
ports (numeric range and `tcp`/`udp`), environment keys (no `=`, no NUL),
and container-side copy paths (absolute, valid UTF-8) are also validated
before reaching the CLI. This library-side guard is not a claim of full
backend name validation: Apple's stricter minimum remains unenforced in
this checkout (#112). Host-side copy paths are resolved to absolute
paths. The CLI validates too, but validating first gives clearer errors
and independence from future CLI changes. Public option validation
remains partial: negative `LogsOptions.Tail`, zero memory, unknown mount
types, and reuse-group grammar are not uniformly rejected before backend
work (#102).

**Handle no credentials**. Registry authentication is delegated to
the backend CLI: use `container registry login` for Apple Container or
`docker login` for Docker. The library has no credential input path.

**No logger injection API**. The public API does not expose a logger
hook. `CLIError` exposes the failed command arguments and bounded stderr
for diagnostics, while environment values remain in the temporary env
file rather than appearing in argv. The watchdog can emit one
standard-log message if it cannot be started after repeated attempts.

## Performance design

**Minimize subprocess count**. Create+start is one backend
`run --detach` call. The first successful inspect is cached and reused by
endpoint-related paths for performance. The current cache also retains the
container IP and host-side bindings, even though those values are dynamic;
`State` and some lifecycle operations re-inspect. This is a stale-data
limitation, not an immutability guarantee; #85 tracks refreshing dynamic
endpoint data.
**Minimize subprocess count**. Create+start is one
`container run --detach` call. The immutable UID, image, and label identity
is cached after validation, while endpoint, Host, lifecycle, and reuse
operations refresh dynamic network, IP, state, and port-binding data from
inspect. Incomplete Created data is refreshed instead of cached, and the
lifecycle state is re-queried. This avoids stale endpoint data without treating
published ports as immutable.

**Wait via connections where possible**. `ForListeningPort` and
`ForHTTP` use the resolved endpoint directly. `ForExec` and state
queries invoke the backend CLI; `ForLog` uses the streaming logs API.
The default polling intervals are 100ms for connection probes and
250ms for exec probes. Ongoing lifecycle queries are limited to once per
second, while a log-stream failure is classified immediately.

**Never serialize parallel startups**. The library holds no global
lock for container creation (reaper ID registration takes a mutex for
a one-line write). Apple Container consumes no host ports by default;
Docker's daemon assigns published ports atomically.

**Bound snapshots only when requested**. `Logs` and a supported
`LogsWithOptions` call finish a finite CLI snapshot, but `Logs` can buffer
all available output. `LogsOptions{Tail, Since}` is backend-specific:
Docker accepts both options, while the checked Apple Container CLI
versions use `-n` for Tail and have no Since option. The current checkout
still sends Docker
flag spellings to Apple, so the option contract is pending #82. Neither
path adds a byte cap. `FollowLogs` is intentionally an unbounded stream;
closing its `io.ReadCloser` or cancelling its context terminates the CLI
process. The wait-failure diagnostic tail is capped at 1MiB.

**Apply operation-specific deadlines**. Most query-like operations
(inspect, copy, snapshot logs, delete, and prune) use a 30-second
default when the caller has no deadline. `Run` and explicit image
fetches use a 10-minute pull/create budget. A shared pull leader is
intentionally detached from an individual caller's cancellation so one
caller cannot abort the pull for other waiters; each waiter still stops
waiting when its own context is cancelled. `Exec`'s command invocation
and `FollowLogs` pass the caller's context through without adding a
library deadline; an `Exec` infrastructure check may use a bounded
follow-up context. When a library default is applied to an operation
that is not a shared pull leader, an existing caller deadline is
preserved.

## Error handling

Root and backend errors are intended to be discriminable with
`errors.Is`/`errors.As`, but the current checkout has additional limits:
`Classify` formats the original CLI error into the `ErrSystemNotRunning`
wrapper as text rather than retaining it as an unwrap target (#104). A
successful inspect response with no matching target may return a generic
error instead of `ErrContainerNotFound` (#103). For Docker, this includes
empty or malformed inspect data, and the current parser does not verify
that a returned object matches the requested ID or name; a valid but
mismatched object is not a reliable no-match signal. The built-in wait
strategies also do not preserve context errors uniformly in every
timeout/cancellation path (#92). Some primitive timeout errors and
`ForLog` cancellations are string-only, while composite strategies may
retain a context error. Do not assume one error-chain contract until
these follow-up issues are applied.

- `ErrSystemNotRunning`: a non-zero CLI exit was followed by a failed
  backend liveness probe. Apple Container's hint is `container system
  start`; Docker's hint is to start the Docker daemon. Missing or
  unlaunchable CLI binaries remain launch errors. The current wrapper
  keeps the sentinel but flattens the original `*CLIError` into text
  (#104).
- `ErrContainerNotFound`: a classified CLI-reported missing-container
  failure. A successful inspect response with no matching target is not
  guaranteed to produce this sentinel and may return a generic error
  (#103). For Docker, empty or malformed inspect data can take that
  generic path, while a valid but mismatched object is not rejected as a
  no-match by the current parser.
- `ErrImageNotFound`: the current `PullNever` precheck found no local
  image. On Apple this is a best-effort backend-specific precheck, not a
  no-fetch guarantee (#112).
- `ErrPortNotExposed`: a port was neither declared nor published, or
  the declared port had no usable host binding.
- `ErrGenerationReplaced`: a delete-time generation check found a
  same-name replacement. The current reuse path does not perform the
  final post-readiness check tracked by #83 and #84.
- `*CLIError`: a backend CLI exited non-zero. It carries the binary,
  arguments, exit code, and stderr (the diagnostic stderr copy is
  capped at 64KiB). The root `CLIError` alias is a current-development
  addition and is not part of `v0.2.0`; a current
  `ErrSystemNotRunning` classification may flatten this original error
  into text (#104).
- `ErrContainerNotFound` and `ErrGenerationReplaced` are also
  current-development additions.
- `ErrSystemNotRunning`: after a CLI failure, a follow-up
  `container system status` probe failed too; the message tells the
  user to run `container system start`
- `ErrContainerNotFound`: not-found from inspect and friends
- `ErrPortNotExposed`: a port was not declared or has no usable host
  binding
- `ErrEnvFileUnsupported`: a non-empty environment map needs a secure env
  file, but the current platform cannot provide per-user secrecy
- `ErrCopyFileNotRegular`: a Docker copy-out destination is not a regular
  file
- `ErrCopyFileFromContainerUnsupported`: the selected backend or host
  cannot perform a type-safe copy-out (Apple Container, Docker client or
  server below 29.7.0, hosts without the required open flags, or Windows
  Go 1.23 through 1.25)
- `ErrInvalidConfig` / `*ConfigError`: a backend-incompatible option
  combination rejected before creation
- `ErrEndpointUnreachable`: an inspected binding, notably remote-daemon
  loopback, cannot be reached by the client
- `*CLIError`: any other CLI failure; carries the subcommand, exit
  code, and stderr (capped at 64KiB)

When a non-reuse `Run` fails after a successful backend `run`, the
post-create rollback error is returned. If rollback deletion fails, its
message says that the container was left behind. A failed backend `run`
uses the separate best-effort `cleanupFailedCreate` path; its cleanup
errors are not joined to the original classified error. Reaper
registration and deletion errors are likewise not returned. Reuse
reports the wait failure while leaving the shared container in place.

The library never runs `container system start` itself: the command
can prompt interactively for a kernel install, which a test library
must not trigger implicitly.

## Package layout

The following is the core layout, not an exhaustive file listing. The
root package contains the public API and backend-neutral lifecycle
code. Backend implementations own argv assembly and inspect
normalization.

```
container-go/
├── container.go      // Run, Container, endpoint and state methods
├── options.go        // public functional options and validation
├── wait_adapter.go   // adaptation from Container to wait.Target
├── pull.go           // pull policies and in-process pull aggregation
├── reuse.go          // WithReuse and PruneReuseGroup
├── cleanup.go        // Cleanup, TerminateContainer, Prune
├── reaper.go         // watchdog reaper
├── exec.go           // Exec and ExecOption
├── logs.go           // Logs, LogsWithOptions, FollowLogs
├── copy.go           // WithFiles and copy methods
├── errors.go         // public error values
├── backend.go        // backend selection
├── engine.go         // backend interface and normalized info
├── engine_apple.go   // Apple Container argv/inspect adapter
├── engine_docker.go  // Docker argv/inspect adapter
├── namelock.go       // cooperating-process name lock
├── flight.go         // in-process single-flight helper
├── internal/cli/     // CLI runner, streaming, and error classification
├── internal/inspect/ // inspect JSON models and decoding
└── wait/             // public readiness strategies
```

The `internal/cli` runner is an interface; tests inject a fake. It is
an internal package, so external users should depend on the public
root and `wait` packages rather than importing it.

The internal runner contract is:

```text
type Runner interface {
    Run(ctx context.Context, args ...string) (stdout []byte, stderr []byte, err error)
}
```

## Testing strategy

**Unit tests** inject a fake `Runner` returning canned JSON and verify
argv assembly, JSON decoding, error classification, and wait strategy
logic without a real backend. Dependencies that production code assumes
non-nil receive real fakes in tests, never nil. The non-integration
`examples/compile_test.go` extracts every fenced code block from both
language versions of the README and both design documents, compares the
paired blocks after removing comments, parses the `go` blocks, and compiles
them in a temporary module with a quoted local `replace` to this checkout.
The `text` signature inventories are intentionally not executable.

**Integration tests** use the `integration` build tag. The root suite
contains Apple Container and Docker lifecycle, connection, exec, copy,
cleanup, reuse, and watchdog cases. Each backend-specific helper checks
its CLI/service first and skips cleanly when it is unavailable.
`make integration` runs both backends and skips the pull-heavy bench
and single-flight scenarios; `make integration-docker` selects Docker;
`make bench-integration` runs the pull-heavy scenarios and the separate
benchmark module (the nested `bench/` module requires Go 1.25+).

**CI**: `.github/workflows/ci.yml` runs unit tests and race tests on
`ubuntu-latest` with Go 1.23.0 and the stable Go release, plus lint and
`govulncheck`. It also runs the Docker integration matrix on
`ubuntu-latest`. Apple Container integration is intentionally local:
the hosted Linux runners do not provide the Apple Container service or
the required host environment. `CONTAINERGO_BACKEND=apple` or
`CONTAINERGO_BACKEND=docker` can select one backend locally.
**Integration tests**: split off behind the `integration` build tag
and run against real backends. Apple tests require macOS 26 with Apple
Container up; Docker tests require a running Docker daemon. The Apple
watchdog test is Darwin-only, and Windows runtime integration is not run
by the repository's Linux-only CI jobs. They check backend availability
first and skip when the daemon or service is down. They cover startup,
connection, exec, copy, normal cleanup, and the Darwin watchdog. Windows
copy-out must be verified manually with Go 1.26+ and Docker client/server
29.7.0+.

**CI**: unit, race, vet, lint, and Docker integration jobs run on Ubuntu;
Apple integration remains local because its service and host requirements
are not available in those runners. No Windows runtime job is configured.

## Backends (v0.2 and current development)

v0.1 was Apple Container only. v0.2 adds a Docker backend so the same
API works on Linux and Windows.

**Selection**: the environment variable `CONTAINERGO_BACKEND` wins,
accepting `apple` or `docker`. Unset, the OS decides: macOS gets Apple
Container; Linux and Windows get Docker. macOS users wanting Docker
Desktop set `CONTAINERGO_BACKEND=docker`.

**Strategy**: Docker is also a CLI wrapper (`docker` via `os/exec`).
container-rs talks to the Docker Engine API directly; this library
does not. A direct API client means hand-implementing tar packing, log
stream demultiplexing, registry auth, and Windows named pipes, while a
CLI wrapper shares the existing runner layer (argv execution,
timeouts, streaming) unchanged. `DOCKER_HOST`, contexts, and auth
resolution stay the docker CLI's job.

**Internal structure**: a backend is an internal interface owning only
argv assembly and inspect normalization. Process execution (the
runner), wait strategies, cleanup, and validation are shared. The
normalized record holds lifecycle state (created, running, restarting,
stopping, stopped, paused, or unknown), labels, image reference, a backend ID
when available, container IP, and host-side port bindings (container port
→ host address and port). The backend ID is the stable identity; IP
addresses and bindings are dynamic, even though the current endpoint cache
retains them.

**Image handling**: both backends implement the pull policy through
explicit image inspection and pull commands. The Docker run argv adds
`--pull=never`; Apple Container uses the same explicit policy path.
Concurrent pulls are aggregated only within the current process and
for the same backend, image, platform, and operation.

**Endpoint differences**: Docker Desktop (macOS / Windows) does not
route to container IPs from the host, so the Docker backend defaults
to the published-port model testcontainers uses. Ports declared via
`WithExposedPorts` are automatically published to random ports:
locally `-p 127.0.0.1::<port>`, on a detected non-loopback
`DOCKER_HOST=tcp://...` daemon `-p 0.0.0.0::<port>` so the client can
reach it; `Host` returns `127.0.0.1` (or the host from a `tcp://`
`DOCKER_HOST`) and `MappedPort` the assigned host port. Loopback and
unspecified binds are rewritten to `defaultHost()`, so a `127.0.0.1`
binding observed on that detected remote daemon still resolves to the
remote host. An explicit `WithPublishedPort` loopback bind on that
detected remote daemon is rejected by `Run`: Docker would listen on the
remote machine's loopback, which no client-side rewrite can reach.
stopping / unknown), labels, immutable identity, image, container IP,
Docker network mode, and host-side port bindings (container port →
host address and port).

**Endpoint differences**: Docker Desktop (macOS / Windows) does not
route to container IPs from the host, so the Docker backend defaults
to the published-port model testcontainers uses.
Ports declared via `WithExposedPorts` are automatically published to
random ports: locally `-p 127.0.0.1::<port>`, on a remote daemon
`-p 0.0.0.0::<port>` so the client can reach it. Remote means any
`DOCKER_HOST` that does not name this machine — `tcp://host`,
`ssh://user@host`, or a scheme-less `host:port` / hostname
— while `unix://`, `npipe://`, loopback addresses, and an
empty value stay local. Docker CLI client protocols are `unix`,
`tcp`, `npipe`, and `ssh`; other schemes are not usable
`DOCKER_HOST` endpoints. `Host` returns `127.0.0.1` (or the remote
hostname) and `MappedPort` the assigned host port.
Unspecified binds resolve through loopback while preserving IPv4/IPv6
family; explicit IPv6 addresses are canonicalized through `netip`.
For `ssh://` that hostname must be directly dialable; the CLI's SSH
session carries only the Docker API, so an alias behind a ProxyJump or
bastion needs a manual `ssh -L` forward.
An explicit or reused loopback binding on a remote daemon is rejected:
it listens on the remote machine's loopback, which no client-side
rewrite can reach.
Only `DOCKER_HOST` is honored; a `docker context` pointing at a remote
daemon is not detected.
The daemon assigns ports atomically at start, so the free-port race
avoided on Apple Container does not reappear.
The Apple backend's direct-IP default is unchanged.

**Cleanup differences**: the watchdog reaper switches its delete
subcommand per backend (`delete --force` for Apple, `rm --force` for
Docker). It depends on `/bin/sh` and does not run on Windows; Windows
relies on `Cleanup` and the normal rollback paths. On this checkout the
Docker-ID registration prerequisite (#73) is not yet present, so the
Docker reaper path is not active even though normal Docker handles use
the immutable ID. `Prune` uses daemon-side filters on Docker, but the
current filter selects only `status=exited`; Docker's `dead` state is
not selected until #113 is applied.

**Liveness detection**: the probe command switches per backend
(`system status` for Apple, `version --format {{.Server.Version}}` for
Docker).

## Out of scope

- Dockerfile builds via `container build` / `docker build`.
- Network creation and management. `WithNetwork` can attach a container
  to an existing named network, but the library does not create networks.
- Volume creation and lifecycle management. `WithMounts` can use bind,
  named-volume, or tmpfs mounts, but their lifecycle belongs to the
  caller. Current bind-source validation is Unix-style; Windows and
  remote Docker bind-source semantics are pending #76.
- Dockerfile builds via `container build` / `docker build`
- Network creation and management (`WithNetwork` can attach an existing
  Docker network)
- Volume creation and management
- High-level packages equivalent to testcontainers modules (postgres
  and the like; revisit once the core is stable).
- A direct Docker Engine API client. The CLI wrapper is the current
  transport; a direct client remains a future decision, not a hidden
  fallback.

## Unresolved product decisions

These are deliberately recorded rather than implied by the current API:

- **Apple name-based deletion**: a non-empty, matching creation-generation
  check and per-name `flock` protect the generation-checked
  `Terminate`/failed-create paths, as well as `Prune` and `PruneReuseGroup`,
  on one host. The external reaper does not take that lock, and the current
  base cannot distinguish an external CLI delete/recreate in the same window.
  Issues #83 and #84 track the ownership and final-verification gaps;
  closing the external race requires an immutable identity or an atomic
  conditional delete from the backend.
- **Remote Docker detection**: only `DOCKER_HOST=tcp://...` is used
  for endpoint selection. A remote Docker context is not detected.
- **Reuse compatibility**: image is checked on both backends. Docker
  also checks published port bindings; Apple cannot compare
  `WithExposedPorts` declarations because they are not retained in its
  inspect data. `env`, `cmd`, and `mounts` differences intentionally
  attach. Whether a future release should compare more configuration is
  an open product decision; callers needing isolation should use
  distinct names.
- **Logger injection**: no public logger hook exists. Adding one would
  require a separate API and a decision about what command data may be
  exposed.
- **Windows and remote bind mounts**: current validation is Unix-style and
  does not establish that a remote Docker daemon can resolve a client
  host path; #76 tracks the required capability boundary.
- **UDP readiness**: `ForListeningPort` and `ForExposedPort` are TCP-only,
  while UDP can be declared for endpoint configuration; #77 tracks
  fail-fast protocol validation.
- **Stop timeout units**: current conversion truncates fractions and does
  not validate negative, overflow, or backend limits; #89 tracks the
  public contract.
- **Wait error chains**: built-in strategies do not uniformly preserve
  context errors; #92 tracks normalization.
- **Public option validation**: negative log tails, zero memory, unknown
  mount types, and reuse-group grammar are not uniformly rejected; #102
  tracks typed validation.
- **Reaper staging**: the current reaper can leave environment-bearing
  inspect output on disk after abnormal termination; #111 tracks cleanup
  and exposure mitigation.
- **Reaper registration**: registration is best-effort and currently
  accepts only Apple-style names. Docker's full ID requires the #73
  prerequisite before it can be registered. A registration failure is
  ignored and normal cleanup remains the fallback.
- **`ForLog.WithPollInterval`**: the current setter has no effect because
  log readiness is stream-based. Whether to remove it or give it
  different semantics is unresolved.
- **Direct Docker Engine API**: the current benchmark decision defers
  it; revisit only if CLI latency or another requirement crosses the
  agreed threshold.

## Historical implementation phases

The following list records the original implementation order. It is
kept for design history, not as a current API or roadmap contract.

1. Project foundation: go.mod, CI, Makefile
2. CLI runner layer: `internal/cli`, timeouts, error classification,
   `ErrSystemNotRunning` detection
3. inspect JSON models: `internal/inspect`
4. Core API: `Run`, options, `Container` lifecycle (start, Stop,
   Terminate, rollback)
5. Connection info API: `Host`, `MappedPort`, `Endpoint`,
   `ContainerIP`, `WithPublishedPort`
6. Wait strategies: the `wait` package
7. Exec, Logs, Copy
8. Cleanup: `Cleanup`, `Prune`, session labels, the watchdog reaper
9. Security pass: env-file environment variables, exhaustive input
   validation, log masking
10. Integration tests and runbook
11. Documentation and examples: README, usage samples

v0.2 (Docker backend) proceeded as:

12. Backend abstraction: interface over argv assembly and inspect
    normalization; carve the Apple implementation out with tests green
13. Docker engine: argv and JSON parsing for run / inspect / lifecycle
    / exec / logs / copy
14. Docker connection info: random published ports, `Host` /
    `MappedPort`, `DOCKER_HOST`
15. Backend selection and surroundings: `CONTAINERGO_BACKEND`, OS
    defaults, reaper / Prune / probe switching
16. Docker integration tests and documentation updates

## References

- [apple/container](https://github.com/apple/container) v1.2.2 and v1.3.0
  command/source references and `ContainerResource` sources
- [shiguredo/container-rs](https://github.com/shiguredo/container-rs):
  the direct-XPC prior art; its watchdog reaper, cleanup contract, and
  catalog of macOS-specific constraints (port races, forwarding
  truncation) informed this design
- [testcontainers-go](https://github.com/testcontainers/testcontainers-go)
  v0.44.0: source of the API shapes (functional options, wait
  strategies, nil-safe cleanup)
