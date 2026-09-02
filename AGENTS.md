# Repository Guidelines

## Project Structure

- The root Go package contains the public container API, lifecycle helpers, and backend abstraction. Backend implementations are in `engine_apple.go` and `engine_docker.go`.
- `wait/` contains readiness strategies; `internal/` contains CLI and inspection support that is not part of the public API.
- Unit tests live beside the code they cover as `*_test.go`. Backend integration tests are at the root and use the `integration` build tag. Runnable examples are in `examples/`; fixtures are in `testdata/`; design notes are in `docs/`.

## Build, Test, and Development Commands

- `mise install` installs the Go and linting tools declared in `mise.toml`.
- `make test` runs the full default test suite (`go test ./...`) without requiring a container backend.
- `make vet` runs `go vet ./...`.
- `make lint` runs `golangci-lint run ./...` (also enforced in CI).
- `make integration` runs tagged integration tests against the available default backend; `make integration-docker` limits them to Docker. These require the relevant CLI and daemon/service.
- `go fmt ./...` formats all packages.

## Coding Style & Naming

Use standard `gofmt` formatting and idiomatic Go names: mixedCaps for identifiers, short package names, and `TestXxx` test functions. Keep APIs and implementations simple, and write comments only to explain intent. Do not add defensive nil handling unless nil is a valid runtime state or API contract; tests should provide the dependencies production code expects.

## Testing Guidelines

Add focused regression tests beside changed code. Unit tests should use the repository’s fake CLI runner and remain backend-independent. Use the `integration` tag only for tests that need a real backend, and ensure they skip cleanly when that backend is unavailable. Run `make test` and `make lint` before opening a PR; run the relevant integration target when backend behavior changes.

## Commits and Pull Requests

Use concise, imperative commit subjects such as `Add ...`, `Extract ...`, or `docs: ...`. Include `[skip ci]` on commits to avoid automatic CI runs and conserve Actions minutes; run CI manually from the Actions tab via the **CI** workflow's **Run workflow** button (`workflow_dispatch`). PRs should explain the behavior change, identify affected backends or platforms, link related issues when applicable, and list the verification commands run.

## Security and Configuration

Never commit credentials or environment files. Configure backend selection with `CONTAINERGO_BACKEND` and use the backend CLI’s own authentication and host settings. Keep integration-only configuration local to the developer environment.
