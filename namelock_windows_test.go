//go:build windows

package container

// Keep the package-local lock test seam defined on Windows so cross-platform
// tests that exercise common operation paths still compile. The production
// Windows implementation has no name-addressed lock path.
var nameLockStateRootOverride string
