// Package container provides a testcontainers-style API for running
// throwaway containers from Go tests.
//
// It shells out to a backend CLI and depends only on the Go standard
// library. Two backends are supported: Apple Container
// (github.com/apple/container) on macOS 26+ Apple Silicon (`container
// system start`), and Docker on Linux, Windows, and macOS (`docker`,
// selected with CONTAINERGO_BACKEND=docker). On Windows, secret environment
// files for WithEnv and WithExecEnv are stored under a current-logon-only
// DACL. The watchdog reaper is a self-executed Go child on every supported
// platform: it reclaims Docker containers by immutable ID, including the ID
// an inspect reports for a name-addressed create, and decodes inspect output
// in memory without /bin/sh. On Windows it is held in a Job Object with every
// process it starts, so stopping it tears down that tree; the loop starts no
// backend command before EOF, so this is defense in depth. The job is not
// configured to terminate the reaper when the parent's handle closes, so the
// watchdog outlives the parent to reap. A build that cannot re-execute itself
// (for example -buildmode=c-shared or c-archive, a setuid/setgid Unix binary,
// or build information that does not name this module) has no reaper:
// registration is a no-op rather than falling back to a shell, so
// abnormal-exit cleanup relies on explicit cleanup and Prune. A bind mount
// against a remote Docker daemon is rejected with ErrUnsupportedCapability
// because the daemon resolves the source path on its own host. See README.md
// for backend selection, environment validation, and endpoint differences.
package container
