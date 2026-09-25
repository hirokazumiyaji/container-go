//go:build !windows

package cli

func badExecutableStartError(error) bool { return false }
