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
	if err == nil {
		return nil
	}
	var branches []cliErrorBranch
	stdoutByCLIError := make(map[*cli.CLIError]string)
	branchIndex := make(map[*cli.CLIError]int)
	var walk func(error)
	walk = func(cur error) {
		if cur == nil {
			return
		}
		if joined, ok := cur.(interface{ Unwrap() []error }); ok {
			for _, child := range joined.Unwrap() {
				walk(child)
			}
			return
		}
		if stdout, _, _ := cli.DiagnosticText(cur); stdout != "" {
			var owner *cli.CLIError
			if errors.As(cur, &owner) {
				stdoutByCLIError[owner] = stdout
			}
		}
		if cliErr, ok := cur.(*cli.CLIError); ok {
			// Empty Binary is the historical `container` executable, so a
			// Docker classifier never accepts an error without an explicit
			// Docker binary. ExecRunner always records the executable name.
			if backend != "" && !cliBinaryMatches(cliErr.Binary, backend) {
				return
			}
			operation := commandOperation(cliErr.Args)
			stdout := stdoutByCLIError[cliErr]
			cause := error(cliErr)
			if stdout != "" {
				cause = cli.WithStdout(cliErr, stdout)
			}
			if index, ok := branchIndex[cliErr]; ok {
				if stdout != "" {
					branches[index].stdout = stdout
					branches[index].cause = cause
				}
				return
			}
			branchIndex[cliErr] = len(branches)
			branches = append(branches, cliErrorBranch{
				ctx: cliErrorContext{
					err:       cliErr,
					operation: operation,
					target:    commandTarget(cliErr.Args, operation),
				},
				stdout: stdout,
				stderr: cliErr.Stderr,
				cause:  cause,
			})
			return
		}
		if wrapped, ok := cur.(interface{ Unwrap() error }); ok {
			walk(wrapped.Unwrap())
		}
	}
	walk(err)
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

func cliBinaryMatches(got, want string) bool {
	got = strings.TrimSpace(strings.ToLower(got))
	want = strings.TrimSpace(strings.ToLower(want))
	// want may be a caller-configured path, so compare executable names rather
	// than whole paths: the CLIError records the executable that ran, while the
	// engine names the backend's CLI, and a configured path carries both.
	if i := strings.LastIndexAny(want, `/\\`); i >= 0 {
		want = want[i+1:]
	}
	if got == "" {
		return want == "container"
	}
	if i := strings.LastIndexAny(got, `/\\`); i >= 0 {
		got = got[i+1:]
	}
	return got == want
}

func commandOperation(args []string) string {
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

func commandTarget(args []string, operation string) string {
	if len(args) == 0 {
		return ""
	}
	switch operation {
	case "run":
		for i := 0; i < len(args); i++ {
			if args[i] == "--name" && i+1 < len(args) {
				return args[i+1]
			}
			if value, ok := strings.CutPrefix(args[i], "--name="); ok {
				return value
			}
		}
	case "image inspect", "image pull":
		return firstPositional(args, 2, "--platform")
	case "inspect":
		return firstPositional(args, 1)
	case "exec":
		return firstPositional(args, 1, "--env-file", "--user", "--workdir")
	case "logs":
		return firstPositional(args, 1, "--tail", "--since", "-n")
	case "pull":
		return firstPositional(args, 1, "--platform")
	case "stop":
		return firstPositional(args, 1, "--time")
	case "delete", "rm":
		return firstPositional(args, 1)
	case "cp":
		for _, arg := range args[1:] {
			if target, path, ok := strings.Cut(arg, ":"); ok && target != "" && strings.HasPrefix(path, "/") {
				return target
			}
		}
	}
	return ""
}

func firstPositional(args []string, start int, valueOptions ...string) string {
	for i := start; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		}
		if strings.HasPrefix(arg, "-") {
			// Only options in this operation's emitted argv schema carry
			// values. Do not mistake a value for an unrelated flag as the
			// backend target.
			for _, option := range valueOptions {
				if arg == option {
					i++
					break
				}
			}
			continue
		}
		return arg
	}
	return ""
}

func branchLines(branch cliErrorBranch, includeStdout bool) (stderr, stdout []string) {
	allowGenericError := genericErrorWrapperAllowed(branch.ctx.err)
	for _, line := range strings.Split(branch.stderr, "\n") {
		line = normalizeCLIErrorLine(line, branch.ctx.err, allowGenericError)
		if line != "" {
			stderr = append(stderr, line)
		}
	}
	if includeStdout {
		for _, line := range strings.Split(branch.stdout, "\n") {
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
	operation := commandOperation(cliErr.Args)
	if cliBinaryMatches(cliErr.Binary, "container") {
		return true
	}
	if !cliBinaryMatches(cliErr.Binary, "docker") {
		return false
	}
	switch operation {
	case "inspect", "image inspect", "logs":
		return true
	default:
		return false
	}
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
	operation := commandOperation(cliErr.Args)
	if cliBinaryMatches(cliErr.Binary, "docker") {
		for _, prefix := range []string{"docker: ", "error response from daemon: "} {
			if strings.HasPrefix(line, prefix) {
				return prefix, true
			}
		}
		return "", false
	}
	if cliBinaryMatches(cliErr.Binary, "container") && appleContainerPrefixAllowed(operation) {
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
	case "run", "inspect", "image inspect", "stop", "delete", "rm":
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
	for _, raw := range strings.Split(branch.stderr, "\n") {
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

const maxAppleContainerizationDepth = 8

var appleContainerizationCodes = map[string]struct{}{
	"unknown":         {},
	"invalidargument": {},
	"internalerror":   {},
	"exists":          {},
	"notfound":        {},
	"cancelled":       {},
	"invalidstate":    {},
	"empty":           {},
	"timeout":         {},
	"unsupported":     {},
	"interrupted":     {},
}

type appleContainerizationError struct {
	code    string
	message string
	cause   *appleContainerizationError
}

// parseAppleContainerizationEnvelope parses ContainerizationError's
// CustomStringConvertible form, with or without the CLI's generic "Error: "
// wrapper. Swift embeds nested causes without escaping their quotes, so
// parsing follows the explicit "(cause: " marker and strips exactly the
// enclosing quote/parenthesis before recursing.
func parseAppleContainerizationEnvelope(line string) (appleContainerizationError, bool) {
	return parseAppleContainerizationEnvelopeAt(line, 0)
}

func parseAppleContainerizationEnvelopeAt(line string, depth int) (appleContainerizationError, bool) {
	if depth > maxAppleContainerizationDepth {
		return appleContainerizationError{}, false
	}
	line = strings.ToLower(strings.TrimSpace(line))
	line = strings.TrimSpace(strings.TrimPrefix(line, "error: "))
	colon := strings.Index(line, ": ")
	if colon <= 0 {
		return appleContainerizationError{}, false
	}
	code := strings.ToLower(line[:colon])
	if _, ok := appleContainerizationCodes[code]; !ok {
		return appleContainerizationError{}, false
	}
	rest := line[colon+2:]
	if !strings.HasPrefix(rest, `"`) {
		return appleContainerizationError{}, false
	}
	rest = strings.TrimPrefix(rest, `"`)

	const causeMarker = `" (cause: "`
	if marker := strings.Index(rest, causeMarker); marker >= 0 {
		causeText := rest[marker+len(causeMarker):]
		if !strings.HasSuffix(causeText, `")`) {
			return appleContainerizationError{}, false
		}
		causeText = strings.TrimSuffix(causeText, ")")
		causeText = strings.TrimSuffix(causeText, `"`)
		envelope := appleContainerizationError{code: code, message: rest[:marker]}
		if cause, ok := parseAppleContainerizationEnvelopeAt(causeText, depth+1); ok {
			envelope.cause = &cause
		}
		return envelope, true
	}
	if !strings.HasSuffix(rest, `"`) {
		return appleContainerizationError{}, false
	}
	return appleContainerizationError{
		code: code, message: strings.TrimSuffix(rest, `"`),
	}, true
}

func (e appleContainerizationError) walk(match func(code, message string) bool) bool {
	if match(e.code, e.message) {
		return true
	}
	return e.cause != nil && e.cause.walk(match)
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
		for _, raw := range strings.Split(branch.stderr, "\n") {
			line := trimCLIErrorWrapper(raw, branch.ctx.err, allowGenericError)
			if line == "" {
				continue
			}
			// Prefer the escape-aware typed parser for 1.3 CLI output; fall
			// back to the unescaped ContainerizationError grammar.
			if typed, ok := parseAppleTypedErrorLine(line); ok {
				if appleTypedErrorMatchesPath(typed, rootCode, matchRootMessage, code, match) {
					return true
				}
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

func appleTypedErrorMatchesPath(
	root appleTypedError,
	rootCode string,
	matchRootMessage func(string) bool,
	code string,
	match func(string) bool,
) bool {
	if rootCode != "" && (root.code != rootCode ||
		(matchRootMessage != nil && !matchRootMessage(strings.ToLower(root.message)))) {
		return false
	}
	for node := &root; node != nil; node = node.cause {
		if node.code == code && match(strings.ToLower(node.message)) {
			return true
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
	if err == nil || errors.Is(err, ErrContainerNotFound) {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			if hasNonCLIAmbiguousObjectText(child) {
				return true
			}
		}
		return false
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		child := wrapped.Unwrap()
		if _, joined := child.(interface{ Unwrap() []error }); joined {
			return hasNonCLIAmbiguousObjectText(child)
		}
	}
	if _, ok := err.(*cli.CLIError); ok {
		return false
	}
	var cliErr *cli.CLIError
	if errors.As(err, &cliErr) {
		return false
	}
	if ambiguousNoSuchLine(err.Error()) {
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return hasNonCLIAmbiguousObjectText(wrapped.Unwrap())
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
