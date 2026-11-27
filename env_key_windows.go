//go:build windows

package container

import "strings"

// envKeyFold normalizes an environment variable key for comparison. Windows
// environment variable names are case-insensitive, matching cli.MergeEnv.
func envKeyFold(key string) string { return strings.ToUpper(key) }
