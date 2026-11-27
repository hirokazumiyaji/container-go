//go:build !windows

package container

// envKeyFold returns key unchanged. Environment variable names are
// case-sensitive on this platform.
func envKeyFold(key string) string { return key }
