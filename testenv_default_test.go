//go:build !integration

package container

// Unit tests use the OS default backend and must not inherit a developer's
// shell selection. Integration builds define this as true and preserve the
// validated selection for the integration helpers.
const integrationTestBuild = false
