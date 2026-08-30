# Benchmarks

Run→ready measurement infrastructure for the performance issues
(#17, #18, #19, #20, #21, #22). The harness answers three questions:

1. How long does a `Run` take until the container is ready, cold and
   warm?
2. How many CLI child processes does a `Run` spawn (per call shape)?
3. Is the CLI-based approach still competitive against
   testcontainers-go, which talks to the Docker Engine API directly?

## Layout

- `internal/bench/`: shared result schema (backend, library, image,
  scenario, iteration, duration, subprocess count), aggregation
  (median / min / max), JSON output, and the backend harness (probe,
  image ensure / pull / remove). Zero-dependency, part of the root
  module.
- Root package, `bench_integration_test.go` (`integration` tag):
  counting scenarios. Each `Run` shape runs against a real backend
  wrapped in a counting runner; every iteration records duration and
  spawn count.
- `bench/`: separate Go module holding the testcontainers-go
  comparison. The dependency on testcontainers-go lives only here so
  the library keeps its zero-dependency constraint. `result.go`
  re-exports the shared schema; `scenario_test.go` (`integration` tag)
  runs the wall-clock scenarios.

## Scenarios

| Scenario            | What it measures |
| ------------------- | ---------------- |
| `run/cold`          | Image removed first; Run includes the pull, up to listening-port readiness |
| `run/warm`          | Image present; Run to listening-port readiness |
| `run/warm-nginx`    | Same as warm with `nginx:alpine` |
| `run/no-wait`       | Run without a wait strategy |
| `run/forlog`        | Run with `wait.ForLog` |
| `run/forexec`       | Run with `wait.ForExec` |
| `run/parallel-8`    | Wall-clock until 8 containers started in parallel are all ready (durations are not summed) |
| `run/multi-5`       | Five sequential containers in one process |
| `tc/session-init`   | testcontainers-go: first container including session initialization (Ryuk sidecar) |
| `tc/single`         | testcontainers-go: steady-state single container |
| `tc/multi-5`        | testcontainers-go: five sequential containers, session init paid once |

Each scenario runs 5 iterations; summaries report the median and the
minimum. Every result doc embeds environment information (OS, arch,
CPUs, Go version, backend CLI/daemon version).

## Running

```sh
make bench-integration
```

This runs the counting scenarios (`go test -tags integration -run
TestIntegrationBench ./...`) and the bench module (testcontainers-go
comparison). Backends that are not available are skipped, same as the
other integration tests:

- docker: needs the `docker` CLI and a running daemon
- apple: needs the `container` CLI and `container system start` to
  have been run

Both modules write JSON result docs and print a summary table:

- counting scenarios: `bench/results/counting-<backend>-<timestamp>.json`
- bench module: `bench/results/<name>-<timestamp>.json`

`bench/results/` is gitignored.

## Updating recorded results

1. Run `make bench-integration` on an otherwise idle machine.
2. Note the environment (recorded in each result doc) and keep the
   variables below fixed when comparing runs.
3. Replace the baseline table below and note the date and the commit.

Variables to keep fixed across comparison runs:

- the images (`redis:7-alpine`, `nginx:alpine`) and their digests
- the readiness probe (`wait.ForListeningPort`)
- the iteration count (5) and the parallelism (8)
- backend CLI and daemon versions

## Baseline (2026-08-29, after #18: pull singleflight + `--pull=never`)

macOS 26, Apple Silicon (10 CPUs), Go 1.27.0, Docker daemon 29.7.2.
Apple backend numbers pending a machine with the system service
running; the scenarios skip cleanly without it.

| Backend | Library            | Scenario       | Median | Spawn |
| ------- | ------------------ | -------------- | ------ | ----- |
| docker  | container-go       | run/cold       | 3.5s (pull) | 4 |
| docker  | container-go       | run/warm       | 149ms  | 3 |
| docker  | container-go       | run/forlog     | 165ms  | 4 |
| docker  | container-go       | run/forexec    | 188ms  | 4 |
| docker  | container-go       | run/no-wait    | 145ms  | 3 |
| docker  | container-go       | run/parallel-8 | 420ms  | 17 |
| docker  | testcontainers-go  | tc/session-init| 0.5s warm / 14.7s cold | 0 |
| docker  | testcontainers-go  | tc/single      | 335ms  | 0 |
| docker  | testcontainers-go  | tc/multi-5     | 1.69s (5 ctrs) | 0 |

Changes observed when the pull singleflight landed (#18), against the
pre-#18 numbers from PR #23:

- cold improved from ~4.7-6.1s to ~3.5s and lost its variance tail
  (9.4s worst observed before): the pull is now an explicit
  `docker pull`, and `docker run --pull=never` no longer performs its
  own registry round-trip.
- warm shapes pay exactly one extra spawn for the image existence
  check (2 → 3 for plain Run); in exchange concurrent Runs of a
  missing image pull once instead of racing.
- testcontainers-go's `tc/session-init` depends on whether the Ryuk
  sidecar image is cached: 14.7s on first-ever use, ~0.5s warm.

Reading: warm single-container latency remains close to
testcontainers-go, container-go still pays no per-process session
initialization, and parallel starts stay port-contention-free. The
remaining per-call spawn cost is what #19 and #20 attack; whether the
Docker backend needs a direct Engine API client (#22) is judged from
these numbers.

## #22 decision (2026-08-30): defer Engine API client

Spike on the same machine (Docker Desktop unix socket
`/var/run/docker.sock`, 30 warm iterations, median):

| Call | CLI median | Engine API median | Speedup |
| ---- | ---------- | ----------------- | ------- |
| version / `/version` | 26.3ms | 3.3ms | ~8× |
| `ps -q` / `/containers/json` | 23.1ms | 1.5ms | ~15× |
| `/_ping` | — | 0.9ms | — |

Per-call API latency clears the “≥5× vs CLI” bar from #22. End-to-end
warm `Run` is already faster than testcontainers-go on this baseline
(149ms vs 335ms for a single ready container), and container-go still
avoids Ryuk session init.

**Decision:** do not land a stdlib Docker Engine API transport yet.
Issue #22’s go/no-go gate was “CLI path cannot reach testcontainers-go
on the #17 harness”; that gate is not met. Prefer finishing spawn
reduction (#19, #20) and shared reuse (#21) first. Revisit the API
client if multi-package CI wall-clock is still CLI-bound after those
land, or if a future baseline shows warm single `Run` falling behind
testcontainers-go again.

If revisited, follow the #22 design note: `CONTAINERGO_DOCKER_MODE=auto|api|cli`,
hot-path only (create/start/inspect/rm/ps/exec/logs), pull/auth stay on
the CLI, and transport choice is fixed at engine init (no per-operation
fallback).
