# Benchmark module

This directory is a separate Go module for the testcontainers-go comparison.
The root `container-go` module supports Go 1.26+, and this module declares
`go 1.26.0` so it can depend on the root module (and its pinned
testcontainers-go dependency). Run it with Go 1.26 or newer:

```sh
cd bench
go test ./...
go test -tags integration -count=1 -timeout 30m ./...
```

The integration benchmark requires a running Docker daemon. Before starting
Testcontainers, the harness validates the effective properties file (including
`TESTCONTAINERS_CONFIG`) with Testcontainers' Java-properties grammar and
rejects Ryuk image, image-prefix, session-ID, and other connection overrides.
It fails if the actual reaper does not match the pinned Ryuk image.

A root-level `go test ./...` does not traverse this nested module, so existing
CI must invoke these commands separately (or use a dedicated nested-module
job). This issue documents the limitation but does not change CI workflows.
