# Benchmarks

Run→ready measurement infrastructure for the performance issues
(#17, #18, #19, #20, #21, #22). The harness answers three questions:

1. How long does a `Run` take until the container is ready, cold and
   warm?
2. How many CLI child processes does a `Run` spawn (per call shape)?
3. Is the CLI-based approach still competitive against
   testcontainers-go, which talks to the Docker Engine API directly?

## Layout

- `internal/bench/`: shared result schema (`schema_version`, backend,
  library, pinned image digest, source commit and Git tree, dirty state,
  scenario, planned/actual iteration, duration, subprocess count),
  aggregation (median / min / max), JSON output, and the backend harness
  (probe, image ensure / pull / remove). Zero-dependency, part of the root
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
| `run/warm-nginx`    | Same as warm with the pinned nginx image |
| `run/no-wait`       | Run without a wait strategy |
| `run/forlog`        | Run with `wait.ForLog` |
| `run/forexec`       | Run with `wait.ForExec` |
| `run/parallel-8`    | Wall-clock until 8 containers started in parallel are all ready (durations are not summed) |
| `run/multi-5`       | Five sequential containers in one process |
| `tc/session-init`   | testcontainers-go: first container including session initialization (Ryuk sidecar; `cache_state` is `cold` or `warm`) |
| `tc/single`         | testcontainers-go: steady-state single container |
| `tc/multi-5`        | testcontainers-go: five sequential containers, session init paid once |

Each ordinary scenario runs 5 iterations. `tc/session-init` is recorded
once because it represents the one-time testcontainers session setup;
its `cache_state` is `cold` when the pinned Ryuk image was absent at the
start of the measurement and `warm` when it was already cached. The
remaining testcontainers scenarios use the same 5-iteration policy.
Summaries report the median and the minimum. Every result doc embeds
environment information (OS, arch, host, CPUs, Go version, source
commit, `env.tree`, and `env.dirty`) and each result records its pinned
image digest plus the planned iteration count. Testcontainers results
also record the pinned `ryuk_image` and `ryuk_image_digest`.

## Result schema and comparison

Every result entry has the following reproducibility fields:

- `image` is an immutable `@sha256:` reference, not a mutable tag.
- `image_digest` is the digest embedded in that reference and is checked
  against the backend/library/scenario policy.
- `commit` is the source revision used to produce the result; it matches
  `env.commit`. `env.tree` is the Git tree object and `env.dirty` must be
  `false`.
- `iterations` is the planned count for the scenario, while `iteration`
  is the 1-based result number. Thus `tc/session-init` has
  `iterations: 1`, and all other scenarios have `iterations: 5`.
- testcontainers-go entries also carry the immutable `ryuk_image` and
  `ryuk_image_digest`. `tc/session-init` additionally carries
  `cache_state` (`cold` or `warm`), making image-cache effects explicit.
- `env.clis` uses `docker.client` and `docker.server` independently. It
  uses `apple.client` and, when the installed Apple CLI exposes it,
  `apple.service`.

Scenario policies are keyed by backend and library: all `run/*` policies
belong to `container-go` on both `docker` and `apple`, while all `tc/*`
policies belong to `testcontainers-go` on `docker`. The name-only policy
API remains for older consumers, but recorded results are validated with
the full backend/library/scenario key.

The document has `schema_version: 2`. `bench.ValidateDoc` and
`bench.ValidateScenarioSet` enforce the complete identity-specific
scenario set and all planned iterations without a container backend.
`bench.ParseDoc` remains permissive for older result files, but strict
validation rejects missing or mutable metadata. `bench.CompareDocs`
validates both documents first and refuses a comparison when image
reference/digest, backend/library/scenario set, cache state, iteration
policy, or stable environment metadata changes. A clean source commit
and tree may differ: that is how before/after comparisons are made.

## Running

```sh
make bench-integration
```

This runs the root counting and pull-singleflight scenarios and the
bench module (testcontainers-go comparison):

```sh
go test -tags integration -count=1 -timeout 30m -run 'TestIntegrationBench|TestIntegrationPullSingleflight' ./...
cd bench && go test -tags integration -count=1 -timeout 30m ./...
```

Backends that are not available are skipped, same as the other
integration tests:

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

- the pinned workload images and their digests:
  `public.ecr.aws/docker/library/redis@sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499`
  (`sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499`) and
  `public.ecr.aws/docker/library/nginx@sha256:1ed1b0e1d7652937d6cbdaf4018c7b6fc009a7dd6c3047351e2eddda745de43f`
  (`sha256:1ed1b0e1d7652937d6cbdaf4018c7b6fc009a7dd6c3047351e2eddda745de43f`)
- the pinned testcontainers Ryuk image and digest:
  `testcontainers/ryuk@sha256:7c1a8a9a47c780ed0f983770a662f80deb115d95cce3e2daa3d12115b8cd28f0`
  (`sha256:7c1a8a9a47c780ed0f983770a662f80deb115d95cce3e2daa3d12115b8cd28f0`).
  The harness verifies the digest behind testcontainers-go's
  `testcontainers/ryuk:0.14.0` request before recording a result.
- the `cache_state` (`cold` or `warm`) for `tc/session-init`
- the readiness probe (`wait.ForListeningPort`) and parallelism (8)
- the `iterations` value for every scenario
- stable environment metadata, including separate Docker client/server
  versions and the Apple client/service versions when available

The harness records the checked-out `HEAD` in `env.commit`, its Git
`env.tree`, and `env.dirty=false`. A dirty or unverified source tree is
rejected. `CONTAINERGO_BENCH_COMMIT` is accepted only as a full Git
object ID and must match `HEAD`; an exported tree without Git metadata
cannot produce a strict result document. Set
`CONTAINERGO_BENCH_RYUK_CACHE=auto|warm|cold` to select the local Ryuk
cache state (the default is `auto`).

## Baseline (2026-08-29, commit `11b7b8a6fb88d65806141fd6d19befe3772c0b6a`, after #18: pull singleflight + `--pull=never`)

macOS 26, Apple Silicon (10 CPUs), Go 1.27.0, Docker daemon 29.7.2.
Apple backend numbers pending a machine with the system service
running; the scenarios skip cleanly without it. The historical values
predate `schema_version: 2` and digest/source-tree validation and are
retained for reference only. New comparisons must use clean source
metadata, pinned workload and Ryuk digests, separate backend versions,
and explicit `cache_state` for `tc/session-init`.
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
- testcontainers-go's `tc/session-init` depends on whether the pinned
  Ryuk sidecar image is cached: 14.7s on first-ever use, ~0.5s warm.
  New result files encode this distinction in `cache_state` rather than
  treating the two values as interchangeable.

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
