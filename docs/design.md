# container-go Design Document

日本語版: [design.ja.md](design.ja.md)

Created: 2026-08-18 (v0.2 backend section added 2026-08-19)
Targets: Apple Container v1.2.x (macOS 26+, Apple Silicon), Docker (Linux, Windows, macOS), Go 1.23+

## Purpose

**container-go** is a testcontainers-style Go library backed by Apple
Container ([apple/container](https://github.com/apple/container)).
It starts throwaway containers from Go tests, hands out connection
endpoints, and guarantees the containers are destroyed when the tests
end.

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
- Each container boots as a lightweight VM with a real IP on a vmnet
  bridge (default network `default`, `192.168.64.0/24`). The host can
  reach that IP directly, so port publishing (`--publish`) is optional.
- Everything is operable through the `container` CLI. `ls --format json`
  and `inspect` emit machine-readable JSON (additive fields are ignored
  by `internal/inspect`).
- The CLI talks XPC to `container-apiserver` under launchd. Commands
  fail while the service is down; `container system status` reports
  its state.
- The container name is the container ID. Names must match
  `^[a-zA-Z0-9][a-zA-Z0-9_.-]+$` and stay within 63 characters.
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
example). The first is contained by only ever parsing `--format json`
output, never text tables. The second is worked around with
client-side filtering.

## Public API

The API shape follows testcontainers-go (v0.44 line) so that existing
testcontainers users need no relearning.

Module path `github.com/hirokazumiyaji/container-go`, root package
`container`.

### Basic usage

```go
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
    ...
}
```

### Run and options

```go
func Run(ctx context.Context, image string, opts ...Option) (*Container, error)
```

`Run` fetches the image (the CLI auto-pulls when missing), creates and
starts the container, and completes the wait strategy; on failure it
rolls back whatever it created before returning the error.

Options use the functional options pattern. The initial release
provides:

- `WithExposedPorts(ports ...string)`: declare the container ports
  (`"6379/tcp"` form) endpoints may resolve
- `WithEnv(env map[string]string)`: environment variables
- `WithCmd(cmd ...string)` / `WithEntrypoint(entrypoint string)`:
  command and entrypoint overrides. Entrypoint is a single token per
  `docker run --entrypoint` semantics; pass multi-token commands via
  `WithCmd`.
- `WithWaitStrategy(s wait.Strategy)`: readiness detection
- `WithName(name string)`: container name (default
  `containergo-<random hex>`)
- `WithLabels(labels map[string]string)`: extra labels
- `WithMounts(mounts ...Mount)`: bind, volume, and tmpfs mounts
- `WithFiles(files ...File)`: files copied into the container after
  start
- `WithPublishedPort(spec string)`: host-side port publishing (off by
  default; see below)
- `WithCPUs(n int)` / `WithMemory(size string)`: resource limits
- `WithUser(u string)` / `WithWorkingDir(dir string)`: process user
  and working directory
- `WithNetwork(name string)`: target network
- `WithPlatform(p string)`: e.g. `linux/amd64` (via Rosetta)

### The Container handle

```go
type Container struct { ... }

func (c *Container) ID() string
func (c *Container) Host(ctx context.Context) (string, error)
func (c *Container) MappedPort(ctx context.Context, port string) (int, error)
func (c *Container) Endpoint(ctx context.Context, port string) (string, error)
func (c *Container) ContainerIP(ctx context.Context) (string, error)
func (c *Container) State(ctx context.Context) (State, error)
func (c *Container) Exec(ctx context.Context, cmd []string, opts ...ExecOption) (int, io.Reader, error)
func (c *Container) Logs(ctx context.Context) (io.ReadCloser, error)
func (c *Container) LogsWithOptions(ctx context.Context, opts LogsOptions) (io.ReadCloser, error)
func (c *Container) CopyToContainer(ctx context.Context, hostPath, containerPath string) error
func (c *Container) CopyFileFromContainer(ctx context.Context, containerPath string) (io.ReadCloser, error)
func (c *Container) Stop(ctx context.Context, timeout *time.Duration) error
func (c *Container) Terminate(ctx context.Context) error
```

`Exec` returns the exit code with combined stdout+stderr (a non-zero
exit is a result, not an error); this is kept for v1 compatibility.
`LogsWithOptions{Tail, Since}` bounds snapshots for long-lived reuse
containers. `Terminate` is generation-guarded: it refuses to delete a
name recycled by another process (see Reuse below).

`Terminate` maps to `container delete --force` and is idempotent
(deleting an already-absent container succeeds). `Cleanup(t, ctr)` and
`TerminateContainer(ctr)` are nil-safe helpers preserving the
testcontainers-go idiom of deferring cleanup before the error check.

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

`MappedPort` errors with `ErrPortNotExposed` for ports not declared
via `WithExposedPorts`. The declarations also feed wait strategies
(the default port of ForListeningPort, for example).

## Wait strategies

Apple Container has neither healthchecks nor a wait command, so
readiness is decided entirely client-side. The `wait` subpackage
provides:

- `wait.ForLog(s string)`: wait until a substring (or regexp via
  `AsRegexp`) appears in `container logs --follow` output;
  `WithOccurrence(n)` for repeat counts
- `wait.ForListeningPort(port string)`: wait until `net.DialTimeout`
  to the container IP succeeds
- `wait.ForHTTP(path string)`: wait until an HTTP request via
  `net/http` matches the status predicate (2xx by default,
  `WithStatusCodeMatcher` to change). `WithPort` / `WithMethod` select
  the target; `WithHeaders` / `WithBasicAuth` / `WithTLS` /
  `WithTLSConfig` / `WithHTTPClient` cover auth, TLS, and custom
  transports without breaking the default plain-HTTP probe.
- `wait.ForExec(cmd []string)`: wait until `container exec` exits with
  an accepted code (0 by default)
- `wait.ForAll(ss ...Strategy)` / `wait.ForAny(ss ...Strategy)`:
  composition. Each child keeps its own `WithStartupTimeout`; bound the
  whole composition with `context.WithTimeout` from the caller rather
  than a synthetic composite deadline.

Every strategy carries `WithStartupTimeout` (default 60s) and
`WithPollInterval` (default 100ms). If the container transitions to
stopped while waiting, the wait fails immediately (no timeout burn)
and the error carries a log tail capped at 1MiB for diagnosis.

The strategy interface:

```go
type Strategy interface {
    WaitUntilReady(ctx context.Context, target Target) error
}
```

`Target` is a small interface (container IP, declared ports, log
reader, exec, state query) implemented by adapting
`*container.Container`. The dependency points from `container` to
`wait`, never back, avoiding an import cycle.

## Cleanup

Every way a test process can exit has a path that still deletes its
containers.

**Normal path**: `Cleanup(t, ctr)` registers `Terminate` via
`t.Cleanup`. Mid-`Run` failures are rolled back by `Run` itself.

**Abnormal exit (SIGKILL, panic, `os.Exit`)**: neither defers nor
`t.Cleanup` run, so an external **watchdog reaper** takes over. At
library initialization one `/bin/sh` child is spawned; container IDs
are registered by writing them down a pipe. However the parent dies,
the pipe reaches EOF, and the reaper runs `container delete --force`
for every registered ID and exits. While the parent lives the reaper
does nothing (deletion belongs to the normal path; the reaper is
insurance). This mirrors container-rs's watchdog and covers SIGKILL,
which no signal handler can.

**Session labels**: every created container carries

- `com.github.hirokazumiyaji.container-go`: `true` (managed-by marker)
- `com.github.hirokazumiyaji.container-go.session`: a per-process
  random ID

The CLI has no label filter, so orphan sweeps filter
`container ls -a --format json` client-side. A helper `Prune(ctx)`
removes stopped containers carrying the managed label from any
session.

Setting `CONTAINERGO_KEEP=1` disables deletion in `Cleanup` and the
reaper (for debugging).

Anonymous volumes survive `--rm`, so the library never creates one;
volumes must be named, and their lifecycle belongs to the caller.

## Reuse

`WithReuse` turns `Run` into a get-or-create for a stable `WithName`
(shared across processes). The compatibility check is intentionally
narrow: image reference and declared/published ports only. `env`,
`cmd`, and `mounts` differences attach silently to the existing
container by design; callers needing isolation should use distinct
names or reset state via `Exec`.

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
than hiding it. The watchdog reaper registers Docker containers by
`Id`; for Apple it stores the generation, reads the label as a
line-anchored JSON field (`"key": "value"`, never a substring), and
skips deletion on mismatch. Each backend call carries a 10-30s timeout
via POSIX `sleep`/`kill` (no `timeout(1)` dependency) so one hung
daemon call cannot wedge the rest. The leader's own pull/create uses an
independent `runTimeout` budget; `reuseAttachTimeout` bounds only
attach polling for another process's container.

## Security design

As a library that spawns subprocesses, these rules hold.

**No shell involvement**. Every CLI call passes an argv array to
`exec.Command`; no shell string is ever assembled. The single
exception is the watchdog reaper's shell script. Its body is a fixed
string; container IDs enter only as stdin data. The script defeats
word splitting and globbing (`set -f`, `IFS=`, `read -r`, quoted
expansions), and the library validates every ID against Apple
Container's name rule `^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$` before
writing it to the pipe. The two layers together leave no command
injection through IDs.

**No environment variables on argv**. `--env key=value` exposes values
to every user via `ps`. Because environment variables are the main
channel for secrets (database passwords and the like), the library
writes them to a file under `os.MkdirTemp` with mode 0600, passes
`--env-file`, and deletes the file after startup.

**Validate inputs**. Container names (name rule above), label keys
(the CLI's Docker/OCI form), ports (numeric range and `tcp`/`udp`),
environment keys (no `=`, no NUL), and copy paths (absolute, valid
UTF-8) are all validated before reaching the CLI. The CLI validates
too, but validating first gives clearer errors and independence from
future CLI changes.

**Handle no credentials**. Registry auth is delegated to
`container registry login` (credentials live in the macOS Keychain);
the library has no credential input path.

**No secrets in logs**. Debug logging of CLI argv never includes
env-file contents.

## Performance design

**Minimize subprocess count**. Create+start is one
`container run --detach` call. Immutable facts (config, labels,
published ports) are cached from the first inspect; only the state is
re-queried.

**Wait via connections, not subprocesses**. ForListeningPort and
ForHTTP dial the container IP directly without spawning the CLI. Only
ForExec and state queries poll through subprocesses, cheap enough at
the 100ms interval.

**Never serialize parallel startups**. The library holds no global
lock (reaper ID registration takes a mutex for a one-line write).
Because the default design consumes no host ports, parallelism is
bounded only by host resources.

**Keep streams finite**. `Logs` returns the `container logs --follow`
child as an `io.ReadCloser` whose `Close` (or context cancellation)
reliably kills the process. ForLog's diagnostic buffer caps at 1MiB.

**Deadline every CLI call**. Every call honors `context` and carries a
default timeout (30s for queries, 10min for pull-bearing runs). On
cancellation the child is SIGKILLed and reaped; no zombies, no hangs.

## Error handling

Errors are discriminable with `errors.Is`/`errors.As`.

- `ErrSystemNotRunning`: after a CLI failure, a follow-up
  `container system status` probe failed too; the message tells the
  user to run `container system start`
- `ErrContainerNotFound`: not-found from inspect and friends
- `ErrPortNotExposed`: querying a port not declared via
  `WithExposedPorts`
- `*CLIError`: any other CLI failure; carries the subcommand, exit
  code, and stderr (capped at 64KiB)

When `Run` fails on a wait timeout, the returned error includes the
container's log tail, and the rollback delete follows.

The library never runs `container system start` itself: the command
can prompt interactively for a kernel install, which a test library
must not trigger implicitly.

## Package layout

```
container-go/
├── container.go      // Run, Container, Option
├── options.go        // functional options
├── cleanup.go        // Cleanup, TerminateContainer, Prune
├── reaper.go         // watchdog reaper
├── exec.go           // Exec
├── logs.go           // Logs
├── copy.go           // CopyToContainer, CopyFileFromContainer
├── errors.go         // error types
├── internal/cli/     // CLI runner (argv assembly, execution, timeouts)
├── internal/inspect/ // inspect JSON models and decoding
└── wait/             // wait strategies
```

The `internal/cli` runner is an interface; tests inject a fake.

```go
type Runner interface {
    Run(ctx context.Context, args ...string) (stdout []byte, stderr []byte, err error)
}
```

## Testing strategy

**Unit tests**: inject a fake `Runner` returning canned JSON and
verify argv assembly, JSON decoding, error classification, and wait
strategy logic without real hardware. Dependencies that production
code assumes non-nil get real fakes in tests, never nil.

**Integration tests**: split off behind the `integration` build tag
and run only on real hardware (macOS 26 with Apple Container up). They
cover startup, connection, exec, copy, cleanup, and the watchdog
(SIGKILL a child process, watch the reaper act). They check
`container system status` first and skip when the service is down.

**CI**: unit tests and `go vet` run in GitHub Actions per push (no
Apple Container needed). GitHub-hosted runners are unlikely to run the
integration tests (macOS version and nested-virtualization limits), so
those stay local as `make integration`.

## Backends (v0.2)

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
normalized record holds four things: state (mapped onto running /
stopped / stopping / unknown), labels, the container IP, and host-side
port bindings (container port → host address and port).

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
Docker). The reaper depends on `/bin/sh` and thus does not run on
Windows; v0.2 documents that Windows relies on the normal cleanup
paths (`Cleanup`, rollback) only. `Prune` can use daemon-side filters
on Docker (`--filter label=... --filter status=exited`).

**Liveness detection**: the probe command switches per backend
(`system status` for Apple, `info` for Docker).

## Out of scope

- Dockerfile builds via `container build` / `docker build`
- Network creation and management (only the default network is used)
- Volume creation and management
- High-level packages equivalent to testcontainers modules (postgres
  and the like; revisit once the core is stable)
- A direct Docker Engine API client (revisit if the CLI wrapper ever
  falls short)

## Implementation phases

Implementation proceeds in this order, one GitHub Issue per phase.

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

v0.2 (Docker backend) proceeds as:

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

- [apple/container](https://github.com/apple/container) v1.2.2
  command reference and `ContainerResource` sources
- [shiguredo/container-rs](https://github.com/shiguredo/container-rs):
  the direct-XPC prior art; its watchdog reaper, cleanup contract, and
  catalog of macOS-specific constraints (port races, forwarding
  truncation) informed this design
- [testcontainers-go](https://github.com/testcontainers/testcontainers-go)
  v0.44.0: source of the API shapes (functional options, wait
  strategies, nil-safe cleanup)
