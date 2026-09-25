# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

This section describes development after the tagged `v0.2.0` release.
The APIs listed under this section are not part of `v0.2.0` unless a
release note explicitly says otherwise. The root checkout requires Go
1.23 or later; the nested `bench/` module requires Go 1.25 or later;
the tagged `v0.2.0` module requires Go 1.27 or later.

### Added

- Add `LogsOptions` and `LogsWithOptions` for requesting a snapshot
  window with `Tail` and `Since`. Docker supports both options; the
  Apple-specific mapping and unsupported `Since` behavior are tracked by
  issue #82 and are not backend-neutral in this checkout.
- Add `wait.ForHTTP` header, basic-auth, TLS, TLS-config, and custom
  HTTP-client setters.
- Export `wait.AllStrategy` and `wait.AnyStrategy` and add
  `WithStartupTimeout` to bound a composite wait.
- Expose the root `CLIError` alias and add `ErrContainerNotFound` and
  `ErrGenerationReplaced`.
- Document verified Apple Container (1.2.x–1.3.x) and Docker (29.x) CLI
  versions; centralize stderr matchers on each engine with source comments;
  add live CLI compatibility integration tests; add an Apple inspect fixture
  for 1.3.0.

### Changed

- Synchronize the English and Japanese README/design documents with the
  current pull policies, `Logs`/`FollowLogs` split, operation-specific
  timeouts, reaper limits, backend selection, and CI matrix. Separate the
  released `v0.2.0` API from current-development APIs, remove nonexistent
  logger API references, and mark implementation phases as historical.
- Add a non-backend documentation test that extracts and compiles the
  actual standalone Go examples in both language versions of the README and
  design documents. The test compares all paired fenced blocks after removing
  comments and quotes the local `replace` path in its temporary module.
- Share Apple/Docker `runArgs` common flags via `config.commonRunArgs` and
  call `allLabels()` once.
- Consolidate image and reuse single-flight state in the generic
  `flightGroup[T]` helper in `flight.go`.
- Deduplicate reuse/cleanup helpers (including `inspectNamed`, prune loops,
  and Docker line splitting), hoist `memoryRE`, and document `Host` vs
  `Endpoint` when publish host-IPs differ.
- Clarify `WithPublishedPort` and `ErrPortNotExposed` documentation for
  Docker auto-publishing and missing host bindings.
- Route `cp` through engine `copyToArgs`/`copyFromArgs`; include the CLI
  binary name in `CLIError` and neutralize `internal/cli` package docs.

### Cross-issue prerequisites

- The current base's reaper registration still accepts only Apple-style
  names. Docker's full immutable ID cannot be registered until issue #73
  is stacked. The current README and design document call out this
  prerequisite; the normal Docker handle/rollback path is separate.
- Apple `LogsWithOptions` needs issue #82 before `Tail` and `Since` can be
  documented as backend-specific capabilities.
- Dynamic endpoint cache refresh is tracked by issue #85; the current
  first-inspect cache can expose stale IP or binding data.
- Reuse ownership and final generation verification are tracked by issues
  #83 and #84; #94 tracks ignored WithFiles/PullAlways side effects on
  reuse attach; #98 tracks the Apple Prune list-to-delete race and
  missing fresh revalidation. This base still has missing-generation,
  post-wait, and prune fail-open paths.
- Docker deletion uses an immutable ID on this base, but other operations
  still address the logical name; #74 tracks the stale-handle fix.
- Docker `Prune` does not select dead containers until issue #113.
- Windows and remote bind-source handling is qualified by issue #76;
  `ForListeningPort`/`ForExposedPort` remain TCP-only until #77.
- `Stop` timeout validation/rounding is pending #89; wait error-chain
  normalization is pending #92; public option validation gaps are tracked
  by #102; reaper staging exposure and mitigation are tracked by #111.
- Successful missing inspect classification is pending #103; liveness
  error-chain flattening is tracked by #104; Apple PullNever capability
  handling is pending #112.

## [0.2.0] - 2026-09-02

This section describes only the tagged `v0.2.0` release. The
`Unreleased` APIs above are not available from that tag. The release
module requires Go 1.27 or later.

First tagged release. Covers the Apple Container backend (v0.1 development)
plus the v0.2 backend and lifecycle work.

### Added

- Apple Container backend: `Run`, functional options, lifecycle (`Stop`,
  `Terminate`), connection endpoints (`Host`, `MappedPort`, `Endpoint`),
  `Exec`, `Logs`, `FollowLogs`, and file copy.
- `wait` package: log, port, HTTP, exec, and composite strategies.
  Connection/HTTP strategies check stopped state while polling, while
  `ForExec` checks stopped state when its wait deadline expires; a failed
  non-reuse wait rolls back and includes a bounded log tail when available.
- Cleanup: `Cleanup` / `TerminateContainer`, session labels, `Prune`, and a
  best-effort watchdog reaper for orphaned containers (not available on
  Windows). Reaper registration and per-entry delete failures are not
  returned by `Run`.
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
