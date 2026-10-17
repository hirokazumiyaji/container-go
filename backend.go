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
	if er, ok := cfg.runner.(*cli.ExecRunner); ok {
		if cfg.binary != "" {
			er.Binary = cfg.binary
		} else if er.Binary == "" {
			er.Binary = cfg.eng.binary()
		}
	}
}

// applyEngineRunnerEnv points the default runner at the selected engine's
// target environment.
func applyEngineRunnerEnv(cfg *config) {
	if de, ok := cfg.eng.(dockerEngine); ok {
		if er, ok := cfg.runner.(*cli.ExecRunner); ok {
			targetEnv := de.targetEnv()
			if len(targetEnv) > 0 {
				cloned := &cli.ExecRunner{
					Binary: er.Binary,
					Env:    cli.MergeEnv(er.Env, targetEnv),
				}
				cfg.runner = cloned
			}
		}
	}
}

func defaultRunnerFor(eng engine) cli.Runner {
	var env []string
	if de, ok := eng.(dockerEngine); ok {
		env = de.targetEnv()
	}
	return &cli.ExecRunner{Binary: eng.binary(), Env: env}
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
