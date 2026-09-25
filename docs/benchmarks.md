# Benchmarks

Run→ready measurement infrastructure for the performance issues
(#17, #18, #19, #20, #21, #22). The harness answers three questions:

1. How long does a `Run` take until the container is ready, cold and
   warm?
2. How many CLI child processes does a `Run` spawn (per call shape)?
3. Is the CLI-based approach still competitive against
   testcontainers-go, which talks to the Docker Engine API directly?

## Layout

- `internal/bench/`: shared result schema (backend, library, pinned
  image digest, source commit, scenario, planned/actual iteration,
  duration, subprocess count), aggregation (median / min / max), JSON
  output, and the backend harness (probe, image ensure / pull / remove).
  Zero-dependency, part of the root module.
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
| `run/warm-nginx`    | Same as warm with the pinned nginx image |
| `run/no-wait`       | Run without a wait strategy |
| `run/forlog`        | Run with `wait.ForLog` |
| `run/forexec`       | Run with `wait.ForExec` |
| `run/parallel-8`    | Wall-clock until 8 containers started in parallel are all ready (durations are not summed) |
| `run/multi-5`       | Five sequential containers in one process |
| `tc/session-init`   | testcontainers-go: first container including session initialization (Ryuk sidecar) |
| `tc/single`         | testcontainers-go: steady-state single container |
| `tc/multi-5`        | testcontainers-go: five sequential containers, session init paid once |

Each ordinary scenario runs 5 iterations. `tc/session-init` is recorded
once because it represents the one-time testcontainers session setup;
the remaining testcontainers scenarios use the same 5-iteration policy.
Summaries report the median and the minimum. Every result doc embeds
environment information (OS, arch, host, CPUs, Go version, source
commit, backend CLI/daemon version) and each result records its pinned
image digest plus the planned iteration count.

## Result schema and comparison

Every result entry has the following reproducibility fields:

- `image` is an immutable `@sha256:` reference, not a mutable tag.
- `image_digest` is the digest embedded in that reference and is checked
  against the scenario policy.
- `commit` is the source revision used to produce the result; it matches
  `env.commit`.
- `iterations` is the planned count for the scenario, while `iteration`
  is the 1-based result number. Thus `tc/session-init` has
  `iterations: 1`, and all other scenarios have `iterations: 5`.

`bench.ValidateDoc` and `bench.ValidateScenarioSet` enforce these rules
without a container backend. `bench.ParseDoc` remains permissive for
older result files, while `bench.CompareDocs` refuses a comparison when
the commit, image reference/digest, scenario set, or iteration policy
changes. A mutable tag therefore cannot silently produce a comparable
baseline.

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

- the pinned images and their digests:
  `public.ecr.aws/docker/library/redis@sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499`
  (`sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499`) and
  `public.ecr.aws/docker/library/nginx@sha256:1ed1b0e1d7652937d6cbdaf4018c7b6fc009a7dd6c3047351e2eddda745de43f`
  (`sha256:1ed1b0e1d7652937d6cbdaf4018c7b6fc009a7dd6c3047351e2eddda745de43f`)
- the source commit and the `iterations` value for every scenario
- the readiness probe (`wait.ForListeningPort`)
- the parallelism (8)
- backend CLI and daemon versions

The harness records the checked-out `HEAD` in `env.commit`. For an
exported source tree without Git metadata, set
`CONTAINERGO_BENCH_COMMIT` to the revision being measured before running
the benchmark.

## Baseline (2026-08-29, commit `11b7b8a6fb88d65806141fd6d19befe3772c0b6a`, after #18: pull singleflight + `--pull=never`)

macOS 26, Apple Silicon (10 CPUs), Go 1.27.0, Docker daemon 29.7.2.
Apple backend numbers pending a machine with the system service
running; the scenarios skip cleanly without it. The historical values
predate digest pinning and are retained for reference; new comparisons
must use the pinned references and record `iterations` explicitly.
`run/warm-nginx` and `run/multi-5` are listed for schema completeness but
were not recorded in this historical run.

| Backend | Library            | Scenario       | Iterations | Median | Spawn |
| ------- | ------------------ | -------------- | ---------- | ------ | ----- |
| docker  | container-go       | run/cold       | 5          | 3.5s (pull) | 4 |
| docker  | container-go       | run/warm       | 5          | 149ms  | 3 |
| docker  | container-go       | run/warm-nginx | 5          | pending | pending |
| docker  | container-go       | run/no-wait    | 5          | 145ms  | 3 |
| docker  | container-go       | run/forlog     | 5          | 165ms  | 4 |
| docker  | container-go       | run/forexec    | 5          | 188ms  | 4 |
| docker  | container-go       | run/parallel-8 | 5          | 420ms  | 17 |
| docker  | container-go       | run/multi-5    | 5          | pending | pending |
| docker  | testcontainers-go  | tc/session-init| 1          | 0.5s warm / 14.7s cold | 0 |
| docker  | testcontainers-go  | tc/single      | 5          | 335ms  | 0 |
| docker  | testcontainers-go  | tc/multi-5     | 5          | 1.69s (5 ctrs) | 0 |

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
