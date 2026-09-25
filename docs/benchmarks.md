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
| `tc/session-init`   | testcontainers-go: first container including session initialization (Ryuk sidecar; `cache_state` is `cold` or `warm`; workload cache is recorded separately) |
| `tc/single`         | testcontainers-go: steady-state single container |
| `tc/multi-5`        | testcontainers-go: five sequential containers, session init paid once |

Each ordinary scenario runs 5 iterations. `run/cold` removes the workload
image before the timer and records `workload_cache_state: cold`; all other
container-go scenarios pre-pull their workload image before the timer and
record `workload_cache_state: warm`. `tc/session-init` is recorded once
because it represents the one-time testcontainers session setup; its
`cache_state` is `cold` when the pinned Ryuk image was absent at the start
of the measurement and `warm` when it was already cached. Its workload image
is pre-pulled before the timer, so it records `workload_cache_state: warm`
regardless of the independent Ryuk state. The remaining testcontainers
scenarios use the same 5-iteration policy and pre-pull the workload image.
Summaries report the workload and independent Ryuk cache columns, median,
and minimum. Every result doc embeds
environment information (OS, arch, host, CPUs, Go version, source commit,
`env.tree`, and an explicitly present `env.dirty`) and each result records
its pinned image digest, workload cache state, and planned iteration count.
Testcontainers results also record the pinned `ryuk_image`,
`ryuk_image_digest`, and the actual generated `env.reaper_session_id`.

## Result schema and comparison

Every result entry has the following reproducibility fields:

- `image` is an immutable `@sha256:` reference, not a mutable tag.
- `image_digest` and `expected_image_digest` are the policy digest and the
  immutable digest expected for that reference. `observed_image_digest` and
  `observed_image_id` are read from the backend after the timed region; they
  must agree with the expected digest, and Docker results require a content
  ID. This prevents a mutable local tag or mismatched cached content from
  being recorded as the policy input.
- `workload_cache_state` is the state of the workload image at timer start
  (`cold` or `warm`) and is validated independently for every scenario.
- `commit` is the source revision used to produce the result; it matches
  `env.commit`. `env.tree` is the Git tree object, and `env.dirty` must be
  present explicitly and `false`.
- `iterations` is the planned count for the scenario, while `iteration`
  is the 1-based result number. Thus `tc/session-init` has
  `iterations: 1`, and all other scenarios have `iterations: 5`.
- testcontainers-go entries also carry the immutable `ryuk_image` and
  `ryuk_image_digest`. `tc/session-init` additionally carries
  `cache_state` (`cold` or `warm`) for the independent Ryuk cache, while
  `env.reaper_session_id` records the actual generated reaper session.
- `env.clis` uses the full stable keys `docker.client` and
  `docker.server` independently. Apple results use `apple.client` and, when
  the installed Apple CLI exposes it, `apple.service`; that service value is
  parsed from the `container-apiserver` JSON component, never from a
  client-only response or the complete JSON document.
- Docker results record `docker_endpoint`, `docker_context`,
  `docker_daemon_id`, `docker_daemon_os`, and `docker_daemon_arch`. The
  endpoint is the effective context endpoint, and a conflicting
  `DOCKER_HOST`/`DOCKER_CONTEXT` selection fails before measurement. These
  fields are compared along with the other environment provenance.

Scenario policies are keyed by backend and library: all `run/*` policies
belong to `container-go` on both `docker` and `apple`, while all `tc/*`
policies belong to `testcontainers-go` on `docker`. The name-only policy
API remains for older consumers, but recorded results are validated with
the full backend/library/scenario key.

The document has `schema_version: 4`. `bench.ValidateDoc` and
`bench.ValidateScenarioSet` enforce the complete identity-specific
scenario set and all planned iterations without a container backend.
`bench.ParseDoc` remains permissive for older result files, but strict
validation rejects missing or mutable metadata, missing `env.dirty`, and
unrecognized CLI metadata keys. `bench.CompareDocs` validates both
documents first and refuses a comparison when image reference/digest,
workload or Ryuk cache state, backend/library/scenario set, iteration
policy, expected/observed image identity, or stable environment metadata
(including Docker endpoint/daemon provenance) changes. A clean source
commit and tree may differ: that is how before/after comparisons are made.
The actual testcontainers session ID is diagnostic and is intentionally not
part of environment equality. The complete baseline provenance record is
`testdata/benchmark-baseline.json`; it contains the environment and every
backend/library/scenario image and cache input, not just table row shape.
Testcontainers properties are parsed with its Java-properties grammar before
any identity setting is accepted.

The image provenance fields are deliberately split: `image_digest` is the
policy digest, `expected_image_digest` records that same expected value, and
`observed_image_digest`/`observed_image_id` are captured from the backend
after the measurement. This prevents a mutable tag or a mismatched local
content from being mistaken for the pinned workload. Docker records the
selected endpoint, context, daemon ID, daemon OS, and daemon architecture
alongside separate `docker.client` and `docker.server` versions. The
provenance members are `docker_endpoint`, `docker_context`,
`docker_daemon_id`, `docker_daemon_os`, and `docker_daemon_arch`. Apple
records `apple.client` and, when the running CLI exposes the apiserver,
`apple.service`; a complete all-backend baseline requires the service
entry, while a backend-specific result may omit it only when the installed
CLI does not expose the service-version command. The baseline provenance
fixture includes both so a later comparison cannot silently use a different
service. The generated
baseline's `env` and `scenarios` arrays are machine-checked against this
policy, not merely used as a human-readable table.

## Running

```sh
make bench-integration
```

This runs the root counting and pull-singleflight scenarios and the
bench module (testcontainers-go comparison). The root module supports Go
1.23+, but the nested `bench/` module has its own `go 1.25.0` directive for
its testcontainers dependency; use Go 1.25+ for the second command.

```sh
go test -tags integration -count=1 -timeout 30m -run 'TestIntegrationBench|TestIntegrationPullSingleflight' ./...
cd bench && go test -tags integration -count=1 -timeout 30m ./...
```

The repository's existing CI commands cover the root module, but a root
`go test ./...` does not traverse the nested `bench/` module. The nested
module therefore needs a separate test/lint invocation (and its own Go
1.25+ toolchain); this limitation is documented here rather than changing
CI workflows in this issue.

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
  The harness verifies the actual running reaper container's image ID and
  digest behind testcontainers-go's `testcontainers/ryuk:0.14.0` request
  before recording a result. It rejects all fixed reaper/session/image
  overrides before startup rather than recording a different reaper.
- `workload_cache_state` (`cold` or `warm`) for every result and the
  independent `cache_state` (`cold` or `warm`) for `tc/session-init`
- the readiness probe (`wait.ForListeningPort`) and parallelism (8)
- the `iterations` value for every scenario
- stable environment metadata, including separate Docker client/server
  versions and the Apple client/service versions when available

The harness records the checked-out `HEAD` in `env.commit`, its Git
`env.tree`, and `env.dirty=false`. The `dirty` member is required to be
present in every strict JSON document; a dirty or unverified source tree
is rejected. `CONTAINERGO_BENCH_COMMIT` is accepted only as a full Git
object ID and must match `HEAD`; an exported tree without Git metadata
cannot produce a strict result document. Set
`CONTAINERGO_BENCH_RYUK_CACHE=auto|warm|cold` to select the local Ryuk
cache state (the default is `auto`). Before any reaper is started, the
benchmark checks both the default properties file and the effective
`TESTCONTAINERS_CONFIG` path, parsing them with testcontainers-go's
Java-properties parser (including escaped keys, whitespace separators, and
backslash continuations). It rejects behavior-affecting Ryuk, Docker,
timeout, socket, image-prefix, session, and reaper-image settings,
including `TESTCONTAINERS_RYUK_IMAGE` and the
`ryuk.container.image` property, plus `RYUK_PORT`, Ryuk timeout/verbose/
privilege settings, Docker socket/TLS/API/auth settings, image-prefix, and
session overrides. The generated session is checked for an
existing reaper, then the actual reaper image ID and digest are verified
before a result is recorded as `env.reaper_session_id`; a fixed/shared
session or image identity fails closed.

## Baseline (2026-08-29, commit `11b7b8a6fb88d65806141fd6d19befe3772c0b6a`, after #18: pull singleflight + `--pull=never`)

macOS 26, Apple Silicon (10 CPUs), Go 1.27.0, Docker daemon 29.7.2.
Apple backend numbers pending a machine with the system service
running; the scenarios skip cleanly without it. The historical values
predate `schema_version: 4` and digest/source-tree validation and are
retained for reference only. New comparisons must use clean source
metadata, pinned workload and Ryuk digests, separate backend versions,
and explicit workload and Ryuk cache states.
`run/warm-nginx` and `run/multi-5` are listed for schema completeness but
were not recorded in this historical run.

| Backend | Library            | Scenario       | Iterations | Image | Image digest | Workload cache | Ryuk cache | Median | Spawn |
| ------- | ------------------ | -------------- | ---------- | ----- | ------------ | -------------- | ---------- | ------ | ----- |
| docker | container-go | run/cold | 5 | public.ecr.aws/docker/library/redis@sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499 | sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499 | cold | - | 3.5s (pull) | 4 |
| docker | container-go | run/warm | 5 | public.ecr.aws/docker/library/redis@sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499 | sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499 | warm | - | 149ms | 3 |
| docker | container-go | run/warm-nginx | 5 | public.ecr.aws/docker/library/nginx@sha256:1ed1b0e1d7652937d6cbdaf4018c7b6fc009a7dd6c3047351e2eddda745de43f | sha256:1ed1b0e1d7652937d6cbdaf4018c7b6fc009a7dd6c3047351e2eddda745de43f | warm | - | pending | pending |
| docker | container-go | run/no-wait | 5 | public.ecr.aws/docker/library/redis@sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499 | sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499 | warm | - | 145ms | 3 |
| docker | container-go | run/forlog | 5 | public.ecr.aws/docker/library/redis@sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499 | sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499 | warm | - | 165ms | 4 |
| docker | container-go | run/forexec | 5 | public.ecr.aws/docker/library/redis@sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499 | sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499 | warm | - | 188ms | 4 |
| docker | container-go | run/parallel-8 | 5 | public.ecr.aws/docker/library/redis@sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499 | sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499 | warm | - | 420ms | 17 |
| docker | container-go | run/multi-5 | 5 | public.ecr.aws/docker/library/redis@sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499 | sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499 | warm | - | pending | pending |
| apple | container-go | run/cold | 5 | public.ecr.aws/docker/library/redis@sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499 | sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499 | cold | - | pending | pending |
| apple | container-go | run/warm | 5 | public.ecr.aws/docker/library/redis@sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499 | sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499 | warm | - | pending | pending |
| apple | container-go | run/warm-nginx | 5 | public.ecr.aws/docker/library/nginx@sha256:1ed1b0e1d7652937d6cbdaf4018c7b6fc009a7dd6c3047351e2eddda745de43f | sha256:1ed1b0e1d7652937d6cbdaf4018c7b6fc009a7dd6c3047351e2eddda745de43f | warm | - | pending | pending |
| apple | container-go | run/no-wait | 5 | public.ecr.aws/docker/library/redis@sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499 | sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499 | warm | - | pending | pending |
| apple | container-go | run/forlog | 5 | public.ecr.aws/docker/library/redis@sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499 | sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499 | warm | - | pending | pending |
| apple | container-go | run/forexec | 5 | public.ecr.aws/docker/library/redis@sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499 | sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499 | warm | - | pending | pending |
| apple | container-go | run/parallel-8 | 5 | public.ecr.aws/docker/library/redis@sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499 | sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499 | warm | - | pending | pending |
| apple | container-go | run/multi-5 | 5 | public.ecr.aws/docker/library/redis@sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499 | sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499 | warm | - | pending | pending |
| docker | testcontainers-go | tc/session-init | 1 | public.ecr.aws/docker/library/redis@sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499 | sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499 | warm | cold/warm | 0.5s warm / 14.7s cold | 0 |
| docker | testcontainers-go | tc/single | 5 | public.ecr.aws/docker/library/redis@sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499 | sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499 | warm | - | 335ms | 0 |
| docker | testcontainers-go | tc/multi-5 | 5 | public.ecr.aws/docker/library/redis@sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499 | sha256:858f009f9709ce576febc734aa78b8f6d624b82571f9ddb6bda4377c833b3499 | warm | - | 1.69s (5 ctrs) | 0 |

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
