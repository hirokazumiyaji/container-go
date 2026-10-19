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

// applyEngineIdentity records which backend CLI a runner speaks for. Error
// classification compares the CLI's name against the backend's, so a runner
// pointed at a wrapper executable would otherwise stop the backend from
// recognizing errors its own CLI produced.
func applyEngineIdentity(r Runner, eng engine) {
	if er, ok := r.(*cli.ExecRunner); ok {
		er.Identity = eng.binary()
	}
}

// applyEngineBinary points the default runner at the selected engine's
// CLI. An explicitly configured binary wins.
func applyEngineBinary(cfg *config) {
	if er, ok := cfg.runner.(*cli.ExecRunner); ok {
		if cfg.binary != "" {
			er.Binary = cfg.binary
		} else if er.Binary == "" {
			er.Binary = cfg.eng.binary()
		}
	}
	applyEngineIdentity(cfg.runner, cfg.eng)
}

// effectiveRunner returns the runner to use for one backend. A caller-supplied
// ExecRunner that named no executable inherits the engine's CLI, which is what
// Run does for its default runner; leaving it empty would make ExecRunner fall
// back to the historical "container" name on every backend.
func effectiveRunner(configured Runner, binary string, eng engine) Runner {
	if configured == nil {
		configured = &cli.ExecRunner{}
	}
	applyBinaryToRunner(configured, binary)
	if er, ok := configured.(*cli.ExecRunner); ok && er.Binary == "" {
		er.Binary = eng.binary()
	}
	applyEngineIdentity(configured, eng)
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
