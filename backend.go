package container

import (
	"fmt"
	"os"
	"runtime"
)

// backendEnv selects the backend explicitly: "apple" or "docker".
// Unset, the OS decides (macOS gets Apple Container, Linux and Windows
// get Docker).
const backendEnv = "CONTAINERGO_BACKEND"

func detectEngine() (engine, error) {
	return detectEngineFor(runtime.GOOS, os.Getenv(backendEnv))
}

func detectEngineFor(goos, value string) (engine, error) {
	switch value {
	case "":
		if goos == "darwin" {
			return appleEngine{}, nil
		}
		return dockerEngine{}, nil
	case "docker":
		return dockerEngine{}, nil
	case "apple":
		if goos != "darwin" {
			return nil, fmt.Errorf("%s=apple: Apple Container only runs on macOS (GOOS=%s)", backendEnv, goos)
		}
		return appleEngine{}, nil
	default:
		return nil, fmt.Errorf("invalid %s=%q: valid values are \"apple\" and \"docker\"", backendEnv, value)
	}
}
