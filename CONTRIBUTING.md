# Contributing

Thank you for considering a contribution. This document is a short entry
point; see [AGENTS.md](AGENTS.md) for the full repository guidelines.

## Getting started

```bash
mise install   # Go and golangci-lint
make test      # unit tests (no container backend required)
make lint
```

Run `make integration` or `make integration-docker` when you change backend
behavior. Integration tests use the `integration` build tag and skip cleanly
when the relevant CLI or daemon is unavailable. They pull images from the AWS
public ECR Docker Hub mirror (`public.ecr.aws/docker/library/...`) to avoid
anonymous Docker Hub rate limits. `make integration` excludes pull-heavy bench
and singleflight cases (`make bench-integration` covers those; the
nested benchmark module requires Go 1.25+). Set
`CONTAINERGO_BACKEND=apple` or `docker` to run only that backend's tests.
The root test binary records that value in `TestMain` and then unsets it,
so the other backend's suites skip correctly; guards read it through
`integrationtest.SelectedBackend` rather than `os.Getenv`, which would
always see the empty value.
Authenticated `docker login` (or the Apple CLI equivalent) still helps if you
pull other Hub images locally.

## Pull requests

- Use concise, imperative commit subjects (`Add ...`, `Fix ...`, `docs: ...`).
- Explain the behavior change, affected backends or platforms, and verification
  commands in the PR description.
- Link related issues when applicable.
- Keep changes focused; match existing naming and formatting (`gofmt`).

## Releasing

The version is not stored anywhere authoritative. It is derived from the newest
dated `CHANGELOG` release, and every other place that names a version must
agree with it. `internal/releasecheck` asserts that, and it runs as part of
`go test ./...`, so drift is caught on the change that introduces it rather
than at tag time.

To cut a release:

1. Move the accumulated `## [Unreleased]` entries under a new
   `## [X.Y.Z] - YYYY-MM-DD` heading, adding an `### Fixed` section if the
   release has fixes. Keep a fresh empty `## [Unreleased]` above it.
2. Update every version reference to `@vX.Y.Z` in `README.md` and
   `README.ja.md` — both the `go get` install instruction and the shorthand in
   the pinning prose — and the `SECURITY.md` support matrix to cover `X.Y.x`.
   `internal/releasecheck` names each one it checks if you miss any.
3. Run `make release-check` from a clean checkout: build, vet (including the
   `integration`-tagged files), `golangci-lint`, `go test -race`,
   `go mod verify`, a `go mod tidy -diff` no-op check, `govulncheck`, and
   `actionlint`, then the same for the separate `bench` module. This is the
   pre-tag gate — the same target the workflow runs — so a green local run
   means the code half will pass. It needs `golangci-lint` on `PATH`
   (`mise install`).

   This covers everything checkable without a container backend. The
   `integration`-tagged suites are not part of it — run `make integration` and
   `make bench-integration` locally before tagging if you touched backend
   behavior. Their compilation *is* covered, by the tagged vet.
4. Tag and push: `git tag -a vX.Y.Z -m "vX.Y.Z" && git push origin vX.Y.Z`.
   A push of a `v*` tag runs only after GitHub has accepted the tag, so the
   workflow run on the tag cannot stop a bad tag from being published: it
   re-runs the gate and additionally fails if the tag is not the highest
   version in the `CHANGELOG`, as a post-push safety net. If it fails, delete
   the tag and retag. The tag-vs-`CHANGELOG` comparison is not in
   `make release-check` because it needs the pushed tag's name, which only
   the workflow has; `internal/releasecheck` is the local half of it, and
   it already ran as part of step 3.

The tag is what downstream pins, so it is annotated rather than lightweight.

"Latest" means the highest version, not the topmost section. A backport patch
belongs at the top of the `CHANGELOG`, but the tag check will not accept it
while a higher version exists — so a patch for an older series cannot be
released once a newer one is out. Given `SECURITY.md` supports a single series,
that is the intended behavior; cut the newer release first if both are needed.

## Project layout

- Root package: public container API and backend abstraction.
- `wait/`: readiness strategies (public).
- `internal/`: CLI runner and inspect parsing (not public API).
- Unit tests live beside the code; backend integration tests are at the repo
  root with the `integration` build tag.
