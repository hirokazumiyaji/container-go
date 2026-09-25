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

// CleanupError reports an operation failure together with a failure from
// removing the container the operation created. Both errors remain in
// the returned chain through Err and CleanupErr.
type CleanupError struct {
	Err        error
	CleanupErr error
}

func (e *CleanupError) Error() string {
	return fmt.Sprintf("%v; cleanup failed: %v", e.Err, e.CleanupErr)
}

// Unwrap returns both the operation and cleanup errors.
func (e *CleanupError) Unwrap() []error {
	return []error{e.Err, e.CleanupErr}
}

func withCleanupError(err, cleanupErr error) error {
	if cleanupErr == nil {
		return err
	}
	return &CleanupError{Err: err, CleanupErr: cleanupErr}
}

// primaryOperationError returns only the operation branch of a cleanup
// error. Retry classifiers must not inspect CleanupErr, which may contain a
// different backend command failure.
func primaryOperationError(err error) error {
	var cleanupErr *CleanupError
	if errors.As(err, &cleanupErr) {
		return cleanupErr.Err
	}
	return err
}

// ErrSystemNotRunning reports that the Apple Container system service is
// not running. Start it with `container system start`.
var ErrSystemNotRunning = cli.ErrSystemNotRunning

// ErrPortNotExposed reports that a port was not declared via
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

// collectCLIErrors gathers CLI errors through both single- and multi-error
// wrappers, including backend liveness wrappers that retain the original
// command failure.
func collectCLIErrors(err error, out *[]*cli.CLIError) {
	if err == nil {
		return
	}
	if cliErr, ok := err.(*cli.CLIError); ok {
		*out = append(*out, cliErr)
		return
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			collectCLIErrors(child, out)
		}
		return
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		collectCLIErrors(wrapped.Unwrap(), out)
	}
}

// backendCLIError returns a lifecycle CLI error only when its binary
// belongs to the selected backend. An empty Binary is the historical
// container default.
func backendCLIError(err error, backend string) (*cli.CLIError, bool) {
	var cliErrs []*cli.CLIError
	collectCLIErrors(err, &cliErrs)
	if len(cliErrs) == 0 {
		return nil, false
	}
	for _, cliErr := range cliErrs {
		op, _, validOperation := notFoundTarget(cliErr.Args)
		if !validOperation || (op != "inspect" && op != "delete" && op != "rm" && op != "stop" && op != "exec" && op != "logs") {
			continue
		}
		binary := normalizedCLIBinary(cliErr.Binary)
		wantBinary := backend
		if backend == "apple" {
			wantBinary = "container"
		}
		if binary == wantBinary || (binary == "" && backend == "apple") {
			return cliErr, true
		}
	}
	return nil, false
}

func normalizedCLIBinary(binary string) string {
	if binary == "" {
		return ""
	}
	if i := strings.LastIndexAny(binary, `/\\`); i >= 0 {
		binary = binary[i+1:]
	}
	return strings.TrimSuffix(strings.ToLower(binary), ".exe")
}

func notFoundTarget(args []string) (string, string, bool) {
	if len(args) == 0 {
		return "", "", false
	}
	op := strings.ToLower(args[0])
	switch op {
	case "inspect", "delete", "rm", "stop", "logs":
		return op, strings.TrimSpace(args[len(args)-1]), true
	case "exec":
		for i := 1; i < len(args); i++ {
			arg := args[i]
			switch arg {
			case "--env-file", "--user", "--workdir":
				i++
				continue
			}
			if strings.HasPrefix(arg, "-") {
				continue
			}
			return op, strings.TrimSpace(arg), true
		}
	}
	return "", "", false
}

func normalizeNotFoundTarget(target string) string {
	target = strings.TrimSpace(strings.Trim(target, `"'`))
	target = strings.TrimPrefix(target, "/")
	return strings.ToLower(target)
}

func stripCLIErrorPrefix(line string) string {
	line = strings.TrimSpace(line)
	for {
		lower := strings.ToLower(line)
		switch {
		case strings.HasPrefix(lower, "error response from daemon:"):
			line = strings.TrimSpace(line[len("error response from daemon:"):])
		case strings.HasPrefix(lower, "error:"):
			line = strings.TrimSpace(line[len("error:"):])
		case strings.HasPrefix(lower, "inspect failed:"):
			line = strings.TrimSpace(line[len("inspect failed:"):])
		case strings.HasPrefix(lower, "delete failed:"):
			line = strings.TrimSpace(line[len("delete failed:"):])
		case strings.HasPrefix(lower, "rm failed:"):
			line = strings.TrimSpace(line[len("rm failed:"):])
		case strings.HasPrefix(lower, "exec failed:"):
			line = strings.TrimSpace(line[len("exec failed:"):])
		case strings.HasPrefix(lower, "logs failed:"):
			line = strings.TrimSpace(line[len("logs failed:"):])
		case strings.HasPrefix(lower, "stop failed:"):
			line = strings.TrimSpace(line[len("stop failed:"):])
		default:
			return line
		}
	}
}

func exactNotFoundValue(rest, target string) bool {
	rest = strings.TrimSpace(rest)
	if rest == "" || target == "" {
		return false
	}
	if strings.HasPrefix(rest, `"`) {
		end := strings.IndexByte(rest[1:], '"')
		if end < 0 {
			return false
		}
		value := rest[1 : 1+end]
		if strings.TrimSpace(rest[end+2:]) != "" {
			return false
		}
		return normalizeNotFoundTarget(value) == normalizeNotFoundTarget(target)
	}
	if strings.ContainsAny(rest, " \t\r\n,") {
		return false
	}
	return normalizeNotFoundTarget(rest) == normalizeNotFoundTarget(target)
}

func exactColonNotFound(line, phrase, target string) bool {
	if !strings.HasPrefix(strings.ToLower(line), strings.ToLower(phrase)+":") {
		return false
	}
	return exactNotFoundValue(line[len(phrase)+1:], target)
}

func exactAppleIDNotFound(line, target string) bool {
	const prefix = "container with id "
	lower := strings.ToLower(line)
	if !strings.HasPrefix(lower, prefix) || !strings.HasSuffix(lower, " not found") {
		return false
	}
	return exactNotFoundValue(line[len(prefix):len(line)-len(" not found")], target)
}

func exactAppleExecNotFound(line, target string) bool {
	const prefix = "get failed: container "
	lower := strings.ToLower(line)
	if !strings.HasPrefix(lower, prefix) || !strings.HasSuffix(lower, " not found") {
		return false
	}
	return exactNotFoundValue(line[len(prefix):len(line)-len(" not found")], target)
}

func exactContainerNotFoundFor(err error, backend string) bool {
	cliErr, ok := backendCLIError(err, backend)
	if !ok || len(cliErr.Args) == 0 {
		return false
	}
	op, target, ok := notFoundTarget(cliErr.Args)
	if !ok || target == "" {
		return false
	}
	for _, rawLine := range strings.Split(cliErr.Stderr, "\n") {
		line := stripCLIErrorPrefix(rawLine)
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		// The generic form is retained for older CLI versions and the
		// repository's fake runners, but it still requires an exact
		// operation and target. Permission/configuration text does not
		// have this shape.
		if exactColonNotFound(line, "not found", target) {
			return true
		}
		if backend == "docker" {
			phrase := "no such object"
			if op != "inspect" {
				phrase = "no such container"
			}
			if exactColonNotFound(line, phrase, target) {
				return true
			}
			continue
		}
		if op == "inspect" && (exactColonNotFound(line, "container not found", target) || exactColonNotFound(line, "no such object", target)) {
			return true
		}
		if op == "exec" && (exactAppleExecNotFound(line, target) || exactColonNotFound(line, "no such container", target)) {
			return true
		}
		if op == "stop" || op == "delete" || op == "rm" {
			for {
				if exactAppleIDNotFound(line, target) {
					return true
				}
				var rest string
				switch op {
				case "stop":
					const prefix = "failed to stop container:"
					if !strings.HasPrefix(lower, prefix) {
						break
					}
					rest = strings.TrimSpace(line[len(prefix):])
				case "delete", "rm":
					const prefix = "failed to delete container:"
					if !strings.HasPrefix(lower, prefix) {
						break
					}
					rest = strings.TrimSpace(line[len(prefix):])
				}
				if rest == "" {
					break
				}
				line = rest
				lower = strings.ToLower(line)
			}
		}
		if op == "logs" {
			if exactColonNotFound(line, "no such container", target) {
				return true
			}
			for range 3 {
				if exactAppleIDNotFound(line, target) || exactAppleExecNotFound(line, target) {
					return true
				}
				if strings.HasPrefix(lower, "failed to open container logs:") {
					line = strings.TrimSpace(line[len("failed to open container logs:"):])
					lower = strings.ToLower(line)
					continue
				}
				const prefix = "failed to get logs for container "
				if !strings.HasPrefix(lower, prefix) {
					break
				}
				rest := strings.TrimSpace(line[len(prefix):])
				separator := strings.IndexByte(rest, ':')
				if separator < 0 {
					break
				}
				line = strings.TrimSpace(rest[separator+1:])
				lower = strings.ToLower(line)
			}
		}
	}
	return false
}

func exactNameConflictFor(err error, backend string) bool {
	var cliErrs []*cli.CLIError
	collectCLIErrors(err, &cliErrs)
	for _, cliErr := range cliErrs {
		binary := normalizedCLIBinary(cliErr.Binary)
		wantBinary := backend
		if backend == "apple" {
			wantBinary = "container"
		}
		if binary != wantBinary && (binary != "" || backend != "apple") {
			continue
		}
		if len(cliErr.Args) == 0 || strings.ToLower(cliErr.Args[0]) != "run" {
			continue
		}
		target := ""
		for i, arg := range cliErr.Args {
			if arg == "--name" && i+1 < len(cliErr.Args) {
				target = cliErr.Args[i+1]
				break
			}
		}
		for _, rawLine := range strings.Split(cliErr.Stderr, "\n") {
			line := stripCLIErrorPrefix(rawLine)
			lower := strings.ToLower(line)
			switch backend {
			case "apple":
				const idPrefix = "container with id "
				if strings.HasPrefix(lower, idPrefix) && strings.HasSuffix(lower, " already exists") {
					value := line[len(idPrefix) : len(line)-len(" already exists")]
					if exactNotFoundValue(value, target) || (target == "" && exactNotFoundValue(value, value)) {
						return true
					}
				}
				const existsPrefix = "already exists: container "
				if strings.HasPrefix(lower, existsPrefix) {
					value := strings.TrimSpace(line[len(existsPrefix):])
					if target == "" || exactNotFoundValue(value, target) {
						return true
					}
				}
			case "docker":
				const conflictPrefix = "conflict. the container name "
				if strings.HasPrefix(lower, conflictPrefix) && strings.Contains(lower, " is already in use by container") {
					rest := strings.TrimSpace(line[len(conflictPrefix):])
					end := strings.IndexByte(rest[1:], '"')
					if end >= 0 {
						end++
						name := rest[1:end]
						if target == "" || normalizeNotFoundTarget(name) == normalizeNotFoundTarget(target) {
							return true
						}
					}
				}
			}
		}
	}
	return false
}

func exactImageLine(line, phrase, target string) bool {
	if exactColonNotFound(line, phrase, target) {
		return true
	}
	if len(line) <= len(phrase)+1 {
		return false
	}
	rest := line[len(phrase)+1:]
	if strings.HasSuffix(strings.ToLower(strings.TrimSpace(rest)), " (no such image)") {
		rest = strings.TrimSpace(rest[:len(rest)-len(" (no such image)")])
		return exactNotFoundValue(rest, target)
	}
	return false
}

func exactImageMissingFor(err error, backend string) bool {
	var cliErrs []*cli.CLIError
	collectCLIErrors(err, &cliErrs)
	for _, cliErr := range cliErrs {
		binary := normalizedCLIBinary(cliErr.Binary)
		wantBinary := backend
		if backend == "apple" {
			wantBinary = "container"
		}
		if binary != wantBinary && (binary != "" || (backend != "apple" && backend != "docker")) {
			continue
		}
		if len(cliErr.Args) < 3 || cliErr.Args[0] != "image" || cliErr.Args[1] != "inspect" {
			continue
		}
		target := strings.TrimSpace(cliErr.Args[len(cliErr.Args)-1])
		phrase := "no such image"
		if backend == "apple" {
			phrase = "image not found"
		}
		for _, rawLine := range strings.Split(cliErr.Stderr, "\n") {
			line := stripCLIErrorPrefix(rawLine)
			if exactImageLine(line, phrase, target) {
				return true
			}
			if backend == "docker" && binary == "" && exactImageLine(line, "image not found", target) {
				return true
			}
		}
	}
	return false
}

func exactContainerNotFound(err error) bool {
	var cliErr *cli.CLIError
	if !errors.As(err, &cliErr) {
		return false
	}
	if cliErr.Binary == "" {
		return exactContainerNotFoundFor(err, "apple") || exactContainerNotFoundFor(err, "docker")
	}
	return exactContainerNotFoundFor(err, "docker") || exactContainerNotFoundFor(err, "apple")
}

func isNotFound(err error) bool {
	return errors.Is(err, ErrContainerNotFound) || exactContainerNotFound(err)
}

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
	switch eng.name() {
	case "apple":
		return appleEngine{}.containerMissing(err)
	case "docker":
		return dockerEngine{}.containerMissing(err)
	default:
		return false
	}
}

func wrapNotFound(err error) error {
	return wrapNotFoundFor(nil, err)
}

func wrapNotFoundFor(eng engine, err error) error {
	if err == nil || errors.Is(err, ErrContainerNotFound) || !isNotFoundFor(eng, err) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrContainerNotFound, err)
}
