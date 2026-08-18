// Package container provides a testcontainers-style API for running
// throwaway containers on Apple Container (github.com/apple/container)
// from Go tests.
//
// It shells out to the `container` CLI and depends only on the Go
// standard library. It requires macOS 26 or later on Apple Silicon with
// the Apple Container system service running (`container system start`).
package container
