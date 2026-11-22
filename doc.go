// Package container provides a testcontainers-style API for running
// throwaway containers from Go tests.
//
// It shells out to a backend CLI and depends only on the Go standard
// library. Two backends are supported: Apple Container
// (github.com/apple/container) on macOS 26+ Apple Silicon (`container
// system start`), and Docker on Linux, Windows, and macOS (`docker`,
// selected with CONTAINERGO_BACKEND=docker). On Windows, secret environment
// files for WithEnv and WithExecEnv are stored under a current-logon-only
// DACL, and the watchdog reaper runs natively without /bin/sh to reclaim
// Docker containers by immutable ID, including the ID an inspect reports for
// a name-addressed create. The reaper is held in a Job Object with every
// process it starts, so stopping it tears down that tree; the native loop
// starts no backend command before EOF, so this is defense in depth. The job
// is not configured to terminate the reaper when the parent's handle closes,
// so the watchdog outlives the parent to reap. A build that cannot re-execute
// itself has no Windows reaper. A bind mount
// against a remote Docker daemon is rejected with ErrUnsupportedCapability
// because the daemon resolves the source path on its own host. See README.md
// for backend selection, environment validation, and endpoint differences.
package container
