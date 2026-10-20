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

// applyEngineIdentity returns r with the engine's backend CLI recorded, so error
// classification attributes this backend's errors to it even when the runner
// executes a wrapper. A caller-owned ExecRunner is copied rather than mutated:
// the library supports concurrent callers sharing one runner, and the stamp
// belongs to this operation rather than to the runner.
func applyEngineIdentity(r Runner, eng engine) Runner {
	if er, ok := r.(*cli.ExecRunner); ok {
		stamped := *er
		stamped.Identity = eng.binary()
		return &stamped
	}
	if setter, ok := r.(interface{ SetIdentity(string) }); ok {
		setter.SetIdentity(eng.binary())
	}
	return r
}

// applyEngineBinary points the default runner at the selected engine's
// CLI. An explicitly configured binary wins.
func applyEngineBinary(cfg *config) {
	if er, ok := cfg.runner.(*cli.ExecRunner); ok {
		stamped := *er
		if cfg.binary != "" {
			stamped.Binary = cfg.binary
		} else if stamped.Binary == "" {
			stamped.Binary = cfg.eng.binary()
		}
		stamped.Identity = cfg.eng.binary()
		cfg.runner = &stamped
		return
	}
	cfg.runner = applyEngineIdentity(cfg.runner, cfg.eng)
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
	if er, ok := configured.(*cli.ExecRunner); ok {
		stamped := *er
		if stamped.Binary == "" {
			stamped.Binary = eng.binary()
		}
		stamped.Identity = eng.binary()
		return &stamped
	}
	return applyEngineIdentity(configured, eng)
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
