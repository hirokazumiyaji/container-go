# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Document verified Apple Container (1.2.x–1.3.x) and Docker (29.x) CLI
  versions; centralize stderr matchers on each engine with source comments;
  add live CLI compatibility integration tests; add Apple inspect fixture
  for 1.3.0.
- `StrictCleanup`, which reports a container-teardown failure as a test
  failure instead of logging it. `Cleanup` still logs, so an unrelated
  backend problem does not turn an unrelated test red.
- `internal/integrationtest`, the shared backend preflight for the tagged
  suites: a required CI run fails when the backend is unavailable or when
  `CONTAINERGO_BACKEND` names the other backend, a `CONTAINERGO_BACKEND`
  typo always fails, and a cross-process helper enforces its deadline while
  blocked and returns the child's partial output on timeout.
- `internal/releasecheck`, the release invariants as a test: the README
  install lines, the `SECURITY.md` support matrix, and the newest dated
  `CHANGELOG` release must name the same version.
- A `release-check` workflow and a `make release-check` target that run the
  full gate — build, vet, test, `go mod verify`, a `go mod tidy` check, and
  the separate `bench` module. `make release-check` and the pull-request
  run are the pre-tag gate. The tag-push run is a post-push safety net: the
  tag already exists, and a failing run means a maintainer must delete it
  and retag.

### Fixed

- Cleanup failures are no longer discarded. A failed-create, reuse
  inspect/copy rollback, or public `Cleanup` that could not remove the
  container now joins the cleanup failure onto the operation error, so
  `errors.Is` still matches the operation error and `errors.As` reaches the
  new exported `CleanupError` to learn that a container was left behind. An
  already-absent container and a name conflict remain idempotent successes,
  and a successful inspect that lists no container is treated as proof of
  absence rather than reported as a leak (#86).
- The Docker integration CI job can no longer report success with zero tests
  run: a missing backend, a misconfigured backend, or a `-run` pattern that
  selects nothing all fail the job (#106).

### Changed

- Share Apple/Docker `runArgs` common flags via `config.commonRunArgs` and
  call `allLabels()` once.
- Merge `flightGroup` / `reuseFlightGroup` into one generic `flightGroup[T]`
  in `flight.go`.
- Deduplicate reuse/cleanup helpers (`inspectNamed`/`deleteNamed`, prune
  loops, Docker line splitting), hoist `memoryRE`, and document `Host` vs
  `Endpoint` when publish host-IPs differ.
- Route `cp` through engine `copyToArgs`/`copyFromArgs`; include the CLI
  binary name in `CLIError` and neutralize `internal/cli` package docs.

## [0.2.0] - 2026-09-02

First tagged release. Covers the Apple Container backend (v0.1 development)
plus the v0.2 backend and lifecycle work.

### Added

- Apple Container backend: `Run`, functional options, lifecycle (`Stop`,
  `Terminate`), connection endpoints (`Host`, `MappedPort`, `Endpoint`),
  `Exec`, `Logs`, `FollowLogs`, and file copy.
- `wait` package: log, port, HTTP, exec, and composite strategies with
  fail-fast on container exit and rollback with log tail on failure.
- Cleanup: `Cleanup` / `TerminateContainer`, session labels, `Prune`, and a
  watchdog reaper for orphaned containers (not available on Windows).
- Docker backend via CLI wrapper with OS-default selection on Linux and
  Windows; `CONTAINERGO_BACKEND` overrides on any OS.
- Automatic loopback port publishing on Docker for declared exposed ports.
- Image pull aggregation across concurrent `Run` calls; `Pull`, `PullAlways`,
  `PullNever`, and `PullMissing` policies.
- `WithReuse` get-or-create for named containers shared across tests and
  processes, with `WithReuseGroup` and `PruneReuseGroup`.
- Deferred first `inspect` until `Endpoint` or `State` is needed.
- CI: `workflow_dispatch`, `go test -race`, and Docker integration tests on
  `ubuntu-latest`.

### Changed

- English is the primary documentation language (`README.md`, `docs/design.md`).
