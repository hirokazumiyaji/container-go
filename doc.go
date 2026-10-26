// Package container provides a testcontainers-style API for running
// throwaway containers from Go tests.
//
// It shells out to a backend CLI and depends only on the Go standard
// library. Two backends are supported: Apple Container
// (github.com/apple/container) on macOS 26+ Apple Silicon (`container
// system start`), and Docker on Linux, Windows, and macOS (`docker`,
// selected with CONTAINERGO_BACKEND=docker). On Windows, operations that
// need a secret environment file fail with ErrEnvFileUnsupported because
// the library does not treat chmod bits as a per-user ACL, and the external
// watchdog reaper is unavailable. A bind mount against a remote Docker
// daemon is rejected with ErrUnsupportedCapability because the daemon
// resolves the source path on its own host. See README.md for backend
// selection, environment validation, and endpoint differences.
package container
