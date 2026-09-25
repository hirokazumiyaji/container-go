package container

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// CLIError is a non-zero exit from the backend CLI. It aliases
// internal/cli.CLIError so callers can use errors.As without importing
// an internal package. The public four-field layout is retained for
// compatibility; classification may use additional bounded diagnostics.
type CLIError = cli.CLIError

// ErrSystemNotRunning reports that the selected container backend is
// not running. The classified error retains the backend-specific hint.
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

func newInspectTargetNotFound(target, detail string) error {
	return &inspectTargetNotFoundError{target: target, detail: detail}
}

func wrapInspectTargetNotFound(err error) error {
	return fmt.Errorf("%w: %w", ErrContainerNotFound, err)
}

// isNotFound reports whether a CLI failure means the container does not
// exist. It is retained for callers that do not have an engine context;
// the concrete backend and command still have to pass their own matcher.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrContainerNotFound) {
		branches := backendCLIErrorBranches(err, "")
		if hasNonCLIDefinitiveErrorText(err) {
			return false
		}
		for _, branch := range branches {
			if cli.IsDefinitiveNonLivenessError(branch.cause) {
				return false
			}
		}
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
	if errors.Is(err, ErrContainerNotFound) {
		branches := backendCLIErrorBranches(err, eng.binary())
		if hasNonCLIDefinitiveErrorText(err) {
			return false
		}
		for _, branch := range branches {
			if cli.IsDefinitiveNonLivenessError(branch.cause) {
				return false
			}
		}
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
	branches := backendCLIErrorBranches(err, eng.binary())
	selected := make([]cliErrorBranch, 0, len(branches))
	for _, branch := range branches {
		if branch.ctx.operation != operation {
			continue
		}
		if len(targets) > 0 && !branchTargetMatchesAny(branch, targets) {
			continue
		}
		selected = append(selected, branch)
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
