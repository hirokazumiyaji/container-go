# container-go Design Document

日本語版: [design.ja.md](design.ja.md)

Created: 2026-08-18 (v0.2 backend section added 2026-08-19)
Last synchronized: 2026-09-25
Targets: Apple Container v1.2.x–1.3.x (macOS 26+, Apple Silicon), Docker 29.x (Linux, Windows, macOS), Go 1.23+

This document describes the current implementation in this checkout.
The **Implementation phases** section is retained as a historical plan;
it is not a promise that every phase is still a current API or roadmap.
The public API and behavior described in the current sections come from
the implementation and its tests.

The latest tagged release is `v0.2.0` (2026-09-02). This checkout is
development after that tag. The `v0.2.0` module requires Go 1.27 or
later; this checkout requires Go 1.23 or later. The released API and the
current development API are not interchangeable: `LogsOptions` /
`LogsWithOptions`, the additional `wait.ForHTTP` setters, exported
`wait.AllStrategy` / `AnyStrategy` with composite `WithStartupTimeout`,
and the root `CLIError`, `ErrContainerNotFound`, and
`ErrGenerationReplaced` symbols are development additions after
`v0.2.0`. The core `Run`, options, lifecycle, endpoints, `Exec`, `Logs`,
`FollowLogs`, copy, pull policies, reuse, cleanup, backend selection, and
original wait strategies were present in `v0.2.0`.

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
- **Security**: no injection or information-leak paths through
  subprocess invocation or user input.
- **Performance**: container (VM) startup dominates test suite time;
  the library's own overhead must stay negligible against that, and it
  must never serialize parallel startups.

## Apple Container facts the design relies on

The design decisions below rest on these properties of Apple Container
(verified against v1.2.x–1.3.x; fixtures cover 1.2.2 and 1.3.0).

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
- The container name is the container ID. Names must match
  `^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$` (one to 63 characters).
- Several Docker features do not exist: healthchecks, a `wait`
  command, an event stream, label filters on `ls`, and re-attaching to
  a running container. Their behavior must be reproduced client-side.
- `--label` exists, but filtering by label means filtering the JSON
  output client-side. Label keys are restricted to lowercase
  Docker/OCI-style keys.
- `container cp` only works on running containers.
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
explicit pull for every new-container attempt, and `PullNever` only
inspects and returns
`ErrImageNotFound` when the image is absent. Docker's run command also
passes `--pull=never`, so a pull is not duplicated by the CLI. If a
post-start operation or wait fails on the non-reuse path, `Run` rolls
back the container it created before returning the error. Reuse has
the separate shared-lifetime contract described below.

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
  `containergo-<random hex>`).
- `WithLabels(labels map[string]string)`: extra labels. The library's
  managed labels are reserved.
- `WithMounts(mounts ...Mount)`: bind, named-volume, and tmpfs mounts.
- `WithFiles(files ...File)`: files copied into the running container
  after start; a copy failure rolls back `Run`.
- `WithPublishedPort(spec string)`: explicit host-side port publishing.
  It is optional because Apple Container uses a direct IP and Docker
  auto-publishes exposed ports.
- `WithPullPolicy(policy PullPolicy)`: choose `PullMissing` (default),
  `PullAlways`, or `PullNever`.
- `WithReuse()`: make a named `Run` a get-or-create operation.
- `WithReuseGroup(group string)`: label a reused container for
  `PruneReuseGroup`; it requires `WithReuse` and is not part of the
  reuse key.
- `WithCPUs(n int)` / `WithMemory(size string)`: resource limits.
- `WithUser(u string)` / `WithWorkingDir(dir string)`: process user
  and working directory.
- `WithNetwork(name string)`: attach to an existing named network.
- `WithPlatform(p string)`: select an image platform such as
  `linux/amd64` (including Rosetta use on Apple Container).

Options outside the backend-neutral CLI surface are intentionally
omitted. There is no public logger-injection option. For log consumers,
`FollowLogs` returns a stream and `Logs`/`LogsWithOptions` return
snapshots.

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

`Exec` returns the exit code and combined stdout+stderr. A command
that runs in a running container and exits non-zero is a result rather
than an infrastructure error; a stopped or unreachable container can
still return an error. `WithExecEnv`, `WithExecUser`, and
`WithExecWorkDir` configure an individual exec invocation; environment
values use the same temporary-file mechanism as `WithEnv`. `Logs` and
`LogsWithOptions` return a finite snapshot because the backend command
finishes, but they do not impose a byte limit by themselves: `Logs`
requests all output, while `LogsOptions{Tail, Since}` requests a
backend-selected line or time window when set, whose size still depends
on the container output. `FollowLogs` is intentionally an unbounded stream and
continues until its reader is closed or its context is cancelled.
`LogsOptions` and `LogsWithOptions` are current-development APIs, not
part of `v0.2.0`. `Terminate` is generation-guarded: it refuses to
delete a name recycled by another process (see Reuse below).

`Terminate` maps to `container delete --force` on Apple Container and
`docker rm --force` on Docker, and is idempotent when the container is
already absent. `Cleanup(t, ctr)` and `TerminateContainer(ctr)` are
nil-safe helpers preserving the testcontainers-go idiom of registering
cleanup before checking `Run`'s error.

## Connection endpoints

testcontainers' Docker implementation publishes container ports to
random host ports and connects to `localhost:<mapped>`. On Apple
Container this is not the default.

By default, `Host` returns the container's real IP (from inspect's
`status.networks[0].ipv4Address`, CIDR suffix stripped) and
`MappedPort` returns the container port unchanged. Three reasons:

- Apple Container has no random port assignment; grabbing a free host
  port up front races between "find free port" and "start container"
  (container-rs documents the same race as a known limitation). Direct
  IP connection consumes no host ports, so the race does not exist.
- With no host-port collisions, parallel test runs scale without
  limit.
- No port-forwarding proxy is involved, avoiding its failure modes
  (silently truncated large transfers have been reported).

When a client demands a `localhost` endpoint (or the container IP is
unreachable in a given setup), publish explicitly with
`WithPublishedPort("127.0.0.1:15432:5432")`. Then `Host` returns the
given host address and `MappedPort` the host port.

`MappedPort` and `Endpoint` return `ErrPortNotExposed` for ports that
are neither declared via `WithExposedPorts` nor explicitly published.
The declarations also feed wait strategies: `ForExposedPort` uses the
first declared port, and an empty `Target.Endpoint` port selects that same
first declaration.

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

The primitive strategies default to a 60-second startup timeout.
`ForListeningPort`, `ForExposedPort`, and `ForHTTP` poll every 100ms;
`ForExec` polls every 250ms. The current `ForLog` type also exposes
`WithPollInterval`, but it has no effect because it consumes a stream.
Connection and HTTP strategies check the stopped state at most once per
second. `ForExec` does not fail fast while polling; it checks the
container state when its wait deadline expires. `ForLog` reports a
stopped container when its stream ends before the pattern appears. A
failed non-reuse `Run` wait rolls the container back and, when the
bounded log fetch succeeds, attaches a log tail capped at 1MiB to the
error; a reuse wait leaves the shared container in place.

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
Apple Container and published-port Docker. The dependency points from
`container` to `wait`, never back, avoiding an import cycle.

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
best-effort path. Errors from its lock, inspect, or delete operations are
not joined to the returned `Run` error.

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
creation label before deleting by name. That name-based guarantee is
limited to cooperating processes using this library on the same host; an
external CLI delete/recreate cannot be distinguished by name.

The Docker branch is prepared to use the immutable 64-hex ID returned by
`docker run`, but this checkout does not yet accept that ID in
`reaper.register`; it accepts only Apple-style names. The normal Docker
`run --detach` path therefore cannot register that ID with the reaper on
this base. If a backend returns no parseable ID, `Run` falls back to the
name-and-generation path. Issue #73 must be stacked for the Docker-ID
registration path to work. After #73, Docker entries use the immutable ID
and `docker rm --force`; normal Docker `Terminate` and rollback handles can
use the ID independently.

**Session labels**: every created container carries

- `com.github.hirokazumiyaji.container-go`: `true` (managed-by marker)
- `com.github.hirokazumiyaji.container-go.session`: a per-process
  random ID
- a creation generation label, and, for reuse, the reuse/group labels

The Apple CLI has no label filter, so orphan sweeps filter
`container ls -a --format json` client-side. `Prune(ctx)` removes
stopped containers carrying the managed label from any session. Docker
can apply the same filter daemon-side.

Setting `CONTAINERGO_KEEP=1` skips `Cleanup`, `TerminateContainer`,
and reaper registration. It does not change an explicit
`Container.Terminate`, post-create rollback, or failed-create cleanup.

Anonymous volumes survive `--rm`, so the library never creates one;
volumes must be named, and their lifecycle belongs to the caller.

## Reuse

`WithReuse` turns `Run` into a get-or-create for a stable `WithName`
(shared across processes on the same host). An existing container
must have been created with `WithReuse`, and every caller reruns its
readiness strategy. Image compatibility is checked on both backends.
Port compatibility is backend-specific. Docker compares auto-published
bindings for `WithExposedPorts` and checks explicit `WithPublishedPort`
bindings. Apple checks explicit published bindings, but the library's
Apple inspect model does not retain `WithExposedPorts` declarations, so
those declarations cannot be compared. Each Apple caller uses its own
exposed-port declaration against the shared container IP; use distinct
names when those declarations must be isolated. `env`, `cmd`, and `mounts`
differences attach silently to the existing container by design;
callers needing isolation should use distinct names or reset state via
`Exec`.

Each creation carries a `creationLabel` generation (16-hex). `Terminate`
and the stopped-recreate path refuse to delete a replaced name. On
Docker the handle keeps the immutable `Id` printed by `docker run` (or
returned by inspect) and deletes by it, so no generation check is
needed: a replacement never shares the ID. Apple Container addresses
containers by name only, so there the delete is name-based: the
generation must match a fresh inspect, and inspect plus delete run
under a per-name `flock` in the temp directory (`containergo-<name>.lock`)
that every such delete in this library takes. That guarantee is
limited to cooperating processes using this library on the same host:
a direct `container delete` plus re-create by an external tool inside
that window is indistinguishable by name, and closing it would need an
immutable ID or an atomic conditional delete that Apple Container does
not provide. An inspect
failure other than not-found aborts the delete (fail closed); `Run`'s
rollback reports a container left behind that way in its error rather
than hiding it. The watchdog reaper registers Apple containers by name and generation,
reading the label as a line-anchored JSON field (`"key": "value"`, never a
substring), and skips deletion on mismatch. The current base does not
register Docker's immutable ID with the reaper; issue #73 is required
before Docker reaper entries can use `Id`. Each backend call carries a
10-30s timeout via POSIX `sleep`/`kill` (no `timeout(1)` dependency) so
one hung daemon call cannot wedge the rest. The leader's own pull/create
uses an independent `runTimeout` budget; `reuseAttachTimeout` bounds only
attach polling for another process's container.

## Security design

As a library that spawns subprocesses, these rules hold.

**No shell involvement**. Every CLI call passes an argv array to
`exec.Command`; no shell string is ever assembled. The single
exception is the watchdog reaper's shell script. Its body is a fixed
string; container IDs enter only as stdin data. The script defeats
word splitting and globbing (`set -f`, `IFS=`, `read -r`, quoted
expansions). On this base, the library validates reaper targets as
Apple-style names matching `^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$` before
writing them to the pipe. After issue #73, the same validation will also
accept a full lowercase 64-hex Docker ID. Reaper registration failures
are ignored. The two layers together leave no command injection through
registered IDs.

**No environment variables on argv**. `--env key=value` exposes values
to every user via `ps`. Because environment variables are the main
channel for secrets (database passwords and the like), the library
writes them to a file under `os.MkdirTemp` with mode 0600, passes
`--env-file`, and deletes the file after startup.

**Validate inputs**. Container names (name rule above), label keys
(the CLI's Docker/OCI form), ports (numeric range and `tcp`/`udp`),
environment keys (no `=`, no NUL), and container-side copy paths
(absolute, valid UTF-8) are all validated before reaching the CLI.
Host-side copy paths are resolved to absolute paths. The CLI validates
too, but validating first gives clearer errors and independence from
future CLI changes.

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
`run --detach` call. Immutable facts (configuration, labels, image,
network address, and published ports) are cached from the first
inspect; state is re-queried when requested.

**Wait via connections where possible**. `ForListeningPort` and
`ForHTTP` use the resolved endpoint directly. `ForExec` and state
queries invoke the backend CLI; `ForLog` uses the streaming logs API.
The default polling intervals are 100ms for connection probes and
250ms for exec probes.

**Never serialize parallel startups**. The library holds no global
lock for container creation (reaper ID registration takes a mutex for
a one-line write). Apple Container consumes no host ports by default;
Docker's daemon assigns published ports atomically.

**Bound snapshots only when requested**. `Logs` and `LogsWithOptions`
finish a finite CLI snapshot, but `Logs` can buffer all available output
and `LogsWithOptions` limits by the requested line or time window rather
than a byte cap. `FollowLogs` is intentionally an unbounded stream;
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

Errors are discriminable with `errors.Is`/`errors.As`.

- `ErrSystemNotRunning`: a CLI failure was followed by a failed backend
  liveness probe. Apple Container's hint is `container system start`;
  Docker's hint is to start the Docker daemon.
- `ErrContainerNotFound`: a container operation could not find the
  container.
- `ErrImageNotFound`: `Run` with `PullNever` found no local image.
- `ErrPortNotExposed`: a port was neither declared nor published, or
  the declared port had no usable host binding.
- `ErrGenerationReplaced`: a generation-guarded delete refused to
  remove a same-name replacement.
- `*CLIError`: a backend CLI exited non-zero. It carries the binary,
  arguments, exit code, and stderr (the diagnostic stderr copy is
  capped at 64KiB). The root `CLIError` alias is a current-development
  addition and is not part of `v0.2.0`.
- `ErrContainerNotFound` and `ErrGenerationReplaced` are also
  current-development additions.

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
`examples/compile_test.go` extracts every fenced `go` block from both
language versions of the README and both design documents, parses it, and
compiles it in a temporary module with a local `replace` to this checkout.
The `text` signature inventories are intentionally not executable.

**Integration tests** use the `integration` build tag. The root suite
contains Apple Container and Docker lifecycle, connection, exec, copy,
cleanup, reuse, and watchdog cases. Each backend-specific helper checks
its CLI/service first and skips cleanly when it is unavailable.
`make integration` runs both backends and skips the pull-heavy bench
and single-flight scenarios; `make integration-docker` selects Docker;
`make bench-integration` runs the pull-heavy scenarios and the separate
benchmark module.

**CI**: `.github/workflows/ci.yml` runs unit tests and race tests on
`ubuntu-latest` with Go 1.23.0 and the stable Go release, plus lint and
`govulncheck`. It also runs the Docker integration matrix on
`ubuntu-latest`. Apple Container integration is intentionally local:
the hosted Linux runners do not provide the Apple Container service or
the required host environment. `CONTAINERGO_BACKEND=apple` or
`CONTAINERGO_BACKEND=docker` can select one backend locally.

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
normalized record holds state (mapped onto running / stopped /
stopping / created / unknown), labels, image reference, immutable
backend ID when available, container IP, and host-side port bindings
(container port → host address and port).

**Image handling**: both backends implement the pull policy through
explicit image inspection and pull commands. The Docker run argv adds
`--pull=never`; Apple Container uses the same explicit policy path.
Concurrent pulls are aggregated only within the current process and
for the same backend, image, platform, and operation.

**Endpoint differences**: Docker Desktop (macOS / Windows) does not
route to container IPs from the host, so the Docker backend defaults
to the published-port model testcontainers uses. Ports declared via
`WithExposedPorts` are automatically published to random ports:
locally `-p 127.0.0.1::<port>`, on a remote daemon
(`DOCKER_HOST=tcp://host`) `-p 0.0.0.0::<port>` so the client can reach
it; `Host` returns `127.0.0.1` (or the host from a `tcp://`
`DOCKER_HOST`) and `MappedPort` the assigned host port. Loopback and
unspecified binds are rewritten to `defaultHost()`, so a `127.0.0.1`
binding observed on a remote daemon still resolves to the remote host.
An explicit `WithPublishedPort` loopback bind on a remote daemon is
rejected by `Run`: Docker would listen on the remote machine's loopback,
which no client-side rewrite can reach.
Only `DOCKER_HOST` is honored; a `docker context` pointing at a remote
daemon is not detected. The daemon assigns ports atomically at start,
so the free-port race avoided on Apple Container does not reappear.
The Apple backend's direct-IP default is unchanged.

**Cleanup differences**: the watchdog reaper switches its delete
subcommand per backend (`delete --force` for Apple, `rm --force` for
Docker). It depends on `/bin/sh` and does not run on Windows; Windows
relies on `Cleanup` and the normal rollback paths. On this checkout the
Docker-ID registration prerequisite (#73) is not yet present, so the
Docker reaper path is not active even though normal Docker handles use
the immutable ID. `Prune` can use daemon-side filters on Docker
(`--filter label=... --filter status=exited`).

**Liveness detection**: the probe command switches per backend
(`system status` for Apple, `version --format {{.Server.Version}}` for
Docker).

## Out of scope

- Dockerfile builds via `container build` / `docker build`.
- Network creation and management. `WithNetwork` can attach a container
  to an existing named network, but the library does not create networks.
- Volume creation and lifecycle management. `WithMounts` can use bind,
  named-volume, or tmpfs mounts, but their lifecycle belongs to the
  caller.
- High-level packages equivalent to testcontainers modules (postgres
  and the like; revisit once the core is stable).
- A direct Docker Engine API client. The CLI wrapper is the current
  transport; a direct client remains a future decision, not a hidden
  fallback.

## Unresolved product decisions

These are deliberately recorded rather than implied by the current API:

- **Apple name-based deletion**: the creation-generation check protects
  cooperating processes using this library on one host, but cannot
  distinguish an external CLI delete/recreate in the same window.
  Closing that gap requires an immutable identity or an atomic
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

- [apple/container](https://github.com/apple/container) v1.2.x–v1.3.x
  command reference and `ContainerResource` sources
- [shiguredo/container-rs](https://github.com/shiguredo/container-rs):
  the direct-XPC prior art; its watchdog reaper, cleanup contract, and
  catalog of macOS-specific constraints (port races, forwarding
  truncation) informed this design
- [testcontainers-go](https://github.com/testcontainers/testcontainers-go)
  v0.44.0: source of the API shapes (functional options, wait
  strategies, nil-safe cleanup)
