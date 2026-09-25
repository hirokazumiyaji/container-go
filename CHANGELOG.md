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

### Changed

- Synchronize the English and Japanese README/design documents with the
  current pull policies, `Logs`/`FollowLogs` split, operation-specific
  timeouts, reaper limits, backend selection, and CI matrix. Remove
  nonexistent logger API references and mark the implementation phases
  as historical.
- Add a non-backend compile check for the documented API examples.
- Share Apple/Docker `runArgs` common flags via `config.commonRunArgs` and
  call `allLabels()` once.
- Consolidate image and reuse single-flight state in the generic
  `flightGroup[T]` helper in `flight.go`.
- Deduplicate reuse/cleanup helpers (including `inspectNamed`, prune loops,
  and Docker line splitting), hoist `memoryRE`, and document `Host` vs
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
