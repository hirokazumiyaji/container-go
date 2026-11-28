package cli

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"

	"github.com/hirokazumiyaji/container-go/internal/diagnostic"
)

// maxStderr bounds each diagnostic stream copied into a CLI failure.
const maxStderr = 64 * 1024

// stderrRedactionWindow is the extra raw stderr retained beyond the rendered
// tail. Structural redaction needs the prefix of a secret (for example
// "password=" or a private-key PEM header) that may sit just outside the
// rendered tail; the window stays small so Error rendering remains cheap.
const stderrRedactionWindow = 16 * 1024

// maxStderrContext is the raw stderr window retained in CLIError for
// classification and for rendering. formatCLIError redacts this window before
// trimming it to maxStderr.
const maxStderrContext = maxStderr + stderrRedactionWindow

// ErrSystemNotRunning reports that a backend CLI command returned a
// non-zero exit status and its follow-up liveness probe also failed.
// Missing or unlaunchable CLI binaries remain launch errors and are not
// classified as this value. When classification wraps this sentinel, the
// current implementation flattens the original CLI error into text (#104).
var ErrSystemNotRunning = errors.New("container backend is not running")

// ErrStreamSetup identifies a deterministic log-stream setup failure.
// Retrying the same runner and backend path cannot recover from it.
var ErrStreamSetup = errors.New("log stream setup failed")

// ErrStdinUnsupported reports that a Runner cannot attach a caller reader to
// the child's standard input. Runner stays a single-method interface for
// compatibility, so stdin is an optional extension rather than a new
// requirement on every existing test double and embedding wrapper.
var ErrStdinUnsupported = errors.New("runner does not support stdin")

// ErrStdinRead identifies a failure while reading the caller's stdin. The
// original reader error remains in the chain, so errors.Is(err, readerErr)
// still works.
var ErrStdinRead = errors.New("exec stdin read failed")

// StdinReadError attaches ErrStdinRead to the caller reader's failure while
// preserving the original error for errors.Is.
type StdinReadError struct {
	Err error
}

func (e *StdinReadError) Error() string {
	if e == nil || e.Err == nil {
		return ErrStdinRead.Error()
	}
	return fmt.Sprintf("%s: %v", ErrStdinRead, e.Err)
}

func (e *StdinReadError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (e *StdinReadError) Is(target error) bool {
	return target == ErrStdinRead
}

// StdinError returns the stdin read failure contained in err, if any.
func StdinError(err error) error {
	var target *StdinReadError
	if errors.As(err, &target) {
		return target
	}
	return nil
}

type SystemNotRunningError struct {
	hint      string
	original  error
	probe     error
	redactor  *Redactor
	rawSystem *SystemNotRunningError
}

func (e *SystemNotRunningError) Error() string {
	if e == nil {
		return "<nil>"
	}
	msg := fmt.Sprintf("%s: %s", ErrSystemNotRunning, e.hint)
	if e.original != nil {
		msg += fmt.Sprintf(" (underlying error: %v)", e.original)
	}
	if e.probe != nil {
		msg += fmt.Sprintf(" (probe error: %v)", e.probe)
	}
	return msg
}

func (e *SystemNotRunningError) Unwrap() []error {
	if e == nil {
		return nil
	}
	errs := []error{ErrSystemNotRunning}
	if e.original != nil {
		errs = append(errs, e.original)
	}
	if e.probe != nil {
		errs = append(errs, e.probe)
	}
	return errs
}

func (e *SystemNotRunningError) UnwrapRaw() error {
	if e == nil {
		return nil
	}
	return e.rawSystem
}

func (e *SystemNotRunningError) OriginalError() error {
	if e == nil {
		return nil
	}
	return e.original
}

func (e *SystemNotRunningError) ProbeError() error {
	if e == nil {
		return nil
	}
	return e.probe
}

// CLIError is a non-zero exit from a backend CLI.
//
// Stderr holds a bounded trailing window of the CLI's stderr. It stays raw so
// classifiers and RawError can inspect the original lines; Error redacts that
// window before trimming the rendered diagnostic.
//
// Keep this struct's four-field shape stable. Callers have historically
// used unkeyed CLIError literals; stdout is therefore carried by the
// internal diagnosticError wrapper below instead of being added here.
type CLIError struct {
	// Binary names the backend CLI this failure belongs to, e.g. "container"
	// or "docker". Classifiers compare against it, so a runner pointed at a
	// configured wrapper executable still reports the backend that owns it --
	// which also means the value names the backend rather than the executable
	// that ran. Empty means the historical default of "container".
	Binary   string
	Args     []string
	ExitCode int
	Stderr   string
}

// Error renders a safe diagnostic. Secret-shaped arguments and stderr
// values are redacted, and control characters are escaped so the result
// cannot alter a terminal. Use RawError only when the unredacted text is
// explicitly needed and the caller will keep it out of logs.
func (e *CLIError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return formatCLIError(e, NewRedactor())
}

// RawError returns the original command and stderr without redaction or
// control-character escaping. It is an explicit escape hatch for local
// debugging; do not write the result to CI logs or issue reports.
func (e *CLIError) RawError() string {
	if e == nil {
		return "<nil>"
	}
	bin := e.Binary
	if bin == "" {
		bin = "container"
	}
	msg := fmt.Sprintf("%s %s: exit code %d", bin, strings.Join(e.Args, " "), e.ExitCode)
	if e.Stderr != "" {
		msg += ": " + e.Stderr
	}
	return msg
}

func formatCLIError(e *CLIError, r *Redactor) string {
	bin := e.Binary
	if bin == "" {
		bin = "container"
	}
	args := r.Args(e.Args)
	msg := fmt.Sprintf("%s %s: exit code %d", r.Text(bin), strings.Join(args, " "), e.ExitCode)
	// Redact the retained window before trimming it. The stored window is
	// larger than the rendered tail so a password prefix or PEM header that
	// sits outside the tail still reaches the structural rules.
	if e.Stderr != "" {
		// Bound the work even for a CLIError constructed directly with a
		// large Stderr. The persisted window is already bounded.
		if stderr := redactedDiagnosticTail(r, truncateStderr(e.Stderr)); stderr != "" {
			msg += ": " + strings.TrimSpace(stderr)
		}
	}
	return msg
}

// redactedDiagnosticTail renders at most the trailing maxStderr bytes of a
// stored stderr window after redaction. Values too large for the streaming
// window fall back to a complete redaction so no raw text is emitted.
func redactedDiagnosticTail(r *Redactor, stderr string) string {
	stderr = redactOrphanedPEM(stderr)
	safe, err := r.RedactTail(strings.NewReader(stderr), maxStderr)
	if err == nil {
		return safe
	}
	safe = r.Text(stderr)
	if len(safe) > maxStderr {
		safe = safe[len(safe)-maxStderr:]
	}
	return safe
}

// redactedStderr renders a stored stderr window for a safe error clone. It
// applies the same orphaned-PEM handling as Error so errors.As cannot expose
// a key body whose BEGIN marker fell outside the retained window.
func redactedStderr(r *Redactor, stderr string) string {
	return r.Text(redactOrphanedPEM(truncateStderr(stderr)))
}

// redactOrphanedPEM hides a private-key body whose BEGIN marker was cut off
// by the retained window. The structural PEM rule needs both markers, so an
// END marker without a preceding BEGIN would otherwise leave the body intact.
func redactOrphanedPEM(s string) string {
	lower := strings.ToLower(s)
	end := strings.Index(lower, "-----end ")
	if end < 0 {
		return s
	}
	endLine := strings.IndexByte(s[end:], '\n')
	markerEnd := len(s)
	if endLine >= 0 {
		markerEnd = end + endLine
	}
	if !strings.Contains(lower[end:markerEnd], "private key") {
		return s
	}
	if strings.Contains(lower[:end], "-----begin ") {
		return s
	}
	if endLine < 0 {
		return "[REDACTED]"
	}
	return "[REDACTED]" + s[end+endLine:]
}

// diagnosticError keeps stdout available to classifiers without changing
// the public CLIError layout. Unwrap preserves errors.As/errors.Is behavior
// for the original CLIError.
type diagnosticError struct {
	cause  error
	cliErr *CLIError
	stdout string
}

func (e *diagnosticError) Error() string {
	return e.errorWith(NewRedactor())
}

func (e *diagnosticError) errorWith(r *Redactor) string {
	msg := formatCLIError(e.cliErr, r)
	if e.stdout == "" {
		return msg
	}
	if e.cliErr.Stderr != "" {
		msg += "; stdout: "
	} else {
		msg += ": "
	}
	return msg + strings.TrimSpace(r.Text(e.stdout))
}

func (e *diagnosticError) Unwrap() error { return e.cause }

// WithStdout attaches bounded stdout diagnostics to a CLIError. It is
// internal to this module so the public four-field CLIError remains source
// compatible with unkeyed literals.
func WithStdout(err *CLIError, stdout string) error {
	return withStdout(err, stdout)
}

func withStdout(err error, stdout string) error {
	if err == nil || stdout == "" {
		return err
	}
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		return err
	}
	var existing *diagnosticError
	if errors.As(err, &existing) && existing.cliErr == cliErr && existing.stdout != "" {
		return err
	}
	return &diagnosticError{
		cause:  err,
		cliErr: cliErr,
		stdout: truncateOutput(stdout),
	}
}

// DiagnosticText returns the diagnostic streams carried by a CLI failure.
// The bool reports whether err contains a CLIError. The stdout side channel
// is available even when the public CLIError remains unchanged.
func DiagnosticText(err error) (stdout, stderr string, ok bool) {
	if err == nil {
		return "", "", false
	}
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		return "", "", false
	}
	var diagnostic *diagnosticError
	if errors.As(err, &diagnostic) && diagnostic.cliErr == cliErr {
		stdout = diagnostic.stdout
	}
	return stdout, cliErr.Stderr, true
}

func commandError(ctx context.Context, bin, identity string, args []string, stdout, stderr []byte, err error) error {
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		exitCode := exitErr.ExitCode()
		if ctxErr := ctx.Err(); ctxErr != nil && exitCode < 0 {
			// A Unix signal has no usable application status. Preserve the
			// context contract while the lifecycle status records whether
			// local cancellation actually terminated the process.
			return commandContextError(bin, args, ctxErr)
		}
		cliErr := &CLIError{
			Binary:   identity,
			Args:     args,
			ExitCode: exitCode,
			Stderr:   truncateStderr(string(stderr)),
		}
		commandErr := WithStdout(cliErr, string(stdout))
		if ctxErr := ctx.Err(); ctxErr != nil {
			// The process did produce an exit status, but cancellation
			// raced with its completion. Keep both facts observable.
			return errors.Join(commandErr, ctxErr)
		}
		return commandErr
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		contextErr := commandContextError(bin, args, ctxErr)
		return errors.Join(contextErr, err)
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		return commandContextError(bin, args, err)
	}

	return err
}

// commandText renders a CLI invocation for a diagnostic. The binary and
// argv are structurally redacted so a sensitive flag value never reaches
// an error message, matching the rendering used for CLIError.
func commandText(bin string, args []string) string {
	redactor := NewRedactor()
	return fmt.Sprintf("%s %s", redactor.Text(bin), strings.Join(redactor.Args(args), " "))
}

func commandContextError(bin string, args []string, err error) error {
	return fmt.Errorf("%s: %w", commandText(bin, args), err)
}

// commandRunError is the historical helper name retained for tests that
// exercise cancellation races without a stdout side channel.
func commandRunError(ctx context.Context, bin string, args []string, stderr []byte, err error) error {
	return commandError(ctx, bin, bin, args, nil, stderr, err)
}

// truncateOutput bounds a diagnostic copy kept in CLIError while keeping
// both its beginning and its decisive tail. Backend diagnostics often put
// the actionable daemon/object error after a long prefix.
func truncateOutput(s string) string {
	if len(s) <= maxStderr {
		return s
	}
	const marker = "\n...[diagnostic truncated]...\n"
	available := maxStderr - len(marker)
	head := available / 2
	tail := available - head
	return s[:head] + marker + s[len(s)-tail:]
}

// truncateStderr bounds the raw stderr window kept in CLIError. It keeps one
// structural window beyond the rendered diagnostic so formatCLIError can see
// a secret prefix or PEM header that sits outside the final tail. A truncated
// window drops its incomplete first line, because a structural secret would
// no longer carry its prefix.
func truncateStderr(s string) string {
	if len(s) <= maxStderrContext {
		return s
	}
	return trimPartialLeadingLine(s[len(s)-maxStderrContext:])
}

// tailBuffer is a mutex-protected diagnostic ring buffer. It always reports
// complete writes and retains the last limit bytes without shifting the whole
// window on every write.
type tailBuffer struct {
	mu      sync.Mutex
	tail    *diagnostic.TailBuffer
	limit   int
	written int64
}

func newTailBuffer(limit int) *tailBuffer {
	return &tailBuffer{tail: diagnostic.NewTailBuffer(limit), limit: limit}
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.written += int64(len(p))
	return b.tail.Write(p)
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	data := b.tail.String()
	if b.written > int64(b.limit) {
		// The window starts mid-stream. Drop the incomplete first line so a
		// structural secret whose prefix was discarded cannot be emitted.
		data = trimPartialLeadingLine(data)
	}
	return data
}

// trimPartialLeadingLine removes an incomplete first line from a truncated
// diagnostic window. A window with no line break has no safe prefix left.
// Progress output overwrites lines with carriage returns, so both separators
// end a line.
func trimPartialLeadingLine(s string) string {
	i := strings.IndexAny(s, "\r\n")
	if i < 0 {
		return ""
	}
	i++
	if s[i-1] == '\r' && i < len(s) && s[i] == '\n' {
		i++
	}
	return s[i:]
}

func CollectNonCLIErrorBranches(err error) []error {
	var out []error
	var walk func(error)
	walk = func(current error) {
		if current == nil {
			return
		}
		if safe, ok := current.(*safeError); ok { //nolint:errorlint // walks the chain node by node; errors.Is/As would re-traverse it and cross safe facades
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
