package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/wait"
)

// CLIError is a non-zero exit from the backend CLI. Its Error method
// redacts diagnostic secrets and escapes control characters; RawError is
// the explicit local-debugging escape hatch. It aliases
// internal/cli.CLIError so callers can use errors.As without importing
// an internal package. The public four-field layout is retained for
// compatibility; classification may use additional bounded diagnostics.
type CLIError = cli.CLIError

type SystemNotRunningError = cli.SystemNotRunningError

// ErrSystemNotRunning reports that the selected container backend is
// not answering: the Apple Container system service (start it with
// `container system start`) or the Docker daemon. The value is shared
// by both backends, so a caller cannot tell which one is down from the
// sentinel alone. The classified error retains the backend-specific hint
// and keeps the original CLI and probe errors in the error chain.
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

func (e *ConfigError) AsRedacted(target any, r *cli.Redactor) bool {
	ptr, ok := target.(**ConfigError)
	if !ok || e == nil {
		return false
	}
	if r == nil {
		r = cli.NewRedactor()
	}
	*ptr = &ConfigError{Backend: e.Backend, Network: e.Network, Option: e.Option, Detail: r.Text(e.Detail)}
	return true
}

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

func (e *ValidationError) AsRedacted(target any, r *cli.Redactor) bool {
	ptr, ok := target.(**ValidationError)
	if !ok || e == nil {
		return false
	}
	if r == nil {
		r = cli.NewRedactor()
	}
	var value any
	switch v := e.Value.(type) {
	case Mount:
		value = Mount{Type: v.Type, Source: r.Text(v.Source), Target: r.Text(v.Target), ReadOnly: v.ReadOnly}
	default:
		if e.Value != nil {
			reflected := reflect.ValueOf(e.Value)
			switch reflected.Kind() {
			case reflect.String:
				clone := reflect.New(reflected.Type()).Elem()
				redacted := r.Text(reflected.String())
				clone.SetString(redacted)
				value = clone.Interface()
			case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
				reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
				value = e.Value
			}
		}
	}
	var cause error
	if e.Err != nil {
		cause = cli.WithRedactor(e.Err, r)
	}
	*ptr = &ValidationError{Option: r.Text(e.Option), Field: r.Text(e.Field), Value: value, Message: r.Text(e.Message), Err: cause}
	return true
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

// ErrExecTerminationUnsupported reports that Exec stopped its local CLI
// process after a context deadline or cancellation, but could not prove
// that the backend-side exec process stopped. Neither supported CLI
// exposes a common exec-instance kill operation.
var ErrExecTerminationUnsupported = errors.New("exec process termination is unsupported by backend")

// ExecTerminationError preserves the context or infrastructure error while
// making the possible daemon-side process leak explicit. Callers should
// terminate the container or use a backend-specific cleanup mechanism when
// this error is returned.
type ExecTerminationError struct {
	// Err is the context or infrastructure error that stopped local Exec.
	Err error
}

func (e *ExecTerminationError) Error() string {
	if e == nil || e.Err == nil {
		return ErrExecTerminationUnsupported.Error()
	}
	return fmt.Sprintf("%s: %v (terminate the container or use backend-specific cleanup)", ErrExecTerminationUnsupported, e.Err)
}

func (e *ExecTerminationError) Unwrap() error { return e.Err }

func (e *ExecTerminationError) AsRedacted(target any, r *cli.Redactor) bool {
	ptr, ok := target.(**ExecTerminationError)
	if !ok || e == nil {
		return false
	}
	*ptr = &ExecTerminationError{Err: cli.WithRedactor(e.Err, r)}
	return true
}

func (e *ExecTerminationError) Is(target error) bool {
	return target == ErrExecTerminationUnsupported
}

// ErrSharedContainer reports that Stop, CopyToContainer, or
// CopyFileFromContainer was called on a WithReuse handle. Those operations
// would disturb peers sharing the container. Use Shared to opt in, or
// Terminate to remove the shared container outright. Cleanup and
// TerminateContainer still skip reused handles even after Shared.
var ErrSharedContainer = errors.New("operation refused on a shared WithReuse handle")

// ErrCopySourceUnsupported reports a host source that is not a regular
// file or directory. CopyToContainer rejects source symlinks and symlink
// entries in source directories rather than copying devices, sockets, or
// FIFOs.
var ErrCopySourceUnsupported = errors.New("copy source must be a regular file or directory")

// ErrCopySourceTooLarge reports a source whose regular-file bytes exceed
// MaxCopyToContainerSize. The limit applies to a directory's total
// snapshot size as well as to a single file.
var ErrCopySourceTooLarge = errors.New("copy source exceeds size limit")

// ErrCopySourceTooManyEntries reports a source tree that exceeds
// MaxCopyToContainerEntries, including entries whose files are empty.
var ErrCopySourceTooManyEntries = errors.New("copy source has too many entries")

// ErrCopySourceTooDeep reports a source tree deeper than
// MaxCopyToContainerDepth.
var ErrCopySourceTooDeep = errors.New("copy source is too deep")

// ErrCopySourceMetadataTooLarge reports source path metadata that exceeds
// MaxCopyToContainerMetadataSize while the snapshot tree is traversed.
var ErrCopySourceMetadataTooLarge = errors.New("copy source metadata exceeds limit")

// ErrCopySourceChanged reports that a source disappeared or its opened
// handle changed while the private snapshot was being built. The triggering
// filesystem or context error remains available through errors.Is/errors.As.
var ErrCopySourceChanged = errors.New("copy source changed while staging")

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

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// commandContextError returns a cancellation/deadline error that must veto
// backend-derived success or absence verdicts. The error chain is checked in
// addition to ctx.Err so a joined CommandRunError remains authoritative
// even when a derived operation context has already been detached.
func commandContextError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		if err == nil {
			return ctxErr
		}
		if errors.Is(err, ctxErr) {
			return err
		}
		return errors.Join(err, ctxErr)
	}
	if isContextError(err) {
		return err
	}
	return nil
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
	if err == nil || isContextError(err) {
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
	if err == nil || isContextError(err) {
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
	if err == nil || isContextError(err) {
		return false
	}
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
	if err == ErrContainerNotFound || errors.Is(err, ErrSystemNotRunning) {
		return false, false
	}
	var cliErr *cli.CLIError
	if errors.As(err, &cliErr) {
		if !cliBinaryMatches(cliErr.Binary, eng.binary()) {
			return false, true
		}
		operation := commandOperation(cliErr.Args)
		if operation != "inspect" {
			// Probe or unrelated CLI branches joined after Classify must
			// not veto a confirmed inspect absence.
			return false, false
		}
		if target != "" && !sameCLITarget(commandTarget(cliErr.Args, operation), target) {
			return false, true
		}
		if eng.containerMissing(cliErr) {
			return true, false
		}
		return false, true
	}
	// Unknown non-CLI failures conflict with a confirmed absence so a
	// joined permission/config error cannot collapse into not-found.
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

// cliErrorLines returns normalized, non-empty stderr lines. Only wrappers
// verified for the selected binary and operation are removed. In
// particular, Docker exec never treats a workload's generic "Error: " line
// as backend evidence; arbitrary application/configuration prefixes remain
// visible and cannot satisfy an anchored backend matcher.
func cliErrorLines(err error) ([]string, bool) {
	var cliErr *cli.CLIError
	if !errors.As(err, &cliErr) {
		return nil, false
	}
	allowGenericError := genericErrorWrapperAllowed(cliErr)
	var lines []string
	for _, line := range strings.Split(cliErr.Stderr, "\n") {
		line = normalizeCLIErrorLine(line, cliErr, allowGenericError)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines, true
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
		return commandContextError(ctx, nil)
	}
	if contextErr := commandContextError(ctx, err); contextErr != nil {
		return contextErr
	}
	if eng == nil {
		return wrapNotFound(err)
	}
	if isAmbiguousApplicationError(eng, err) {
		return err
	}
	return cli.Classify(ctx, r, err, eng.probe())
}

func classifyErrorFor(ctx context.Context, r cli.Runner, err error, eng engine, operation string, targets ...string) error {
	if err == nil || eng == nil || operation == "" {
		return classifyError(ctx, r, err, eng)
	}
	if contextErr := commandContextError(ctx, err); contextErr != nil {
		return contextErr
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
