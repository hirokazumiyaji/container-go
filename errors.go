package container

import (
	"errors"
	"fmt"
	"strings"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// CLIError is a non-zero exit from the backend CLI. It aliases
// internal/cli.CLIError so callers can use errors.As without importing
// an internal package.
type CLIError = cli.CLIError

// ErrSystemNotRunning reports that the Apple Container system service is
// not running. Start it with `container system start`.
var ErrSystemNotRunning = cli.ErrSystemNotRunning

// ErrPortNotExposed reports a port that was not declared via
// WithExposedPorts.
var ErrPortNotExposed = errors.New("port not declared via WithExposedPorts")

// ErrImageNotFound reports that an image is not in the backend's local
// store. Run returns it when the pull policy is PullNever and the image
// is absent.
var ErrImageNotFound = errors.New("image not found in local store")

// ErrContainerNotFound reports that the container does not exist.
// Inspect, State, Exec, and Logs wrap it with %w so callers can use
// errors.Is instead of matching CLI stderr text.
var ErrContainerNotFound = errors.New("container not found")

// ErrGenerationReplaced reports that Terminate refused to delete because
// the live container's creation label no longer matches this handle.
var ErrGenerationReplaced = errors.New("container was recreated; refusing to delete replaced container")

// cliCommandParts returns the backend command and its arguments from a
// CLI error. Backend matchers use this to avoid treating an unrelated
// "not found" (for example, a missing volume plugin) as a missing
// container.
func cliCommandParts(err error) (command string, args []string, ok bool) {
	var cliErr *cli.CLIError
	if !errors.As(err, &cliErr) || len(cliErr.Args) == 0 {
		return "", nil, false
	}
	return strings.ToLower(cliErr.Args[0]), cliErr.Args[1:], true
}

func cliBinaryMatches(got, want string) bool {
	got = strings.TrimSpace(strings.ToLower(got))
	if got == "" {
		return want == "container"
	}
	if i := strings.LastIndexAny(got, `/\\`); i >= 0 {
		got = got[i+1:]
	}
	return got == strings.ToLower(want)
}

func cliErrorBelongsTo(err error, backend string) bool {
	var cliErr *cli.CLIError
	return errors.As(err, &cliErr) && cliBinaryMatches(cliErr.Binary, backend)
}

func cliCommandTarget(command string, args []string) string {
	switch command {
	case "run":
		for i, arg := range args {
			if arg == "--name" && i+1 < len(args) {
				return strings.Trim(strings.TrimSpace(args[i+1]), `"'`)
			}
			if strings.HasPrefix(arg, "--name=") {
				return strings.Trim(strings.TrimSpace(strings.TrimPrefix(arg, "--name=")), `"'`)
			}
		}
	case "rm", "delete":
		for i := len(args) - 1; i >= 0; i-- {
			if strings.HasPrefix(args[i], "-") {
				continue
			}
			return strings.Trim(strings.TrimSpace(args[i]), `"'`)
		}
	case "stop":
		for i := 0; i < len(args); i++ {
			arg := args[i]
			if !strings.HasPrefix(arg, "-") {
				return strings.Trim(strings.TrimSpace(arg), `"'`)
			}
			option := arg
			if equal := strings.IndexByte(option, '='); equal >= 0 {
				option = option[:equal]
			}
			if cliOptionTakesValue(option) && !strings.Contains(arg, "=") {
				i++
			}
		}
	case "inspect", "exec", "logs":
		for i := 0; i < len(args); i++ {
			arg := args[i]
			if !strings.HasPrefix(arg, "-") {
				return strings.Trim(strings.TrimSpace(arg), `"'`)
			}

			// Options that take a separate value must consume that value
			// before the target scan. In particular, Apple logs uses
			// `-n 1000 <id>`; returning "1000" would make a target-qualified
			// not-found error look like a backend/container identity error.
			option := arg
			if equal := strings.IndexByte(option, '='); equal >= 0 {
				option = option[:equal]
			}
			if cliOptionTakesValue(option) && !strings.Contains(arg, "=") {
				if i+1 >= len(args) {
					return ""
				}
				i++
			}
		}
	}
	return ""
}

func cliOptionTakesValue(option string) bool {
	switch option {
	case "--env-file", "--user", "--workdir", "--time", "--tail", "--since", "--until",
		"--platform", "--format", "--size", "--type", "--filter", "-n":
		return true
	default:
		return false
	}
}

func cliErrorLines(err error) ([]string, bool) {
	var cliErr *cli.CLIError
	if !errors.As(err, &cliErr) {
		return nil, false
	}
	var lines []string
	for _, line := range strings.Split(cliErr.Stderr, "\n") {
		line = normalizeCLIErrorLine(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines, true
}

func normalizeCLIErrorLine(line string) string {
	line = strings.ToLower(strings.TrimSpace(line))
	for {
		changed := false
		for _, prefix := range []string{"docker: ", "container: ", "error response from daemon: ", "error: "} {
			if strings.HasPrefix(line, prefix) {
				line = strings.TrimSpace(strings.TrimPrefix(line, prefix))
				changed = true
				break
			}
		}
		if !changed {
			return line
		}
	}
}

func hasCLIErrorLine(err error, match func(string) bool) bool {
	lines, ok := cliErrorLines(err)
	if !ok {
		return false
	}
	for _, line := range lines {
		if match(line) {
			return true
		}
	}
	return false
}

func cliTargetListMatches(rest, want string) bool {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return false
	}
	found := false
	for _, part := range strings.Split(rest, ",") {
		part = strings.Trim(strings.TrimSpace(part), `"'`)
		if part == "" || strings.ContainsAny(part, " \t\r\n") {
			return false
		}
		if want == "" || strings.EqualFold(part, strings.Trim(want, `"'`)) {
			found = true
		}
	}
	return found
}

// isNotFoundFor applies the selected backend's operation-aware matcher.
func isNotFoundFor(eng engine, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrContainerNotFound) {
		return true
	}
	if eng == nil {
		return isNotFound(err)
	}
	return eng.containerMissing(err)
}

// isNotFound is retained for callers that do not have an engine context.
// It still selects a backend from the CLI error and uses the same
// operation-aware matcher.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrContainerNotFound) {
		return true
	}
	var cliErr *cli.CLIError
	if !errors.As(err, &cliErr) {
		return false
	}
	if cliBinaryMatches(cliErr.Binary, "docker") {
		return (dockerEngine{}).containerMissing(err)
	}
	return (appleEngine{}).containerMissing(err)
}

// wrapNotFoundFor converts a classified CLI not-found failure into
// ErrContainerNotFound for the selected backend.
func wrapNotFoundFor(eng engine, err error) error {
	if eng == nil {
		return wrapNotFound(err)
	}
	if err == nil || !isNotFoundFor(eng, err) || errors.Is(err, ErrContainerNotFound) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrContainerNotFound, err)
}

// wrapNotFound converts a classified CLI not-found failure into
// ErrContainerNotFound so errors.Is works from the root package.
func wrapNotFound(err error) error {
	if err == nil || !isNotFound(err) || errors.Is(err, ErrContainerNotFound) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrContainerNotFound, err)
}
