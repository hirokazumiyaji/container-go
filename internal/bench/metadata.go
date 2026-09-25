package bench

import (
	"errors"
	"os"
	"os/exec"
	"runtime/debug"
	"strings"
)

// CurrentCommit returns the source revision used for a benchmark run.
// The environment override is useful when a benchmark is executed from an
// exported source tree; otherwise the checked-out revision is preferred,
// with build metadata as a fallback for binaries.
func CurrentCommit() (string, error) {
	if commit := strings.TrimSpace(os.Getenv("CONTAINERGO_BENCH_COMMIT")); commit != "" {
		return commit, nil
	}

	if commit := gitCommit(); commit != "" {
		return commit, nil
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" && strings.TrimSpace(setting.Value) != "" {
				return strings.TrimSpace(setting.Value), nil
			}
		}
	}
	return "", errors.New("resolve benchmark commit: no git revision or vcs.revision")
}

func gitCommit() string {
	out, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
