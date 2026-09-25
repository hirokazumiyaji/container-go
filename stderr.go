package container

import (
	"errors"
	"strings"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// cliErrorLinePrefixes are wrappers emitted by the supported CLIs around
// their structured error messages. Removing only these prefixes lets the
// backend matchers require an error kind to start a stderr line, rather
// than finding the same words in an application or configuration message.
var cliErrorLinePrefixes = []string{
	"docker: ",
	"container: ",
	"error response from daemon: ",
	"error: ",
}

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
		return firstPositional(args, 2)
	case "inspect", "exec", "logs":
		return firstPositional(args, 1)
	case "stop", "delete", "rm":
		if len(args) > 1 {
			return args[len(args)-1]
		}
	}
	return ""
}

func firstPositional(args []string, start int) string {
	for i := start; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		}
		if strings.HasPrefix(arg, "-") {
			// These options carry a value in the argv forms emitted by
			// this package.  Do not mistake that value for the target.
			switch arg {
			case "--env-file", "--user", "--workdir", "--time", "--tail", "--since", "--platform":
				i++
			}
			continue
		}
		return arg
	}
	return ""
}

// cliErrorLines returns normalized, non-empty stderr lines.  Only known CLI
// wrappers are removed; arbitrary application/configuration prefixes remain
// visible and consequently cannot satisfy an anchored backend matcher.
func cliErrorLines(err error) ([]string, bool) {
	var cliErr *cli.CLIError
	if !errors.As(err, &cliErr) {
		return nil, false
	}

	var lines []string
	for _, line := range strings.Split(cliErr.Stderr, "\n") {
		line = normalizeCLIErrorLine(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines, true
}

func normalizeCLIErrorLine(line string) string {
	line = strings.ToLower(strings.TrimSpace(line))
	for {
		changed := false
		for _, prefix := range cliErrorLinePrefixes {
			if strings.HasPrefix(line, prefix) {
				line = strings.TrimSpace(strings.TrimPrefix(line, prefix))
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

func sameCLITarget(got, want string) bool {
	got = strings.Trim(strings.TrimSpace(got), `"'`)
	want = strings.Trim(strings.TrimSpace(want), `"'`)
	if want == "" {
		return got != ""
	}
	return strings.EqualFold(got, want)
}

// cliTargetListMatches accepts Apple's inspect spelling for one or more
// missing IDs, but rejects an arbitrary explanatory suffix.  This is what
// keeps "application: container not found: ..." from looking like a
// backend error.
func cliTargetListMatches(rest, want string) bool {
	rest = strings.TrimSpace(rest)
	if rest == "" {
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
