package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

// Probe is the backend-specific liveness check Classify runs after a
// failure: a cheap CLI invocation plus the hint to show the user when
// it fails.
type Probe struct {
	Args   []string
	Hint   string
	Binary string
	// Timeout bounds the diagnostic liveness check. When zero, probeTimeout
	// (5 seconds) is used.
	Timeout time.Duration
	// Operation names the backend command represented by Args. It lets
	// classifiers ignore joined CLIError branches from other operations.
	Operation string
	// IsUnavailable reports whether a probe failure means that the
	// backend is not running. A nil predicate uses conservative
	// liveness-text matching; caller cancellation and other
	// non-liveness errors are rejected before this predicate is used.
	IsUnavailable func(error) bool
}

// withProbeOutput attaches captured probe streams to an error so
// configuration/liveness classifiers can see them. Prefer withStdout for
// new code that only needs the stdout side channel.
func withProbeOutput(err error, stdout, stderr []byte) error {
	if err == nil {
		return nil
	}
	if text := strings.TrimSpace(string(stdout)); text != "" {
		err = withStdout(err, text)
	}
	if text := strings.TrimSpace(string(stderr)); text != "" {
		err = errors.Join(err, errors.New(text))
	}
	return err
}

// probeTimeout bounds the diagnostic liveness check so a hung backend
// cannot stall error handling forever. Caller cancellation still
// aborts the probe via context propagation.
const probeTimeout = 5 * time.Second

func attachProbeStdout(err error, stdout string, probe Probe) error {
	if err == nil || stdout == "" {
		return err
	}
	operation := probe.Operation
	if operation == "" {
		operation = CommandOperation(probe.Args)
	}
	if operation == "" {
		return err
	}
	branch := matchingProbeCLIError(err, probe.Binary, operation)
	if branch == nil {
		return err
	}
	return errors.Join(err, WithStdout(branch, stdout))
}

func matchingProbeCLIError(err error, binary, operation string) *CLIError {
	if err == nil {
		return nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			if branch := matchingProbeCLIError(child, binary, operation); branch != nil {
				return branch
			}
		}
		return nil
	}
	if cliErr, ok := err.(*CLIError); ok {
		if (binary == "" || BinaryMatches(cliErr.Binary, binary)) && CommandOperation(cliErr.Args) == operation {
			return cliErr
		}
		return nil
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return matchingProbeCLIError(wrapped.Unwrap(), binary, operation)
	}
	return nil
}

func probeRelevantError(err error, probe Probe) error {
	if err == nil {
		return nil
	}
	operation := probe.Operation
	if operation == "" {
		operation = CommandOperation(probe.Args)
	}
	if operation == "" {
		return err
	}

	var branches []error
	for _, branch := range ClassificationBranches(err, probe.Binary) {
		if CommandOperation(branch.Error.Args) == operation {
			branches = append(branches, branch.Cause)
		}
	}
	if len(branches) == 0 {
		return err
	}
	return errors.Join(append(branches, CollectNonCLIErrorBranches(err)...)...)
}

func CollectNonCLIErrorBranches(err error) []error {
	var out []error
	var walk func(error)
	walk = func(current error) {
		if current == nil {
			return
		}
		if safe, ok := current.(*safeError); ok {
			for _, branch := range CollectNonCLIErrorBranches(safe.err) {
				out = append(out, WithRedactor(branch, safe.redactor))
			}
			return
		}
		var cliErr *CLIError
		if !errors.As(current, &cliErr) {
			out = append(out, current)
			return
		}
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			for _, child := range joined.Unwrap() {
				walk(child)
			}
		} else if wrapped, ok := current.(interface{ Unwrap() error }); ok {
			walk(wrapped.Unwrap())
		}
	}
	walk(err)
	return out
}

type ErrorBranch struct {
	Error  *CLIError
	Stdout string
	Cause  error
}

func collectCLIErrorBranches(err error, binary string) []ErrorBranch {
	var branches []ErrorBranch
	stdoutByError := make(map[*CLIError]string)
	index := make(map[*CLIError]int)
	var walk func(error)
	walk = func(current error) {
		if current == nil {
			return
		}
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			for _, child := range joined.Unwrap() {
				walk(child)
			}
			return
		}
		if stdout, _, _ := DiagnosticText(current); stdout != "" {
			var owner *CLIError
			if errors.As(current, &owner) {
				stdoutByError[owner] = stdout
			}
		}
		if wrapped, ok := current.(interface{ Unwrap() error }); ok {
			walk(wrapped.Unwrap())
			return
		}
		var cliErr *CLIError
		if errors.As(current, &cliErr) {
			if binary != "" && !BinaryMatches(cliErr.Binary, binary) {
				return
			}
			stdout := stdoutByError[cliErr]
			cause := error(cliErr)
			if stdout != "" {
				cause = WithStdout(cliErr, stdout)
			}
			if at, ok := index[cliErr]; ok {
				if stdout != "" {
					branches[at].Stdout, branches[at].Cause = stdout, cause
				}
				return
			}
			index[cliErr] = len(branches)
			branches = append(branches, ErrorBranch{Error: cliErr, Stdout: stdout, Cause: cause})
			return
		}
	}
	walk(err)
	return branches
}

func CommandOperation(args []string) string {
	if len(args) == 0 {
		return ""
	}
	if args[0] == "image" {
		if len(args) > 1 {
			return "image " + args[1]
		}
		return "image"
	}
	return args[0]
}

// BinaryMatches compares executable names; want must be non-empty.
func BinaryMatches(got, want string) bool {
	got = strings.TrimSpace(strings.ToLower(got))
	if got == "" {
		got = "container"
	}
	if i := strings.LastIndexAny(got, `/\\`); i >= 0 {
		got = got[i+1:]
	}
	return got == strings.ToLower(want)
}

// Classify augments a failed CLI call. When the backend-specific probe
// identifies a liveness failure, the result is marked with
// ErrSystemNotRunning. The original and probe errors remain in the
// result's error chain in every probe-failure case.
func Classify(ctx context.Context, r Runner, err error, probe Probe) error {
	if err == nil {
		return nil
	}
	// Check the caller before the CLIError gate. Launch errors are not
	// CLI failures, but cancellation still has to remain in their chain.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return withContextError(ctx, err)
	}
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		return withContextError(ctx, err)
	}
	// A timeout or signal reported by the operation itself is already a
	// known termination result. Probing the backend after it would turn a
	// useful operation error into a second, misleading operation.
	if IsOperationTimeoutError(err) {
		return withContextError(ctx, err)
	}

	timeout := probe.Timeout
	if timeout <= 0 {
		timeout = probeTimeout
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	probeStdout, probeStderr, probeErr := r.Run(probeCtx, probe.Args...)
	if probeErr != nil {
		// Runners may return stdout/stderr separately from the error.
		// Attach both before backend classifiers inspect the probe so
		// Windows npipe and Desktop diagnostics on stderr remain visible.
		probeErr = withProbeOutput(probeErr, probeStdout, probeStderr)
		probeErr = attachProbeStdout(probeErr, string(probeStdout), probe)
	}
	if probeErr == nil {
		// A runner can return successfully just as the caller cancels.
		// Do not lose that cancellation, but never invent a liveness
		// failure for it.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return errors.Join(err, ctxErr)
		}
		return err
	}

	// Cancellation takes precedence over every probe classification. It
	// may race with the runner returning, so check it before inspecting
	// the diagnostic and retain caller and probe context errors explicitly
	// when the runner did not wrap them.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return joinProbeFailure(ctx, probeCtx, err, probeErr)
	}
	probeForClassification := probeRelevantError(probeErr, probe)
	if isNonLivenessError(err) &&
		(probe.IsUnavailable == nil || isDefinitiveNonLivenessError(err)) {
		// The command already identified a precise configuration,
		// permission, TLS, or other client-side failure. A failed probe
		// cannot replace that diagnosis with daemon-down.
		return joinProbeFailure(ctx, probeCtx, err, probeErr)
	}
	// Configuration/authentication diagnostics can be attached to any
	// joined probe branch. Check them before a liveness-looking branch or
	// an explicit backend predicate can add the sentinel.
	if IsProbeConfigurationError(probeForClassification) {
		return joinProbeFailure(ctx, probeCtx, err, probeErr)
	}
	// A returned deadline is the bounded probe's liveness evidence when
	// it is the only cause. A joined permission, configuration, or
	// cancellation cause is a client-side veto and must remain a joined
	// ordinary failure instead.
	if isReturnedProbeTimeout(probeForClassification) && !probeVetoError(probeForClassification) {
		return classifySystemNotRunning(ctx, probeCtx, err, probeErr, probe.Hint)
	}
	// A timeout belonging to the bounded probe is evidence that the
	// backend did not answer, unless the runner reported a distinct
	// non-liveness error. A caller deadline was handled above.
	if isProbeNonLiveness(probeCtx, probeForClassification) {
		return joinProbeFailure(ctx, probeCtx, err, probeErr)
	}
	if probeCtx.Err() == context.DeadlineExceeded {
		if isDefinitiveNonLivenessError(probeForClassification) {
			return joinProbeFailure(ctx, probeCtx, err, probeErr)
		}
		return classifySystemNotRunning(ctx, probeCtx, err, probeErr, probe.Hint)
	}

	// Check immediately before invoking a backend predicate. The
	// predicate may be user-provided and can itself take time.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return joinProbeFailure(ctx, probeCtx, err, probeErr)
	}
	unavailable := probeUnavailable(probe, probeForClassification)
	// Check again after the predicate. In particular, a predicate that
	// cancels the caller and then reports liveness must not win the
	// race and add ErrSystemNotRunning.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return joinProbeFailure(ctx, probeCtx, err, probeErr)
	}
	if unavailable {
		return classifySystemNotRunning(ctx, probeCtx, err, probeErr, probe.Hint)
	}

	// A failed probe is not proof that the backend is down. Keep both
	// errors so callers can inspect the original diagnostic.
	return joinProbeFailure(ctx, probeCtx, err, probeErr)
}

func joinProbeFailure(ctx, probeCtx context.Context, original, probeErr error) error {
	joined := withContextError(probeCtx, errors.Join(original, probeErr))
	return withContextError(ctx, joined)
}

func withContextError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
		return errors.Join(err, ctxErr)
	}
	return err
}

func classifySystemNotRunning(ctx, probeCtx context.Context, original, probeErr error, hint string) error {
	// Keep this check at the point where the sentinel is added. Context
	// cancellation can race with all of the preceding diagnostics checks.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return joinProbeFailure(ctx, probeCtx, original, probeErr)
	}
	classified := errors.Join(
		fmt.Errorf("%w: %s", ErrSystemNotRunning, hint),
		withContextError(probeCtx, errors.Join(original, probeErr)),
	)
	// Building the joined error can itself race with cancellation. Do
	// not return a daemon-down result if cancellation was observed while
	// it was being assembled.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return joinProbeFailure(ctx, probeCtx, original, probeErr)
	}
	return classified
}

// IsOperationTimeoutError reports whether err has structured timeout,
// cancellation, or signal evidence. Application stderr and argv are
// deliberately not inspected: a command may legitimately print either
// phrase while exiting as an ordinary application result.
func IsOperationTimeoutError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var timeoutErr interface{ Timeout() bool }
	if errors.As(err, &timeoutErr) && timeoutErr.Timeout() {
		return true
	}
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		return false
	}
	// A signal exit has no application status to report and cannot benefit
	// from a liveness probe.
	return cliErr.ExitCode < 0
}

func isProbeNonLiveness(probeCtx context.Context, err error) bool {
	if !isNonLivenessError(err) {
		return false
	}
	// A context deadline returned by the bounded probe is the timeout
	// signal itself, not a reason to suppress the liveness sentinel.
	// Keep a simultaneous client-side diagnostic non-liveness, though.
	if errors.Is(err, context.DeadlineExceeded) {
		return probeVetoError(err)
	}
	return true
}

func isReturnedProbeTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var timeoutErr interface{ Timeout() bool }
	return errors.As(err, &timeoutErr) && timeoutErr.Timeout()
}

func probeVetoError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, os.ErrPermission) ||
		errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrInvalid) {
		return true
	}
	if IsProbeConfigurationError(err) {
		return true
	}
	s := strings.ToLower(classificationText(err))
	// Cancellation diagnostics must veto before deadline phrasing is
	// stripped: a probe that prints "context canceled" and is also joined
	// with DeadlineExceeded is a client-side abort, not liveness evidence.
	if containsCancellationDiagnostic(s) && !isOnlyDeadlineDiagnostic(s) {
		return true
	}
	for _, fragment := range []string{
		"context deadline exceeded",
		"deadline exceeded",
	} {
		s = strings.ReplaceAll(s, fragment, "")
	}
	return containsPermissionDiagnostic(s) || containsConfigurationDiagnostic(s) ||
		containsAuthenticationDiagnostic(s) || containsTransportDiagnostic(s) ||
		containsContextDiagnostic(s) || containsInvalidOptionDiagnostic(s)
}

// isOnlyDeadlineDiagnostic reports text whose only cancellation/timeout
// wording is a deadline. Pure probe timeouts remain liveness evidence.
func isOnlyDeadlineDiagnostic(s string) bool {
	trimmed := s
	for _, fragment := range []string{
		"context deadline exceeded",
		"deadline exceeded",
	} {
		trimmed = strings.ReplaceAll(trimmed, fragment, "")
	}
	return !containsCancellationDiagnostic(trimmed)
}

func probeUnavailable(probe Probe, err error) bool {
	if isNonLivenessError(err) {
		return false
	}
	if probe.IsUnavailable != nil {
		return probe.IsUnavailable(err)
	}
	return defaultProbeUnavailable(err)
}

func defaultProbeUnavailable(err error) bool {
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		return false
	}
	return defaultProbeUnavailableText(classificationText(err))
}

func defaultProbeUnavailableText(s string) bool {
	s = strings.ToLower(s)
	for _, fragment := range []string{
		"cannot connect to the docker daemon",
		"cannot connect to the container backend",
		"connection refused",
		"xpc connection",
		"system is not running",
		"apiserver is not running",
		"not registered with launchd",
		"daemon is not running",
		"is the docker daemon running",
		"failed to connect to the docker daemon",
	} {
		if strings.Contains(s, fragment) {
			return true
		}
	}
	return false
}

var endpointURI = regexp.MustCompile(`(?i)\b(unix|tcp|ssh|npipe|http|https)://[^\s"'<>]+`)

func diagnosticText(err error) string {
	if err == nil {
		return ""
	}
	var parts []string
	var walk func(error)
	walk = func(cur error) {
		if cur == nil {
			return
		}
		if diagnostic, ok := cur.(*diagnosticError); ok {
			if diagnostic.stdout != "" {
				parts = append(parts, diagnostic.stdout)
			}
			walk(diagnostic.cause)
			return
		}
		if joined, ok := cur.(interface{ Unwrap() []error }); ok {
			for _, child := range joined.Unwrap() {
				walk(child)
			}
			return
		}
		if wrapped, ok := cur.(interface{ Unwrap() error }); ok {
			if child := wrapped.Unwrap(); child != nil {
				if _, joined := child.(interface{ Unwrap() []error }); joined {
					walk(child)
					return
				}
			}
		}
		var cliErr *CLIError
		if errors.As(cur, &cliErr) {
			stdout, stderr, ok := DiagnosticText(cur)
			if ok {
				if stdout != "" {
					parts = append(parts, stdout)
				}
				if stderr != "" {
					parts = append(parts, stderr)
				}
			}
			return
		}
		if text := cur.Error(); text != "" {
			parts = append(parts, text)
		}
		if unwrapped, ok := cur.(interface{ Unwrap() error }); ok {
			walk(unwrapped.Unwrap())
		}
	}
	walk(err)
	return strings.Join(parts, "\n")
}

func classificationText(err error) string {
	return endpointURI.ReplaceAllString(diagnosticText(err), " ")
}

// IsDefinitiveNonLivenessError reports a concrete client-side failure
// that must veto backend object-absence and liveness classification. It
// deliberately excludes ambiguous object wording so a verified backend
// absence can still be recognized on its own.
func IsDefinitiveNonLivenessError(err error) bool {
	return isDefinitiveNonLivenessError(err)
}

// isNonLivenessError identifies failures that must not be relabeled
// as a stopped backend, even when the probe happens to fail too.
func isNonLivenessError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, os.ErrPermission) || errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrInvalid) {
		return true
	}
	var timeoutErr interface{ Timeout() bool }
	if errors.As(err, &timeoutErr) && timeoutErr.Timeout() {
		return true
	}
	if IsProbeConfigurationError(err) {
		return true
	}
	var cliErr *CLIError
	if errors.As(err, &cliErr) && (cliErr.ExitCode == 126 || cliErr.ExitCode == 127) {
		return true
	}
	return containsNonLivenessText(strings.ToLower(classificationText(err)))
}

// IsProbeConfigurationError reports concrete transport, authentication,
// and endpoint-configuration diagnostics that must not be classified as a
// stopped backend. It intentionally matches diagnostic phrases, not bare
// words such as "config", "credential", or "forbidden".
func IsProbeConfigurationError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, os.ErrPermission) ||
		errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrInvalid) {
		return true
	}
	s := strings.ToLower(classificationText(err))
	return containsConfigurationDiagnostic(s) ||
		containsAuthenticationDiagnostic(s) ||
		containsPermissionDiagnostic(s) ||
		containsTransportDiagnostic(s) ||
		containsContextDiagnostic(s) ||
		containsInvalidOptionDiagnostic(s)
}

func containsConfigurationDiagnostic(s string) bool {
	for _, fragment := range []string{
		"invalid config",
		"invalid configuration",
		"configuration is invalid",
		"proxy configuration",
		"config file",
		"config directory",
		"config error",
		"configuration file",
		"configuration directory",
		"configuration error",
		"failed to load config",
		"unable to load config",
		"cannot load config",
		"config not found",
		"missing config",
	} {
		if strings.Contains(s, fragment) {
			return true
		}
	}
	return false
}

func containsContextDiagnostic(s string) bool {
	for _, fragment := range []string{
		"invalid context",
		"unknown context",
		"no such context",
		"context not found",
		"context does not exist",
	} {
		if strings.Contains(s, fragment) {
			return true
		}
	}
	return false
}

func containsAuthenticationDiagnostic(s string) bool {
	for _, fragment := range []string{
		"authentication required",
		"authentication failed",
		"error getting credentials",
		"credentials not found",
		"credential helper",
		"401 unauthorized",
		"403 forbidden",
		"unauthorized: access denied",
		"forbidden: access denied",
		"access forbidden",
		"permission forbidden",
	} {
		if strings.Contains(s, fragment) {
			return true
		}
	}
	return false
}

func containsPermissionDiagnostic(s string) bool {
	for _, fragment := range []string{
		"permission denied",
		"insufficient permissions",
		"operation not permitted",
		"eacces",
		"eperm",
		"access denied",
		"not permitted",
	} {
		if strings.Contains(s, fragment) {
			return true
		}
	}
	return false
}

func containsTransportDiagnostic(s string) bool {
	for _, fragment := range []string{
		"tls handshake",
		"tls:",
		"tls alert",
		"tls record",
		"tls version",
		"x509:",
		"x509 error",
		"x509 certificate",
		"certificate signed by unknown authority",
		"certificate verification failed",
		"certificate verify failed",
		"certificate has expired",
		"certificate is not trusted",
		"certificate is invalid",
		"unable to verify certificate",
		"failed to verify certificate",
		"unknown authority",
		"handshake failure",
		"http response to https",
		"ssh handshake",
		"ssh:",
		"proxyconnect",
		"proxy error",
		"proxy:",
	} {
		if strings.Contains(s, fragment) {
			return true
		}
	}
	return false
}

func containsInvalidOptionDiagnostic(s string) bool {
	for _, fragment := range []string{
		"unknown flag",
		"unknown shorthand flag",
		"flag needs an argument",
		"flag provided but not defined",
		"invalid flag",
		"unknown option",
		"no such option",
		"invalid option",
		"invalid argument",
	} {
		if strings.Contains(s, fragment) {
			return true
		}
	}
	return false
}

func isDefinitiveNonLivenessError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, os.ErrPermission) || errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrInvalid) {
		return true
	}
	var timeoutErr interface{ Timeout() bool }
	if errors.As(err, &timeoutErr) && timeoutErr.Timeout() {
		return true
	}
	if IsProbeConfigurationError(err) {
		return true
	}
	if containsCancellationDiagnostic(strings.ToLower(classificationText(err))) {
		return true
	}
	var cliErr *CLIError
	return errors.As(err, &cliErr) && (cliErr.ExitCode == 126 || cliErr.ExitCode == 127)
}

func containsCancellationDiagnostic(s string) bool {
	for _, fragment := range []string{
		"context canceled",
		"context cancelled",
		"deadline exceeded",
		"operation canceled",
		"operation cancelled",
		"command canceled",
		"command cancelled",
		"request canceled",
		"request cancelled",
	} {
		if strings.Contains(s, fragment) {
			return true
		}
	}
	return false
}

func containsNonLivenessText(s string) bool {
	if containsPermissionDiagnostic(s) || containsConfigurationDiagnostic(s) ||
		containsContextDiagnostic(s) || containsAuthenticationDiagnostic(s) ||
		containsTransportDiagnostic(s) || containsInvalidOptionDiagnostic(s) ||
		containsCancellationDiagnostic(s) {
		return true
	}
	for _, fragment := range []string{
		"operation canceled",
		"operation cancelled",
		"context canceled",
		"context cancelled",
		"deadline exceeded",
		"container not found",
		"image not found",
		"no such container",
		"no such object",
		"no such image",
	} {
		if strings.Contains(s, fragment) {
			return true
		}
	}
	return false
}
