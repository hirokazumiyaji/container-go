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

// applyEngineBinary points the default runner at the selected engine's
// CLI. An explicitly configured binary wins.
func applyEngineBinary(cfg *config) {
	if er, ok := cfg.runner.(*cli.ExecRunner); ok && er.Binary == "" {
		er.Binary = cfg.eng.binary()
	}
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
			return nil, fmt.Errorf("%s=apple: Apple Container only runs on macOS (GOOS=%s)", backendEnv, goos)
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
