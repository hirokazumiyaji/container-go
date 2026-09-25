# Benchmark module

This directory is a separate Go module for the testcontainers-go comparison.
The root `container-go` module supports Go 1.23+, but this module declares
`go 1.25.0` because its pinned testcontainers-go dependency requires a newer
Go toolchain. Run it with Go 1.25 or newer:

```sh
cd bench
go test ./...
go test -tags integration -count=1 -timeout 30m ./...
```

The integration benchmark requires a running Docker daemon. The harness
rejects testcontainers Ryuk, image-prefix, and session-ID overrides and fails
if the actual reaper does not match the pinned Ryuk image.
