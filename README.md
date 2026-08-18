# container-go

A [testcontainers](https://testcontainers.com/)-style Go library for
[Apple Container](https://github.com/apple/container): run throwaway
containers from Go tests on macOS, with zero third-party dependencies.

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

## Requirements

- macOS 26 or later on Apple Silicon
- [Apple Container](https://github.com/apple/container) 1.2.x installed,
  with the system service running: `container system start`
- Go 1.26+

The library shells out to the `container` CLI; it does not talk to
Docker and does not require cgo.

## Installation

```
go get github.com/hirokazumiyaji/container-go
```

## Connection endpoints

Apple Container gives every container a real IP on the `default` vmnet
network, reachable directly from the host. By default this library uses
that IP:

- `Host` returns the container IP, `MappedPort` returns the container
  port itself, and `Endpoint` combines them.
- No host ports are consumed, so parallel tests never conflict over
  ports.

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
wait.ForHTTP("/health")                      // .WithPort, .WithMethod, .WithStatusCodeMatcher
wait.ForExec([]string{"pg_isready"})         // .WithExitCodeMatcher
wait.ForAll(...), wait.ForAny(...)           // composition
```

Every strategy accepts `WithStartupTimeout` (default 60s) and
`WithPollInterval` (default 100ms). Waiting fails fast if the container
stops, and a failed wait rolls the container back with a tail of its
logs attached to the error.

## Cleanup contract

Three layers make sure containers do not outlive your tests:

1. `container.Cleanup(t, ctr)` registers removal via `t.Cleanup`;
   `container.TerminateContainer(ctr)` is the deferred-style variant.
   Both are nil-safe, so call them before checking `Run`'s error.
2. If `Run` fails partway, it removes whatever it created before
   returning.
3. A watchdog reaper (an external `/bin/sh` child) force-deletes every
   registered container when the test process dies in any way,
   SIGKILL and panics included.

Extras:

- `CONTAINERGO_KEEP=1` keeps containers around for debugging.
- `container.Prune(ctx)` removes stopped containers this library
  created in any previous session (they carry the
  `com.github.hirokazumiyaji.container-go` label).

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
| `wait.ForHealthCheck` | No runtime healthcheck exists; use `ForLog`/`ForExec`/`ForHTTP` |
| Building from a Dockerfile | Out of scope (use `container build` yourself) |
| Ryuk reaper container | Replaced by the local watchdog reaper process |
| Random host port mapping | Not needed: connect to the container IP directly |
| Network/volume management APIs | Out of scope for now |
| Docker provider / `DOCKER_HOST` | Apple Container only |

## Development

```
make test         # unit tests (no Apple Container needed)
make vet
make integration  # integration tests against the real CLI; skips if unavailable
```

Design document (Japanese): [docs/design.md](docs/design.md)

## License

MIT
