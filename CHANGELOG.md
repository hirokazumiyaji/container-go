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