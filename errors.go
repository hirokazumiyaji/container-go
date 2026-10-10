package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/wait"
)

// CLIError is a non-zero exit from the backend CLI. It aliases
// internal/cli.CLIError so callers can use errors.As without importing
// an internal package. The public four-field layout is retained for
// compatibility; classification may use additional bounded diagnostics.
type CLIError = cli.CLIError

// ErrSystemNotRunning reports that the selected container backend is
// not running. The classified error retains the backend-specific hint.
var ErrSystemNotRunning = cli.ErrSystemNotRunning

// ErrLogStreamSetup reports that a log stream cannot be started with the
// configured runner or backend executable and will not be retried.
var ErrLogStreamSetup = wait.ErrLogStreamSetup

// ErrInvalidConfiguration reports a wait strategy that cannot run with
// the options supplied to Run.
var ErrInvalidConfiguration = wait.ErrInvalidConfiguration

// ErrInvalidConfig reports an option combination that the selected
// backend cannot honor. Run returns it as a *ConfigError.
var ErrInvalidConfig = errors.New("invalid container configuration")

// ConfigError describes an invalid option combination before container
// creation. Callers can use errors.As to inspect the backend, network,
// and option involved.
type ConfigError struct {
	Backend string
	Network string
	Option  string
	Detail  string
}

func (e *ConfigError) Error() string {
	scope := "container"
	if e.Backend != "" {
		scope = e.Backend
	}
	message := ErrInvalidConfig.Error() + ": " + scope + " configuration"
	if e.Network != "" {
		message += fmt.Sprintf(" for network %q", e.Network)
	}
	if e.Option != "" {
		message += " (" + e.Option + ")"
	}
	if e.Detail != "" {
		message += ": " + e.Detail
	}
	return message
}

func (e *ConfigError) Unwrap() error { return ErrInvalidConfig }

// ErrPortNotExposed reports a port that was not declared via
// WithExposedPorts or WithPublishedPort, or that has no usable host
// binding in the backend's actual network mode.
var ErrPortNotExposed = wait.ErrPortNotExposed

// ErrEndpointUnreachable reports an inspected host binding that cannot
// be reached by this client, such as loopback on a remote Docker daemon.
var ErrEndpointUnreachable = errors.New("container endpoint is unreachable")

// ErrNetworkMismatch reports that the network reported by inspect does
// not match the network requested for the handle.
var ErrNetworkMismatch = errors.New("container network mode does not match the requested network")

// ErrNoReachableHost reports a backend network that has no host endpoint.
var ErrNoReachableHost = errors.New("container has no reachable host")

// ErrInvalidOption identifies an invalid public option, operation argument,
// or option value. Callers can use errors.Is without matching the
// human-readable message, or use errors.As with *ValidationError for field
// metadata.
var ErrInvalidOption = errors.New("invalid option")

// ValidationError describes a public input rejected before a backend
// operation starts.
//
// Option names the public option or operation. Field names the exact input
// field, such as "key", "value", "hostPath", or "containerPath". Value is
// the rejected value when it is safe to expose; values that may contain
// credentials or other sensitive material are represented by nil. Message is
// retained for source compatibility, but Err is the source of truth for the
// rendered message and the error chain.
type ValidationError struct {
	Option  string
	Field   string
	Value   any
	Message string
	Err     error
}

func (e *ValidationError) Error() string {
	if e == nil {
		return "<nil>"
	}
	// Derive the message from Err so callers cannot make Message and Err
	// disagree by mutating the exported compatibility field.
	if e.Err != nil {
		return e.Err.Error()
	}
	if e.Message != "" {
		return e.Message
	}
	return ErrInvalidOption.Error()
}

// Unwrap preserves an underlying parse or validation error when one is
// available. Is classifies every ValidationError as ErrInvalidOption.
func (e *ValidationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (e *ValidationError) Is(target error) bool {
	if e == nil {
		return false
	}
	if target == ErrInvalidOption {
		return true
	}
	return e.Err != nil && errors.Is(e.Err, target)
}

func newValidationError(option string, value any, err error) error {
	return newValidationErrorWithField(option, option, value, err)
}

func newValidationErrorWithField(option, field string, value any, err error) error {
	if err == nil {
		err = ErrInvalidOption
	}
	var existing *ValidationError
	if errors.As(err, &existing) {
		return err
	}
	return &ValidationError{
		Option:  option,
		Field:   field,
		Value:   value,
		Message: err.Error(),
		Err:     err,
	}
}

func validationErrorf(option string, value any, format string, args ...any) error {
	return newValidationError(option, value, fmt.Errorf(format, args...))
}

// ErrImageNotFound reports that an image is not in the backend's local
// store. Run returns it when the pull policy is PullNever and the image
// is absent.
var ErrImageNotFound = errors.New("image not found in local store")

// ErrEnvFileUnsupported reports that the current platform cannot provide
// the per-user private temporary storage required for environment files.
// On Windows, Run and Exec return this error when a non-empty WithEnv or
// WithExecEnv option would require such a file.
var ErrEnvFileUnsupported = errors.New("secure environment files are not supported on this platform")

// ErrContainerNotFound reports that the container does not exist.
// Inspect, State, Exec, Logs, and FollowLogs wrap it with %w so callers can use
// errors.Is instead of matching CLI stderr text. For FollowLogs,
// a failure after the stream has started is reported by Read.
var ErrContainerNotFound = wait.ErrContainerNotFound

// ErrGenerationReplaced reports that a handle's immutable identity or
// generation no longer matches the live container. Destructive and endpoint
// operations refuse to act on the replacement.
var ErrGenerationReplaced = errors.New("container was recreated; refusing to delete replaced container")

// ErrCopyFileNotRegular reports that a file copied out of a container
// was not a regular file. Callers should not consume paths that resolve
// to directories, links, or other special files.
var ErrCopyFileNotRegular = errors.New("copied container path is not a regular file")

// ErrCopyFileFromContainerUnsupported reports that the selected backend
// or host cannot safely perform a file copy-out. This includes Docker
// client/server versions below the supported copy-out minimum. The method
// fails before invoking `cp` when the backend, host, or version cannot
// preserve and validate file types.
var ErrCopyFileFromContainerUnsupported = errors.New("CopyFileFromContainer is not safely supported by this backend or host")

// ErrUnsupportedCapability reports a configuration that the selected
// backend cannot support safely, such as a bind mount whose source the
// remote daemon would resolve on its own host. Every rejection of that class
// wraps this sentinel, so callers detect it uniformly with errors.Is.
var ErrUnsupportedCapability = errors.New("unsupported capability")

var errInspectTargetNotFound = errors.New("inspect target not found")

type inspectTargetNotFoundError struct {
	target string
	detail string
}

func (e *inspectTargetNotFoundError) Error() string {
	if e.detail == "" {
		return fmt.Sprintf("inspect target %q not found", e.target)
	}
	return fmt.Sprintf("inspect target %q not found: %s", e.target, e.detail)
}

func (e *inspectTargetNotFoundError) Unwrap() error { return errInspectTargetNotFound }

func (e *inspectTargetNotFoundError) Is(target error) bool {
	return target == errInspectTargetNotFound || target == ErrContainerNotFound
}

func newInspectTargetNotFound(target, detail string) error {
	return &inspectTargetNotFoundError{target: target, detail: detail}
}

func wrapInspectTargetNotFound(err error) error {
	return fmt.Errorf("%w: %w", ErrContainerNotFound, err)
}

func isBareContainerNotFound(err error) bool {
	if err == nil || !errors.Is(err, ErrContainerNotFound) {
		return false
	}
	found := false
	conflict := false
	var walk func(error)
	walk = func(cur error) {
		if cur == nil || conflict {
			return
		}
		if cur == ErrContainerNotFound || cur == errInspectTargetNotFound {
			found = true
			return
		}
		if _, ok := cur.(*inspectTargetNotFoundError); ok {
			found = true
			return
		}
		if joined, ok := cur.(interface{ Unwrap() []error }); ok {
			for _, child := range joined.Unwrap() {
				walk(child)
			}
			return
		}
		if wrapped, ok := cur.(interface{ Unwrap() error }); ok {
			walk(wrapped.Unwrap())
			return
		}
		conflict = true
	}
	walk(err)
	return found && !conflict
}

// isNotFound reports whether a CLI failure means the container does not
// exist. It is retained for callers that do not have an engine context;
// the concrete backend and command still have to pass their own matcher.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	if isBareContainerNotFound(err) {
		return true
	}
	branches := backendCLIErrorBranches(err, "")
	for _, branch := range branches {
		eng := engineForCLIError(branch.ctx.err)
		if eng == nil || !eng.containerMissing(branch.ctx.err) {
			continue
		}
		backendBranches := backendCLIErrorBranches(err, eng.binary())
		if definitiveBranchVetoes(err, branch, backendBranches) || hasNonCLIAmbiguousObjectText(err) {
			continue
		}
		return true
	}
	for _, branch := range branches {
		if isAmbiguousApplicationError(engineForCLIError(branch.ctx.err), err) {
			return false
		}
	}
	return false
}

// isNotFoundFor applies the selected backend's classifier. Keeping this
// separate from isNotFound prevents a Docker error from being accepted by
// an Apple operation (and vice versa) when callers do have engine context.
func isNotFoundFor(eng engine, err error) bool {
	if err == nil {
		return false
	}
	if eng == nil {
		return isNotFound(err)
	}
	if isBareContainerNotFound(err) {
		return true
	}
	branches := backendCLIErrorBranches(err, eng.binary())
	for _, branch := range branches {
		if !eng.containerMissing(branch.ctx.err) {
			continue
		}
		if definitiveBranchVetoes(err, branch, branches) || hasNonCLIAmbiguousObjectText(err) {
			continue
		}
		return true
	}
	if isAmbiguousApplicationError(eng, err) {
		return false
	}
	return false
}

func isNotFoundForOperation(eng engine, err error, operation string, targets ...string) bool {
	if eng == nil {
		return isNotFound(err)
	}
	selected := matchingCLIErrorBranches(err, eng.binary(), operation, targets...)
	if operation == "inspect" {
		found, conflict := inspectAbsenceStatus(eng, err, firstTarget(targets))
		if conflict {
			return false
		}
		if found && len(selected) == 0 {
			return true
		}
	}
	if isBareContainerNotFound(err) {
		return true
	}
	if len(selected) == 0 {
		return false
	}
	parts := make([]error, 0, len(selected)+1)
	for _, branch := range selected {
		parts = append(parts, branch.cause)
	}
	var nonCLI []error
	collectNonCLIErrorBranches(err, &nonCLI)
	parts = append(parts, nonCLI...)
	return isNotFoundFor(eng, errors.Join(parts...))
}

func firstTarget(targets []string) string {
	for _, target := range targets {
		if target != "" {
			return target
		}
	}
	return ""
}

func inspectAbsenceStatus(eng engine, err error, target string) (found, conflict bool) {
	if err == nil {
		return false, false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			childFound, childConflict := inspectAbsenceStatus(eng, child, target)
			found = found || childFound
			conflict = conflict || childConflict
		}
		return found, conflict
	}
	if detail, ok := err.(*inspectTargetNotFoundError); ok {
		if target == "" || sameCLITarget(detail.target, target) {
			return true, false
		}
		return false, true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return inspectAbsenceStatus(eng, wrapped.Unwrap(), target)
	}
	if err == errInspectTargetNotFound {
		return target == "", target != ""
	}
	if err == ErrContainerNotFound {
		return false, false
	}
	var cliErr *cli.CLIError
	if errors.As(err, &cliErr) {
		if !cliBinaryMatches(cliErr.Binary, eng.binary()) {
			return false, true
		}
		operation := commandOperation(cliErr.Args)
		if operation != "inspect" || (target != "" && !sameCLITarget(commandTarget(cliErr.Args, operation), target)) {
			return false, true
		}
		if eng.containerMissing(cliErr) {
			return true, false
		}
		return false, true
	}
	return false, true
}

func engineForCLIError(err *cli.CLIError) engine {
	if err == nil {
		return nil
	}
	if cliBinaryMatches(err.Binary, "docker") {
		return dockerEngine{}
	}
	return appleEngine{}
}

func definitiveBranchVetoes(err error, candidate cliErrorBranch, branches []cliErrorBranch) bool {
	if hasNonCLIDefinitiveErrorText(err) {
		return true
	}
	for _, branch := range branches {
		if !sameCLIErrorContext(branch, candidate) {
			continue
		}
		if cli.IsDefinitiveNonLivenessError(branch.cause) {
			return true
		}
	}
	return false
}

func sameCLIErrorContext(a, b cliErrorBranch) bool {
	if a.ctx.operation != b.ctx.operation {
		return false
	}
	if strings.TrimSpace(a.ctx.target) == "" && strings.TrimSpace(b.ctx.target) == "" {
		return true
	}
	return sameCLITarget(a.ctx.target, b.ctx.target)
}

func hasNonCLIDefinitiveErrorText(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			if hasNonCLIDefinitiveErrorText(child) {
				return true
			}
		}
		return false
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		child := wrapped.Unwrap()
		if _, joined := child.(interface{ Unwrap() []error }); joined {
			return hasNonCLIDefinitiveErrorText(child)
		}
	}
	var cliErr *cli.CLIError
	if errors.As(err, &cliErr) {
		return false
	}
	if cli.IsDefinitiveNonLivenessError(err) {
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return hasNonCLIDefinitiveErrorText(wrapped.Unwrap())
	}
	return false
}

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
	case "cp":
		for _, arg := range args {
			if before, _, ok := strings.Cut(arg, ":"); ok && before != "" && !strings.Contains(before, string(os.PathSeparator)) && !isWindowsDriveLetter(before) {
				return strings.Trim(strings.TrimSpace(before), `"'`)
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

// isWindowsDriveLetter reports whether s is the drive of a Windows host path
// such as C:\tmp. Container names and IDs are never a single letter.
func isWindowsDriveLetter(s string) bool {
	return len(s) == 1 && ('a' <= s[0] && s[0] <= 'z' || 'A' <= s[0] && s[0] <= 'Z')
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
		line = normalizeDaemonErrorLine(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines, true
}

func normalizeDaemonErrorLine(line string) string {
	line = strings.ToLower(strings.Trim(line, "\x00 \t\r\n"))
	for {
		changed := false
		for _, prefix := range []string{"docker: ", "container: ", "error response from daemon: ", "error: "} {
			if strings.HasPrefix(line, prefix) {
				line = strings.Trim(strings.TrimPrefix(line, prefix), "\x00 \t\r\n")
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

// wrapNotFound converts a classified CLI not-found failure into
// ErrContainerNotFound so errors.Is works from the root package.
func wrapNotFound(err error) error {
	return wrapNotFoundFor(nil, err)
}

// wrapNotFoundFor applies the selected backend before converting a
// classified CLI not-found failure into ErrContainerNotFound.
func wrapNotFoundFor(eng engine, err error) error {
	if err == nil || !isNotFoundFor(eng, err) || errors.Is(err, ErrContainerNotFound) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrContainerNotFound, err)
}

func wrapNotFoundForOperation(eng engine, err error, operation string, targets ...string) error {
	if err == nil || !isNotFoundForOperation(eng, err, operation, targets...) || errors.Is(err, ErrContainerNotFound) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrContainerNotFound, err)
}

// classifyError applies the backend liveness contract, while refusing to
// probe an ambiguous application message such as a generic "container not
// found" from a run/exec process. The original CLIError is then returned
// unchanged even if a runner would report its probe as unavailable.
func classifyError(ctx context.Context, r cli.Runner, err error, eng engine) error {
	if err == nil {
		return nil
	}
	if eng == nil {
		if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
			return errors.Join(err, ctxErr)
		}
		return wrapNotFound(err)
	}
	if isAmbiguousApplicationError(eng, err) {
		if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
			return errors.Join(err, ctxErr)
		}
		return err
	}
	return cli.Classify(ctx, r, err, eng.probe())
}

func classifyErrorFor(ctx context.Context, r cli.Runner, err error, eng engine, operation string, targets ...string) error {
	if err == nil || eng == nil || operation == "" {
		return classifyError(ctx, r, err, eng)
	}
	branch, ok := matchingCLIErrorBranch(err, eng.binary(), operation, targets...)
	if !ok {
		return classifyError(ctx, r, err, eng)
	}
	allBranches := backendCLIErrorBranches(err, "")
	selected := branch.cause
	var nonCLI []error
	collectNonCLIErrorBranches(err, &nonCLI)
	parts := make([]error, 0, len(allBranches)+len(nonCLI))
	parts = append(parts, selected)
	for _, related := range allBranches {
		if related.ctx.err == branch.ctx.err || !cliBinaryMatches(related.ctx.err.Binary, eng.binary()) ||
			!sameCLIErrorContext(related, branch) {
			continue
		}
		if cli.IsDefinitiveNonLivenessError(related.cause) {
			parts = append(parts, related.cause)
		}
	}
	parts = append(parts, nonCLI...)
	if len(parts) > 1 {
		selected = errors.Join(parts...)
	}
	if isAmbiguousApplicationError(eng, selected) {
		if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(selected, ctxErr) {
			selected = errors.Join(selected, ctxErr)
		}
		if len(allBranches) == 1 && len(nonCLI) == 0 {
			return selected
		}
		return errors.Join(selected, err)
	}
	classified := cli.Classify(ctx, r, selected, eng.probe())
	if classified == nil {
		return err
	}
	if len(allBranches) == 1 && len(nonCLI) == 0 {
		return classified
	}
	return errors.Join(classified, err)
}

func collectNonCLIErrorBranches(err error, out *[]error) {
	if err == nil {
		return
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			collectNonCLIErrorBranches(child, out)
		}
		return
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		child := wrapped.Unwrap()
		if _, joined := child.(interface{ Unwrap() []error }); joined {
			collectNonCLIErrorBranches(child, out)
			return
		}
	}
	var cliErr *cli.CLIError
	if errors.As(err, &cliErr) {
		return
	}
	*out = append(*out, err)
}
