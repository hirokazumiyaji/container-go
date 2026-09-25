package container

import (
	"errors"
	"strings"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// cliErrorLinePrefixes are backend-specific wrappers emitted by the
// supported CLIs around their structured error messages. A generic
// "Error: " prefix is intentionally absent: exec stderr may come from the
// workload, so it is admitted only for operations known to emit it.
var cliErrorLinePrefixes = []string{
	"docker: ",
	"container: ",
	"error response from daemon: ",
}

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

func matchingCLIErrorBranch(err error, backend, operation string, targets ...string) (cliErrorBranch, bool) {
	branches := matchingCLIErrorBranches(err, backend, operation, targets...)
	if len(branches) == 0 {
		return cliErrorBranch{}, false
	}
	return branches[0], true
}

func cliBinaryMatches(got, want string) bool {
	got = strings.TrimSpace(strings.ToLower(got))
	if got == "" {
		return strings.EqualFold(want, "container")
	}
	if i := strings.LastIndexAny(got, `/\\`); i >= 0 {
		got = got[i+1:]
	}
	return got == strings.ToLower(want)
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
		line = normalizeCLIErrorLine(line, allowGenericError)
		if line != "" {
			stderr = append(stderr, line)
		}
	}
	if includeStdout {
		for _, line := range strings.Split(branch.stdout, "\n") {
			line = normalizeCLIErrorLine(line, allowGenericError)
			if line != "" {
				stdout = append(stdout, line)
			}
		}
	}
	return stderr, stdout
}

func genericErrorWrapperAllowed(cliErr *cli.CLIError) bool {
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
func trimCLIErrorWrapper(line string, allowGenericError bool) string {
	line = strings.TrimSpace(line)
	for {
		lower := strings.ToLower(line)
		changed := false
		for _, prefix := range cliErrorLinePrefixes {
			if strings.HasPrefix(lower, prefix) {
				line = strings.TrimSpace(line[len(prefix):])
				changed = true
				break
			}
		}
		if allowGenericError && strings.HasPrefix(lower, "error: ") {
			line = strings.TrimSpace(line[len("error: "):])
			changed = true
		}
		if !changed {
			return line
		}
	}
}

func normalizeCLIErrorLine(line string, allowGenericError bool) string {
	return strings.ToLower(trimCLIErrorWrapper(line, allowGenericError))
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
		line := trimCLIErrorWrapper(raw, allowGenericError)
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
