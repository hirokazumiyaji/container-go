package container

import (
	"fmt"
	"os"
	"runtime"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// backendEnv selects the backend explicitly: "apple" or "docker".
// Unset, the OS decides (macOS gets Apple Container, Linux and Windows
// get Docker).
const backendEnv = "CONTAINERGO_BACKEND"

func detectEngine() (engine, error) {
	return detectEngineFor(runtime.GOOS, os.Getenv(backendEnv))
}

// withEngineBinary returns eng carrying the caller-configured CLI, so every
// place that names the backend CLI -- runner construction, reaper
// registration, the probe, and error classification -- agrees on one
// executable. Classification keys on that name, so leaving it at the engine
// default while running a different executable would stop the backend's own
// errors from being attributed to it.
func withEngineBinary(eng engine, binary string) engine {
	if binary == "" {
		return eng
	}
	switch e := eng.(type) {
	case dockerEngine:
		e.binaryOverride = binary
		return e
	case appleEngine:
		e.binaryOverride = binary
		return e
	}
	return eng
}

// applyEngineBinary points the default runner at the selected engine's
// CLI. An explicitly configured binary wins.
func applyEngineBinary(cfg *config) {
	cfg.eng = withEngineBinary(cfg.eng, cfg.binary)
	if er, ok := cfg.runner.(*cli.ExecRunner); ok && er.Binary == "" {
		er.Binary = cfg.eng.binary()
	}
}

// effectiveRunner returns the runner to use for one backend. A caller-supplied
// ExecRunner that named no executable inherits the engine's CLI, which is what
// Run does for its default runner; leaving it empty would make ExecRunner fall
// back to the historical "container" name on every backend.
func effectiveRunner(configured Runner, binary string, eng engine) Runner {
	if configured == nil {
		return &cli.ExecRunner{Binary: eng.binary()}
	}
	applyBinaryToRunner(configured, binary)
	if er, ok := configured.(*cli.ExecRunner); ok && er.Binary == "" {
		er.Binary = eng.binary()
	}
	return configured
}

func detectEngineFor(goos, value string) (engine, error) {
	if err := validateBackendEnv(value); err != nil {
		return nil, err
	}
	switch value {
	case "":
		if goos == "darwin" {
			return appleEngine{}, nil
		}
		return dockerEngine{}, nil
	case "docker":
		return dockerEngine{}, nil
	default: // apple
		if goos != "darwin" {
			return nil, invalidOption("backend", "Apple Container is available only on macOS")
		}
		return appleEngine{}, nil
	}
}

func validateBackendEnv(value string) error {
	switch value {
	case "", "apple", "docker":
		return nil
	default:
		return fmt.Errorf("invalid %s=%q: valid values are \"apple\" and \"docker\"", backendEnv, value)
	}
}
