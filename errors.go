package container

import (
	"context"
	"errors"
	"fmt"
	"os"
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

// appleTypedNotFoundLine recognizes the structured Apple Container 1.3
// error vocabulary. It accepts only a typed notFound value and the
// operation-specific message underneath internalError/cause wrappers.
func appleTypedNotFoundLine(line, target, operation string) bool {
	message, ok := appleTypedNotFoundMessage(line)
	return ok && appleContainerNotFoundMessage(message, target, operation)
}

// appleTypedContainerIDNotFoundLine is used by the concurrent-create
// classifier. A run failure with the typed ID form is a lost-create
// race; an arbitrary application line is not.
func appleTypedContainerIDNotFoundLine(line, target string) bool {
	message, ok := appleTypedNotFoundMessage(line)
	return ok && appleIDMessageMatches(message, target)
}

// appleTypedNotFoundMessage unwraps only the typed wrapper grammar. Swift's
// rendered descriptions occur in both escaped and older unescaped nested
// forms, so each quoted cause is decoded before the next wrapper is read.
func appleTypedNotFoundMessage(line string) (string, bool) {
	current := strings.TrimSpace(line)
	for range 16 {
		current = strings.TrimSpace(current)
		current = strings.TrimSpace(strings.Trim(current, "()"))
		lower := strings.ToLower(current)
		if strings.HasPrefix(lower, "error:") {
			current = strings.TrimSpace(current[len("error:"):])
			continue
		}
		switch {
		case strings.HasPrefix(lower, "notfound:"):
			value := decodeAppleQuotedValue(strings.TrimSpace(current[len("notfound:"):]))
			value = strings.TrimSpace(value)
			return value, value != ""
		case strings.HasPrefix(lower, "internalerror:"), strings.HasPrefix(lower, "cause:"):
			index := strings.Index(lower, "cause:")
			if index < 0 {
				value := decodeAppleQuotedValue(strings.TrimSpace(current[strings.Index(current, ":")+1:]))
				if value == "" {
					return "", false
				}
				if notFound := strings.Index(strings.ToLower(value), "notfound:"); notFound >= 0 {
					current = value[notFound:]
					continue
				}
				return "", false
			}
			current = decodeAppleQuotedValue(strings.TrimSpace(current[index+len("cause:"):]))
			if current == "" {
				return "", false
			}
		case strings.HasPrefix(lower, "invalidargument:"):
			value := decodeAppleQuotedValue(strings.TrimSpace(current[len("invalidargument:"):]))
			if value == "" {
				return "", false
			}
			// The logs client wraps its typed absence in invalidArgument
			// rather than internalError. Peel only the known typed value.
			if index := strings.Index(strings.ToLower(value), "notfound:"); index >= 0 {
				current = value[index:]
			} else {
				current = value
			}
		default:
			return "", false
		}
	}
	return "", false
}

// appleTypedExistsMessage unwraps Apple's typed exists errors. Run uses
// this form for a concurrent name conflict, so it must be recognized as
// precisely as the older untyped "already exists" wording.
func appleTypedExistsMessage(line string) (string, bool) {
	current := strings.TrimSpace(line)
	for range 16 {
		current = strings.TrimSpace(strings.Trim(current, "()"))
		lower := strings.ToLower(current)
		if strings.HasPrefix(lower, "error:") {
			current = strings.TrimSpace(current[len("error:"):])
			continue
		}
		switch {
		case strings.HasPrefix(lower, "exists:"):
			value := strings.TrimSpace(decodeAppleQuotedValue(strings.TrimSpace(current[len("exists:"):])))
			return value, value != ""
		case strings.HasPrefix(lower, "internalerror:"), strings.HasPrefix(lower, "cause:"):
			index := strings.Index(lower, "cause:")
			if index < 0 {
				value := strings.TrimSpace(decodeAppleQuotedValue(strings.TrimSpace(current[strings.Index(current, ":")+1:])))
				if value == "" {
					return "", false
				}
				if exists := strings.Index(strings.ToLower(value), "exists:"); exists >= 0 {
					current = value[exists:]
					continue
				}
				return "", false
			}
			current = strings.TrimSpace(decodeAppleQuotedValue(strings.TrimSpace(current[index+len("cause:"):])))
			if current == "" {
				return "", false
			}
		case strings.HasPrefix(lower, "invalidargument:"):
			value := strings.TrimSpace(decodeAppleQuotedValue(strings.TrimSpace(current[len("invalidargument:"):])))
			if value == "" {
				return "", false
			}
			current = value
		default:
			return "", false
		}
	}
	return "", false
}

func appleContainerExistsMessage(message, target string) bool {
	message = strings.TrimSpace(strings.Trim(message, `"'`))
	lower := strings.ToLower(message)
	const idPrefix = "container with id "
	if strings.HasPrefix(lower, idPrefix) && strings.HasSuffix(lower, " already exists") {
		return exactNotFoundValue(message[len(idPrefix):len(message)-len(" already exists")], target)
	}
	const existsPrefix = "container already exists:"
	if strings.HasPrefix(lower, existsPrefix) {
		return exactNotFoundValue(message[len(existsPrefix):], target)
	}
	return false
}

// decodeAppleQuotedValue removes one outer quoted value and decodes the
// escaping used by Swift's diagnostic descriptions. When the CLI emits
// nested unescaped quotes, the outer quote is removed first and the inner
// typed value remains available to the next loop iteration.
func decodeAppleQuotedValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 0 && (value[0] == '"' || value[0] == '\'') {
		quote := value[0]
		if len(value) < 2 || value[len(value)-1] != quote {
			return ""
		}
		value = value[1 : len(value)-1]
	}
	var b strings.Builder
	b.Grow(len(value))
	escaped := false
	for _, r := range value {
		if escaped {
			switch r {
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case '"', '\\':
				b.WriteRune(r)
			default:
				b.WriteByte('\\')
				b.WriteRune(r)
			}
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		b.WriteRune(r)
	}
	if escaped {
		b.WriteByte('\\')
	}
	return strings.TrimSpace(b.String())
}

func appleContainerNotFoundMessage(message, target, operation string) bool {
	message = strings.TrimSpace(strings.Trim(message, `"'`))
	lower := strings.ToLower(message)
	if strings.HasPrefix(lower, "container not found:") {
		return exactNotFoundValue(message[len("container not found:"):], target)
	}
	switch operation {
	case "exec":
		return exactAppleExecNotFound(message, target)
	case "stop", "delete", "rm":
		return exactAppleIDNotFound(message, target)
	case "logs":
		return exactAppleIDNotFound(message, target) || exactAppleExecNotFound(message, target)
	case "run":
		return exactAppleIDNotFound(message, target)
	default:
		return false
	}
}

func appleIDMessageMatches(message, target string) bool {
	return exactAppleIDNotFound(strings.TrimSpace(strings.Trim(message, `"'`)), target)
}

// cliErrorDiagnosticLines returns the diagnostic lines of a CLIError from
// both streams. The backends report some failures on stdout (a streamed
// command, for example), so a matcher that reads only Stderr would miss a
// not-found or conflict that the backend did print.
func cliErrorDiagnosticLines(err error, cliErr *cli.CLIError) []string {
	stdout, stderr, ok := cli.DiagnosticText(err)
	if !ok {
		stdout, stderr = "", cliErr.Stderr
	}
	if stderr == "" {
		stderr = cliErr.Stderr
	}
	return strings.Split(stdout+"\n"+stderr, "\n")
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
	for _, rawLine := range cliErrorDiagnosticLines(err, cliErr) {
		line := stripCLIErrorPrefix(rawLine)
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		if backend == "apple" && appleTypedNotFoundLine(line, target, op) {
			return true
		}
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
		for _, rawLine := range cliErrorDiagnosticLines(err, cliErr) {
			line := stripCLIErrorPrefix(rawLine)
			lower := strings.ToLower(line)
			if backend == "apple" {
				if message, ok := appleTypedExistsMessage(line); ok && appleContainerExistsMessage(message, target) {
					return true
				}
			}
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
		for _, rawLine := range cliErrorDiagnosticLines(err, cliErr) {
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
	if hasDefinitiveErrorBranch(err) {
		return false
	}
	return errors.Is(err, ErrContainerNotFound) || exactContainerNotFound(err)
}

func hasDefinitiveErrorBranch(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, os.ErrPermission) || errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrInvalid) {
		return true
	}
	var cliErrs []*cli.CLIError
	collectCLIErrors(err, &cliErrs)
	for _, cliErr := range cliErrs {
		if cli.IsDefinitiveNonLivenessError(cliErr) {
			return true
		}
	}
	return false
}

func isNotFoundFor(eng engine, err error) bool {
	if err == nil || hasDefinitiveErrorBranch(err) {
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
