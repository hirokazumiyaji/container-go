package cli

import (
	"runtime"
	"strings"
)

// MergeEnv merges base environment variables with overrides, ensuring that
// variables in overrides replace any matching key in base. Duplicate override
// keys keep the last value, so the child environment stays deterministic. An
// override may also be a bare key without "=": that removes the inherited
// entry instead of leaving a stale value behind for the child process.
func MergeEnv(base, overrides []string) []string {
	if len(overrides) == 0 {
		return append([]string(nil), base...)
	}
	// order keeps the first appearance position of each key while value
	// keeps its last override. An entry of "" is a removal marker: the key
	// was named without "=", so the inherited value must be dropped and
	// nothing is emitted for it.
	order := make([]string, 0, len(overrides))
	value := make(map[string]string, len(overrides))
	for _, env := range overrides {
		key, _, ok := strings.Cut(env, "=")
		if !ok {
			key = env
			env = ""
		}
		if key == "" {
			continue
		}
		if runtime.GOOS == "windows" {
			key = strings.ToUpper(key)
		}
		if _, seen := value[key]; !seen {
			order = append(order, key)
		}
		value[key] = env
	}
	if len(order) == 0 {
		return append([]string(nil), base...)
	}
	res := make([]string, 0, len(base)+len(order))
	for _, env := range base {
		key, _, ok := strings.Cut(env, "=")
		if ok {
			lookupKey := key
			if runtime.GOOS == "windows" {
				lookupKey = strings.ToUpper(key)
			}
			if _, overridden := value[lookupKey]; overridden {
				continue
			}
		}
		res = append(res, env)
	}
	for _, key := range order {
		if entry := value[key]; entry != "" {
			res = append(res, entry)
		}
	}
	return res
}
