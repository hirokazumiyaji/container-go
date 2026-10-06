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
- Document the repository evidence for Apple Container CLI 1.2.2 and 1.3.0
  and a Docker 29.x-shaped inspect fixture with a local 29.7.2 run; centralize
  stderr matchers on each engine with source comments; add live CLI
  compatibility integration tests; add an Apple inspect fixture for 1.3.0.

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
- Document verified Apple Container (1.2.x–1.3.x) and Docker (29.x;
  copy-out requires 29.7.0+) CLI versions; centralize stderr matchers on
  each engine with source comments; add live CLI compatibility integration
  tests; add Apple inspect fixture for 1.3.0.
- Add `ConfigError` / `ErrInvalidConfig` for backend-incompatible options,
  `ErrEndpointUnreachable` for unusable inspected bindings, and
  `ErrNetworkMismatch` / `ErrNoReachableHost` for runtime network failures.
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

- Remove anonymous Docker volumes from every managed deletion path while
  preserving named volumes (#81).
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

- Reject Docker host, none, internal, and isolated networks before
  published endpoint creation; leave omitted `WithNetwork` to the
  daemon default and resolve Docker's `default` mode from actual
  `NetworkSettings.Networks` and the daemon's authoritative platform
  default; user-defined `bridge`/`nat` names no longer impersonate an
  omitted default. Refresh dynamic inspect data through the immutable
  Docker UID for endpoint, Host, lifecycle, and reuse operations.
- Canonicalize IPv6 bindings and preserve address family in endpoint
  resolution, including fail-closed remote-daemon loopback handling.
- Accept full lowercase Docker UIDs in the reaper while retaining strict
  logical-name validation.
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
- Make `CopyFileFromContainer` fail closed on Apple Container, whose CLI
  has no type-preserving/no-follow copy-out mode, and on Windows Go
  1.23 through 1.25, whose `os.OpenFile` silently ignores the required
  Windows file flags. On supported hosts, Docker retains the host-side
  regular-file and no-follow checks; unsupported host open APIs also
  fail closed.
- Reject backslashes in container paths so Windows Docker path
  normalization cannot reinterpret a literal path component.
- Require Docker client and server 29.7.0 or newer for safe
  `CopyFileFromContainer`; older or unverifiable versions fail closed before
  temporary-directory creation or `docker cp`.

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
