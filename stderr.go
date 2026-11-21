package container

import (
	"errors"
	"strings"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// cliErrorLinePrefixes lists Docker/daemon wrappers that any Docker
// operation may strip. The Apple "container: " prefix is handled
// separately by cliErrorLinePrefix so Docker workload text cannot spoof it.

// cliErrorContext is the small, structured part of a CLI failure that a
// backend matcher is allowed to use. In particular, stderr by itself is
// not enough: a process running in a container can print the same words as
// the backend. The command and executable are therefore part of the
// classification contract.
type cliErrorContext struct {
	err       *cli.CLIError
	operation string
	target    string
}

type cliErrorBranch struct {
	ctx    cliErrorContext
	stdout string
	stderr string
	cause  error
}

// backendCLIErrorBranches returns every CLIError branch in an error tree
// that belongs to backend. Stdout is keyed by the CLIError that owns it:
// joined parents and siblings never lend their diagnostics to another
// branch.
func backendCLIErrorBranches(err error, backend string) []cliErrorBranch {
	var branches []cliErrorBranch
	for _, branch := range cli.ClassificationBranches(err, backend) {
		operation := cli.CommandOperation(branch.Error.Args)
		branches = append(branches, cliErrorBranch{
			ctx:    cliErrorContext{err: branch.Error, operation: operation, target: commandTarget(branch.Error.Args, operation)},
			stdout: branch.Stdout, stderr: branch.Error.Stderr, cause: branch.Cause,
		})
	}
	return branches
}

func matchingCLIErrorBranches(err error, backend, operation string, targets ...string) []cliErrorBranch {
	branches := backendCLIErrorBranches(err, backend)
	operationBranches := make([]cliErrorBranch, 0, len(branches))
	hasTarget := false
	for _, target := range targets {
		if target != "" {
			hasTarget = true
			break
		}
	}
	for _, branch := range branches {
		if branch.ctx.operation == operation {
			operationBranches = append(operationBranches, branch)
		}
	}
	if !hasTarget {
		return operationBranches
	}

	targetless := make([]cliErrorBranch, 0, len(operationBranches))
	for _, branch := range operationBranches {
		if strings.TrimSpace(branch.ctx.target) == "" {
			targetless = append(targetless, branch)
		}
	}
	for _, target := range targets {
		if target == "" {
			continue
		}
		exact := make([]cliErrorBranch, 0, len(operationBranches))
		for _, branch := range operationBranches {
			if strings.TrimSpace(branch.ctx.target) != "" && sameCLITarget(branch.ctx.target, target) {
				exact = append(exact, branch)
			}
		}
		if len(exact) > 0 {
			return exact
		}
	}
	return targetless
}

func exactBranchTarget(branch cliErrorBranch, target string) bool {
	if strings.TrimSpace(branch.ctx.target) == "" {
		return false
	}
	return target == "" || sameCLITarget(branch.ctx.target, target)
}

func exactImageTarget(branch cliErrorBranch, target string) bool {
	if strings.TrimSpace(branch.ctx.target) == "" {
		return false
	}
	return target == "" || strings.TrimSpace(branch.ctx.target) == strings.TrimSpace(target)
}

func ambiguousBranchTargets(branches []cliErrorBranch) bool {
	seen := make(map[string]struct{}, len(branches))
	for _, branch := range branches {
		seen[strings.ToLower(strings.TrimSpace(branch.ctx.target))] = struct{}{}
	}
	return len(seen) > 1
}

func matchingCLIErrorBranch(err error, backend, operation string, targets ...string) (cliErrorBranch, bool) {
	branches := matchingCLIErrorBranches(err, backend, operation, targets...)
	if len(branches) == 0 {
		return cliErrorBranch{}, false
	}
	return branches[0], true
}

// backendCLIError returns the first CLIError branch that belongs to backend.
// Context cancellation alone is never treated as a backend diagnostic.
func backendCLIError(err error, backend string) (cliErrorContext, bool) {
	if isContextError(err) {
		return cliErrorContext{}, false
	}
	branches := backendCLIErrorBranches(err, backend)
	if len(branches) == 0 {
		return cliErrorContext{}, false
	}
	return branches[0].ctx, true
}

func hasParsedCLITarget(ctx cliErrorContext) bool {
	return strings.TrimSpace(ctx.target) != ""
}

func branchLines(branch cliErrorBranch, includeStdout bool) (stderr, stdout []string) {
	allowGenericError := genericErrorWrapperAllowed(branch.ctx.err)
	for _, line := range splitCLIErrorLines(branch.stderr) {
		line = normalizeCLIErrorLine(line, branch.ctx.err, allowGenericError)
		if line != "" {
			stderr = append(stderr, line)
		}
	}
	if includeStdout {
		for _, line := range splitCLIErrorLines(branch.stdout) {
			line = normalizeCLIErrorLine(line, branch.ctx.err, allowGenericError)
			if line != "" {
				stdout = append(stdout, line)
			}
		}
	}
	return stderr, stdout
}

func genericErrorWrapperAllowed(cliErr *cli.CLIError) bool {
	if cliErr == nil {
		return false
	}
	operation := cli.CommandOperation(cliErr.Args)
	if cli.BinaryMatches(cliErr.Binary, "container") {
		return true
	}
	if !cli.BinaryMatches(cliErr.Binary, "docker") {
		return false
	}
	switch operation {
	case "inspect", "image inspect", "logs":
		return true
	default:
		return false
	}
}

// splitCLIErrorLines splits backend diagnostics on both newline and
// carriage return. Progress display overwrites its line with carriage returns,
// so the decisive error line can follow one without a newline.
func splitCLIErrorLines(text string) []string {
	return strings.FieldsFunc(text, func(r rune) bool { return r == '\r' || r == '\n' })
}

// trimCLIErrorWrapper removes only verified CLI wrappers while preserving
// the case of the diagnostic payload. Image target comparisons need that
// case; backend phrase matching can use normalizeCLIErrorLine below.
func trimCLIErrorWrapper(line string, cliErr *cli.CLIError, allowGenericError bool) string {
	// Streamed Docker diagnostics can prefix the decisive line with a
	// bounded null-byte flood; trim those before wrapper matching.
	line = strings.Trim(line, "\x00 \t\r\n")
	for {
		lower := strings.ToLower(line)
		prefix, ok := cliErrorLinePrefix(lower, cliErr)
		if !ok || !strings.HasPrefix(lower, prefix) {
			break
		}
		line = strings.Trim(line[len(prefix):], "\x00 \t\r\n")
	}
	lower := strings.ToLower(line)
	if allowGenericError && strings.HasPrefix(lower, "error: ") {
		line = strings.Trim(line[len("error: "):], "\x00 \t\r\n")
	}
	return line
}

func normalizeCLIErrorLine(line string, cliErr *cli.CLIError, allowGenericError bool) string {
	return strings.ToLower(trimCLIErrorWrapper(line, cliErr, allowGenericError))
}

// cliErrorLinePrefix returns a backend- and operation-scoped wrapper that
// may be stripped from a diagnostic line. Docker never strips Apple's
// "container: " prefix, and Apple exec/logs never treat that prefix as
// backend evidence on its own.
func cliErrorLinePrefix(line string, cliErr *cli.CLIError) (string, bool) {
	if cliErr == nil {
		return "", false
	}
	operation := cli.CommandOperation(cliErr.Args)
	if cli.BinaryMatches(cliErr.Binary, "docker") {
		for _, prefix := range []string{"docker: ", "error response from daemon: "} {
			if strings.HasPrefix(line, prefix) {
				return prefix, true
			}
		}
		return "", false
	}
	if cli.BinaryMatches(cliErr.Binary, "container") && appleContainerPrefixAllowed(operation) {
		if strings.HasPrefix(line, "container: ") {
			return "container: ", true
		}
	}
	return "", false
}

func appleContainerPrefixAllowed(operation string) bool {
	// Exec and logs can carry workload output, so their "container: " prefix
	// is not by itself evidence of an Apple backend diagnostic.
	switch operation {
	case "run", "inspect", "image inspect", "stop", "delete", "rm", "start", "kill":
		return true
	default:
		return false
	}
}

func hasBranchLine(branch cliErrorBranch, match func(string) bool) bool {
	stderr, _ := branchLines(branch, false)
	for _, line := range stderr {
		if match(line) {
			return true
		}
	}
	return false
}

func hasBranchImageLine(branch cliErrorBranch, prefix, target string, exact bool) bool {
	allowGenericError := genericErrorWrapperAllowed(branch.ctx.err)
	for _, raw := range splitCLIErrorLines(branch.stderr) {
		line := trimCLIErrorWrapper(raw, branch.ctx.err, allowGenericError)
		lower := strings.ToLower(line)
		if !strings.HasPrefix(lower, prefix) {
			continue
		}
		rest := line[len(prefix):]
		if exact {
			if cliTargetListExactMatches(rest, target) {
				return true
			}
		} else if cliTargetListMatches(rest, target) {
			return true
		}
	}
	return false
}

// parseAppleContainerizationEnvelope returns the shared parser's tree with
// codes and messages lowercased, matching the historical envelope contract
// used by wording-based matchers.
func parseAppleContainerizationEnvelope(line string) (appleContainerizationError, bool) {
	envelope, ok := parseAppleContainerizationError(line)
	if !ok {
		return appleContainerizationError{}, false
	}
	return lowercaseAppleContainerizationError(envelope), true
}

func hasAppleContainerizationError(err error, code string, match func(string) bool) bool {
	return hasAppleContainerizationErrorPath(err, "", nil, code, match)
}

func hasAppleContainerizationErrorPath(
	err error,
	rootCode string,
	matchRootMessage func(string) bool,
	code string,
	match func(string) bool,
) bool {
	for _, branch := range backendCLIErrorBranches(err, "container") {
		allowGenericError := genericErrorWrapperAllowed(branch.ctx.err)
		for _, raw := range splitCLIErrorLines(branch.stderr) {
			line := trimCLIErrorWrapper(raw, branch.ctx.err, allowGenericError)
			if line == "" {
				continue
			}
			envelope, ok := parseAppleContainerizationEnvelope(line)
			if !ok {
				continue
			}
			if rootCode != "" && (envelope.code != rootCode ||
				(matchRootMessage != nil && !matchRootMessage(envelope.message))) {
				continue
			}
			if envelope.walk(func(gotCode, message string) bool {
				return gotCode == code && match(message)
			}) {
				return true
			}
		}
	}
	return false
}

func sameCLITarget(got, want string) bool {
	got = strings.Trim(strings.TrimSpace(got), `"'`)
	want = strings.Trim(strings.TrimSpace(want), `"'`)
	if want == "" {
		return got != ""
	}
	return strings.EqualFold(got, want)
}

// cliTargetListMatches accepts one or more missing IDs, but rejects an
// arbitrary explanatory suffix. Container identifiers are matched
// case-insensitively for compatibility with backend display formatting.
func cliTargetListMatches(rest, want string) bool {
	return cliTargetListMatchesWith(rest, want, strings.EqualFold)
}

// cliTargetListExactMatches is used for image references, whose target
// spelling is case-sensitive.
func cliTargetListExactMatches(rest, want string) bool {
	return cliTargetListMatchesWith(rest, want, func(a, b string) bool { return a == b })
}

func cliTargetListMatchesWith(rest, want string, equal func(string, string) bool) bool {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return false
	}
	found := false
	want = strings.Trim(want, `"'`)
	for _, part := range strings.Split(rest, ",") {
		part = strings.Trim(strings.TrimSpace(part), `"'`)
		if part == "" || strings.ContainsAny(part, " \t\r\n") {
			return false
		}
		if want == "" || equal(part, want) {
			found = true
		}
	}
	return found
}

// ambiguousNoSuchLine recognizes application wording that resembles an
// object-absence message without satisfying the selected backend's
// command-specific matcher.
func ambiguousNoSuchLine(line string) bool {
	line = strings.ToLower(line)
	for _, fragment := range []string{
		"container not found",
		"no such container",
		"no such object",
		"image not found",
		"no such image",
	} {
		if strings.Contains(line, fragment) {
			return true
		}
	}
	return false
}

func hasNonCLIAmbiguousObjectText(err error) bool {
	if errors.Is(err, ErrContainerNotFound) {
		return false
	}
	for _, branch := range cli.CollectNonCLIErrorBranches(err) {
		if !errors.Is(branch, ErrContainerNotFound) && ambiguousNoSuchLine(branch.Error()) {
			return true
		}
	}
	return false
}

func isAmbiguousApplicationError(eng engine, err error) bool {
	if eng == nil || err == nil {
		return false
	}
	branches := backendCLIErrorBranches(err, eng.binary())
	if len(branches) == 0 {
		return hasNonCLIAmbiguousObjectText(err)
	}
	for _, branch := range branches {
		verified := eng.containerMissing(branch.ctx.err) || eng.imageMissing(branch.ctx.err) || eng.nameConflict(branch.ctx.err)
		if verified || createRaceMissing(branch.ctx.err) {
			continue
		}
		if hasBranchLine(branch, ambiguousNoSuchLine) {
			return true
		}
	}
	return hasNonCLIAmbiguousObjectText(err)
}
