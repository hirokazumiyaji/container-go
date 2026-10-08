// Package container provides a testcontainers-style API for running
// throwaway containers from Go tests.
//
// It shells out to a backend CLI and depends only on the Go standard
// library. Two backends are supported: Apple Container
// (github.com/apple/container) on macOS 26+ Apple Silicon (`container
// system start`), and Docker on Linux, Windows, and macOS (`docker`,
// selected with CONTAINERGO_BACKEND=docker). Current Windows and remote
// Docker bind-source limitations are documented in README.md and are
// tracked by issue #76. See README.md for backend selection and endpoint
// differences.
package container
