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
when the relevant CLI or daemon is unavailable.

## Pull requests

- Use concise, imperative commit subjects (`Add ...`, `Fix ...`, `docs: ...`).
- Explain the behavior change, affected backends or platforms, and verification
  commands in the PR description.
- Link related issues when applicable.
- Keep changes focused; match existing naming and formatting (`gofmt`).

## Project layout

- Root package: public container API and backend abstraction.
- `wait/`: readiness strategies (public).
- `internal/`: CLI runner and inspect parsing (not public API).
- Unit tests live beside the code; backend integration tests are at the repo
  root with the `integration` build tag.
