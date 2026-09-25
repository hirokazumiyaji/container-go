package cli

import "os/exec"

// This alias keeps package-local integrations that used the old helper name
// working while all production paths use the lifecycle-owned tree.
func killProcessGroup(cmd *exec.Cmd) error { return terminateProcessTree(cmd) }
