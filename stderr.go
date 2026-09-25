package container

import (
	"errors"
	"strings"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// cliErrorContext is the small, structured part of a CLI failure that a
// backend matcher is allowed to use.  In particular, stderr by itself is
// not enough: a process running in a container can print the same words as
// the backend.  The command and executable are therefore part of the
// classification contract.
type cliErrorContext struct {
	err       *cli.CLIError
	operation string
	target    string
}

// backendCLIError returns the first CLIError in err when it can belong to
// backend. CLIError defines an empty Binary as the historical `container`
// executable, so a Docker classifier never accepts an error without an
// explicit Docker binary. ExecRunner always records the executable name.
func backendCLIError(err error, backend string) (cliErrorContext, bool) {
	if isContextError(err) {
		return cliErrorContext{}, false
	}
	var cliErr *cli.CLIError
	if !errors.As(err, &cliErr) {
		return cliErrorContext{}, false
	}
	if !cliBinaryMatches(cliErr.Binary, backend) {
		return cliErrorContext{}, false
	}
	operation := commandOperation(cliErr.Args)
	return cliErrorContext{
		err:       cliErr,
		operation: operation,
		target:    commandTarget(cliErr.Args, operation),
	}, true
}

// backendCLIErrorText validates a matching backend command before returning
// the complete wrapped/joined error text. Probe predicates use this instead
// of reading only the first CLIError.Stderr so instrumentation branches are
// not silently discarded.
func backendCLIErrorText(err error, backend, operation string) (string, bool) {
	ctx, ok := backendCLIError(err, backend)
	if !ok || ctx.operation != operation {
		return "", false
	}
	return strings.ToLower(err.Error()), true
}

func hasParsedCLITarget(ctx cliErrorContext) bool {
	return strings.TrimSpace(ctx.target) != ""
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
	case "image inspect":
		return firstPositional(args, 2, "--platform")
	case "inspect":
		return firstPositional(args, 1)
	case "exec":
		return firstPositional(args, 1, "--env-file", "--user", "--workdir")
	case "logs":
		return firstPositional(args, 1, "--tail", "--since", "-n")
	case "stop":
		return firstPositional(args, 1, "--time")
	case "delete", "rm":
		return firstPositional(args, 1)
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

func normalizeCLIErrorLine(line string, cliErr *cli.CLIError, allowGenericError bool) string {
	line = strings.ToLower(strings.TrimSpace(line))
	for {
		prefix, ok := cliErrorLinePrefix(line, cliErr)
		if !ok || !strings.HasPrefix(line, prefix) {
			break
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, prefix))
	}
	if allowGenericError && strings.HasPrefix(line, "error: ") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "error: "))
	}
	return line
}

func cliErrorLinePrefix(line string, cliErr *cli.CLIError) (string, bool) {
	operation := commandOperation(cliErr.Args)
	if cliBinaryMatches(cliErr.Binary, "docker") {
		for _, prefix := range []string{"docker: ", "error response from daemon: "} {
			if strings.HasPrefix(line, prefix) {
				return prefix, true
			}
		}
	}
	if cliBinaryMatches(cliErr.Binary, "container") && appleContainerPrefixAllowed(operation) {
		return "container: ", true
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
	lines, ok := cliErrorLines(err)
	if !ok {
		return false
	}
	for _, line := range lines {
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
	return false
}

func sameCLITarget(got, want string) bool {
	got = strings.Trim(strings.TrimSpace(got), `"'`)
	want = strings.Trim(strings.TrimSpace(want), `"'`)
	if got == "" || want == "" {
		return false
	}
	return strings.EqualFold(got, want)
}

// cliTargetListMatches accepts Apple's inspect spelling for one or more
// missing IDs, but rejects an arbitrary explanatory suffix.  This is what
// keeps "application: container not found: ..." from looking like a
// backend error.
func cliTargetListMatches(rest, want string) bool {
	rest = strings.TrimSpace(rest)
	want = strings.Trim(strings.TrimSpace(want), `"'`)
	if rest == "" || want == "" {
		return false
	}
	found := false
	for _, part := range strings.Split(rest, ",") {
		part = strings.Trim(strings.TrimSpace(part), `"'`)
		if part == "" || strings.ContainsAny(part, " \t\r\n") {
			return false
		}
		if want == "" || strings.EqualFold(part, strings.Trim(want, `"'`)) {
			found = true
		}
	}
	return found
}

// ambiguousContainerNotFound recognizes the generic phrase that is
// intentionally not part of Apple's anchored backend forms.  It is used to
// avoid probing/attaching on application output such as "container not
// found: record"; a real inspect error is handled by the operation-specific
// matcher instead.
func ambiguousContainerNotFound(err error) bool {
	lines, ok := cliErrorLines(err)
	if !ok {
		return false
	}
	for _, line := range lines {
		// Do not mistake the real Apple "container with ID ... not found"
		// form for this phrase.
		if strings.Contains(line, "container not found") &&
			!strings.Contains(line, "container with id") {
			return true
		}
	}
	return false
}

func isAmbiguousApplicationError(eng engine, err error) bool {
	if eng == nil || err == nil {
		return false
	}
	ctx, ok := backendCLIError(err, eng.binary())
	if !ok {
		return false
	}
	// Apple's inspect command uses this exact phrase for a real missing
	// object.  Other container operations do not, so the same wording there
	// is an application/process diagnostic.
	return ctx.operation != "inspect" && ambiguousContainerNotFound(err)
}
