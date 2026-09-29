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

// ErrImageIdentityUnavailable reports that an image inspect completed
// successfully but did not provide a usable immutable identity (a
// repository-bearing digest or a verified local image ID). It is not
// used for transport, permission, or cancellation failures. Run fails
// closed by default; WithAllowMutableImageTag opts into passing the
// original mutable tag to the backend. Apple name@digest references,
// including name:tag@digest, remain immutable and are never downgraded by
// that option. PullNever also rejects an identity-less Apple inspect even
// when the caller supplied a digest, because the backend has not confirmed
// the local identity.
var ErrImageIdentityUnavailable = errors.New("backend did not report an immutable image identity")

// ErrImageIdentityMismatch reports that image inspect returned an
// identity that does not belong to the requested image. It is never
// downgraded to the mutable-tag fallback.
var ErrImageIdentityMismatch = errors.New("backend reported a different image identity")

// ErrImageIdentityNotLocal reports that a resolved immutable identity
// was successfully checked but is not available from the backend's local
// image store under the selected policy. Apple Container has no run-time
// --pull=never switch; running an unaddressable pinned reference could
// otherwise fetch it, so Run fails before create unless the caller opted
// into the mutable-tag compatibility fallback.
var ErrImageIdentityNotLocal = errors.New("immutable image identity is not available locally under the selected pull policy")

// ErrContainerNotFound reports that the container does not exist.
// Inspect, State, Exec, Logs, Stop, and copy operations wrap it with %w
// so callers can use errors.Is instead of matching CLI stderr text.
var ErrContainerNotFound = errors.New("container not found")

// ErrGenerationReplaced reports that Terminate refused to delete because
// the live container's creation label no longer matches this handle.
var ErrGenerationReplaced = errors.New("container was recreated; refusing to delete replaced container")

const (
	lifecycleRun     = "run"
	lifecycleInspect = "inspect"
	lifecycleDelete  = "delete"
	lifecycleStop    = "stop"
	lifecycleExec    = "exec"
	lifecycleLogs    = "logs"
	lifecycleCopy    = "copy"
)

func lifecycleCommandMatches(operation, command string) bool {
	command = strings.ToLower(command)
	switch operation {
	case lifecycleRun:
		return command == "run"
	case lifecycleInspect:
		return command == "inspect"
	case lifecycleDelete:
		return command == "delete" || command == "rm"
	case lifecycleStop:
		return command == "stop"
	case lifecycleExec:
		return command == "exec"
	case lifecycleLogs:
		return command == "logs"
	case lifecycleCopy:
		return command == "cp"
	default:
		return false
	}
}

func lifecycleTargetInError(cliErr *cli.CLIError, target string) bool {
	if cliErr == nil || target == "" {
		return false
	}
	target = strings.ToLower(target)
	for _, arg := range cliErr.Args {
		arg = strings.ToLower(arg)
		if arg == target || strings.HasPrefix(arg, target+":") {
			return true
		}
	}
	// A complete lifecycle argv names the target explicitly. Only legacy
	// injected errors that contain just the command may fall back to the
	// target in stderr; a different complete argv must not be overridden by
	// diagnostic text.
	if len(cliErr.Args) > 1 {
		return false
	}
	return lifecycleTargetInText(cliErr.Stderr, target)
}

func lifecycleTargetInText(text, target string) bool {
	text = strings.ToLower(text)
	target = strings.ToLower(strings.TrimSpace(target))
	if target == "" {
		return false
	}
	for _, field := range strings.FieldsFunc(text, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '"' || r == '\'' || r == '(' || r == ')' || r == '[' || r == ']' || r == '{' || r == '}' || r == ',' || r == ':' || r == ';' || r == '/'
	}) {
		if field == target {
			return true
		}
	}
	return false
}

func lifecycleTargetNotFoundAtStart(text, target string) bool {
	for _, raw := range strings.Split(strings.ToLower(text), "\n") {
		line := strings.TrimSpace(raw)
		for _, prefix := range []string{"error:", "failed:", "warning:"} {
			line = strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
		if strings.HasPrefix(line, "not found:") && lifecycleTargetNotFound(line, target) {
			return true
		}
	}
	return false
}

func lifecycleTargetNotFound(text, target string) bool {
	text = strings.ToLower(text)
	target = strings.ToLower(strings.TrimSpace(target))
	if target == "" {
		return false
	}
	if strings.Contains(text, "not found: \""+target+"\"") {
		return true
	}
	needle := "not found: " + target
	for start := 0; ; {
		index := strings.Index(text[start:], needle)
		if index < 0 {
			return false
		}
		index += start
		end := index + len(needle)
		if end == len(text) || strings.ContainsRune(" \t\r\n,;)]}", rune(text[end])) {
			return true
		}
		start = end
	}
}

func lifecycleCLIErrorForBackend(err error, backend, operation, target string) (*cli.CLIError, bool) {
	var cliErr *cli.CLIError
	if !errors.As(err, &cliErr) || len(cliErr.Args) == 0 {
		return nil, false
	}
	if binary := strings.ToLower(strings.TrimSpace(cliErr.Binary)); binary != "" && binary != backend {
		return nil, false
	}
	if !lifecycleCommandMatches(operation, cliErr.Args[0]) || !lifecycleTargetInError(cliErr, target) {
		return nil, false
	}
	return cliErr, true
}

// isContainerNotFound reports a missing target only for the requested
// backend, lifecycle operation, and target. Generic application output
// such as `command not found` is deliberately not a container absence.
func isContainerNotFound(eng engine, operation, target string, err error) bool {
	if errors.Is(err, ErrContainerNotFound) {
		return true
	}
	return eng != nil && eng.containerMissing(operation, target, err)
}

// wrapContainerNotFound preserves the operation/backend/target context
// while exposing the stable root-package sentinel.
func wrapContainerNotFound(eng engine, operation, target string, err error) error {
	if err == nil || !isContainerNotFound(eng, operation, target, err) || errors.Is(err, ErrContainerNotFound) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrContainerNotFound, err)
}
