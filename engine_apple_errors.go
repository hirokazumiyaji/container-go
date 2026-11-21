package container

import (
	"strings"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// Verified against Apple Container CLI 1.2.x–1.3.x (local: 1.3.0).
// Matchers are command-, binary-, code-, and target-aware. Apple's
// ContainerizationError envelope is either a direct code/message pair
// (run conflict, inspect, image inspect, exec) or an internalError wrapper
// whose cause is notFound (create race, stop, delete, logs). Legacy flat
// spellings remain accepted only for the same operation-specific forms.
// Generic application output containing "not found" is not a backend match;
// an application-prefixed typed message is rejected as well.
const (
	appleStderrNameConflict  = "container with id"
	appleStderrAlreadyExists = "already exists"
	appleStderrImageNotFound = "image not found:"
	// appleLogsGetPrefix and appleLogsOpenPrefix are the two exact markers
	// the 1.2/1.3 logs client wraps an absent container in. They appear
	// both in the flat spelling and as the root and cause messages of the
	// nested internalError envelope.
	appleLogsGetPrefix  = "failed to get logs for container "
	appleLogsOpenPrefix = "failed to open container logs: "
)

// imageMissing matches backend-specific image-absence reasons. Apple
// reports local inspect misses as `image not found: <name>` and registry
// pull misses as a MANIFEST_UNKNOWN code or an explicit HTTP 404 response.
func (appleEngine) imageMissing(err error) bool {
	return (appleEngine{}).imageMissingForTarget(err, "")
}

func (appleEngine) imageMissingForTarget(err error, target string) bool {
	if isContextError(err) {
		return false
	}
	branches := backendCLIErrorBranches(err, "container")
	if target == "" && ambiguousBranchTargets(branches) {
		return false
	}
	for _, branch := range branches {
		if !exactImageTarget(branch, target) {
			continue
		}
		if appleImageOperationalFailure(branch.stderr) {
			continue
		}
		if branch.ctx.operation == "image inspect" {
			if hasBranchImageLine(branch, appleStderrImageNotFound, branch.ctx.target, true) {
				return true
			}
			if hasAppleTypedImageLine(branch, branch.ctx.target) {
				return true
			}
		}
		if branch.ctx.operation == "image pull" {
			if appleImagePullManifestMissing(branch.ctx.err) {
				return true
			}
			if hasBranchImageLine(branch, appleStderrImageNotFound, branch.ctx.target, true) ||
				hasBranchImageLine(branch, appleStderrImageNotFound, "", false) {
				return true
			}
		}
	}
	return false
}

func appleImagePullManifestMissing(cliErr *cli.CLIError) bool {
	if cliErr == nil || len(cliErr.Args) < 3 ||
		!strings.EqualFold(cliErr.Args[0], "image") ||
		!strings.EqualFold(cliErr.Args[1], "pull") {
		return false
	}
	target := strings.TrimSpace(cliErr.Args[len(cliErr.Args)-1])
	if target == "" || strings.HasPrefix(target, "--") {
		return false
	}
	stderr := strings.ToLower(cliErr.Stderr)
	if appleManifestUnknownCode(stderr) {
		return true
	}
	for _, marker := range []string{
		"response: 404", "response 404", "response status 404", "status code: 404", "status: 404", "status 404", "http status 404", "status=404",
		"http 404", "http/1.1 404", "http/2 404", "404 not found",
	} {
		if strings.Contains(stderr, marker) {
			return true
		}
	}
	return false
}

func appleManifestUnknownCode(stderr string) bool {
	stderr = strings.ToLower(stderr)
	const code = "manifest_unknown"
	for start := 0; ; {
		index := strings.Index(stderr[start:], code)
		if index < 0 {
			return false
		}
		index += start
		end := index + len(code)
		beforeOK := index == 0 || !isASCIIWordByte(stderr[index-1])
		afterOK := end == len(stderr) || !isASCIIWordByte(stderr[end])
		if beforeOK && afterOK {
			return true
		}
		start = end
	}
}

func isASCIIWordByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_'
}

func appleImageOperationalFailure(stderr string) bool {
	stderr = strings.ToLower(stderr)
	for _, marker := range []string{
		"permission denied", "access denied", "unauthorized", "forbidden", "authentication required",
		"connection refused", "connection reset", "timed out", "no such host", "network is unreachable",
		"context deadline exceeded", "deadline exceeded", "i/o timeout", "network error", "xpc connection error",
	} {
		if strings.Contains(stderr, marker) {
			return true
		}
	}
	return false
}

// nameConflict matches Apple Container's duplicate-name wording on a
// create/run command. Other commands may legitimately contain the words
// "already" or "exists" in application/configuration diagnostics.
func (appleEngine) nameConflict(err error) bool {
	return (appleEngine{}).nameConflictForTarget(err, "")
}

func (appleEngine) nameConflictForTarget(err error, target string) bool {
	if isContextError(err) {
		return false
	}
	branches := backendCLIErrorBranches(err, "container")
	if target == "" && ambiguousBranchTargets(branches) {
		return false
	}
	for _, branch := range branches {
		if branch.ctx.operation != "" && branch.ctx.operation != "run" {
			continue
		}
		if !exactBranchTarget(branch, target) {
			continue
		}
		branchTarget := branch.ctx.target
		if hasAppleContainerizationError(branch.cause, "exists", func(message string) bool {
			return appleNameConflictLine(message, branchTarget) || appleServerConflictLine(message, branchTarget)
		}) {
			return true
		}
		if hasAppleContainerizationErrorPath(branch.cause, "internalerror", func(message string) bool {
			return message == "failed to create container"
		}, "exists", func(message string) bool {
			return appleServerConflictLine(message, branchTarget) || appleNameConflictLine(message, branchTarget)
		}) {
			return true
		}
		if hasBranchLine(branch, func(line string) bool {
			return appleNameConflictLine(line, branchTarget) || appleServerConflictLine(line, branchTarget)
		}) {
			return true
		}
		if hasBranchLine(branch, func(line string) bool {
			return appleTypedNameConflictLine(line, branchTarget)
		}) {
			return true
		}
	}
	return false
}

func appleNameConflictLine(line, target string) bool {
	line = strings.TrimSpace(strings.ToLower(line))
	if strings.Contains(line, "image configuration") || strings.Contains(line, "container configuration") {
		return false
	}
	if strings.HasPrefix(line, "error:") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "error:"))
	}
	if rest, ok := strings.CutPrefix(line, appleStderrNameConflict); ok {
		rest = strings.TrimSpace(rest)
		suffix := " " + appleStderrAlreadyExists
		if strings.HasSuffix(rest, suffix) {
			id := strings.TrimSpace(strings.TrimSuffix(rest, suffix))
			id = strings.Trim(id, `"'`)
			if id != "" && !strings.ContainsAny(id, " \t\r\n") &&
				(target == "" || sameCLITarget(id, target)) {
				return true
			}
		}
	}
	// Quoted create conflict: container "wanted" already exists
	if rest, ok := strings.CutPrefix(line, "container "); ok {
		rest = strings.TrimSpace(rest)
		suffix := " " + appleStderrAlreadyExists
		if strings.HasSuffix(rest, suffix) {
			id := strings.Trim(strings.TrimSpace(strings.TrimSuffix(rest, suffix)), `"'`)
			if id != "" && !strings.ContainsAny(id, " \t\r\n") &&
				(target == "" || sameCLITarget(id, target)) {
				return true
			}
		}
	}
	if rest, ok := strings.CutPrefix(line, "already exists: container"); ok {
		id := strings.TrimSpace(rest)
		id = strings.Trim(id, `"'`)
		if id != "" && !strings.ContainsAny(id, " \t\r\n") &&
			(target == "" || sameCLITarget(id, target)) {
			return true
		}
	}
	// Server-side create race spelling: "container already exists: <id>".
	if appleServerConflictLine(line, target) {
		return true
	}
	// Reject loose "already"/"exist"/"in use"/"taken" co-occurrence: that
	// matches application phrases such as "address already in use".
	return false
}

func appleServerConflictLine(message, target string) bool {
	rest, ok := strings.CutPrefix(message, "container already exists:")
	if !ok {
		return false
	}
	id := strings.Trim(strings.TrimSpace(rest), `"'`)
	return id != "" && !strings.ContainsAny(id, " \t\r\n") &&
		(target == "" || sameCLITarget(id, target))
}

// containerMissing matches the command-specific Apple Container forms
// for an absent container. The command, binary, and exact target are
// required so application output containing "not found" is not treated
// as a backend result. Typed envelopes are accepted through the escape-
// aware notFound unwrapper, then validated with operation-specific leaves.
func (appleEngine) containerMissing(err error) bool {
	ctx, ok := backendCLIError(err, "container")
	if !ok || !hasParsedCLITarget(ctx) {
		return false
	}
	switch ctx.operation {
	case "inspect", "exec", "stop", "delete", "rm", "logs", "start", "kill", "cp":
		return hasCLIErrorLine(err, func(line string) bool {
			return appleContainerMissingLine(line, ctx.target, ctx.operation)
		})
	default:
		return false
	}
}

func appleContainerMissingLine(line, target, command string) bool {
	if appleTypedNotFoundLine(line, target, command) {
		return true
	}
	switch command {
	case "inspect":
		rest, ok := strings.CutPrefix(line, "container not found:")
		return ok && cliTargetListMatches(rest, target)
	case "exec":
		return appleExecMissingLine(line, target)
	case "stop":
		if rest, ok := strings.CutPrefix(line, "container not found:"); ok && cliTargetListMatches(rest, target) {
			return true
		}
		return appleStateMissingLine(line, target, "failed to stop container:")
	case "start":
		return appleLifecycleMissing(line, target, "failed to start container:")
	case "kill":
		return appleLifecycleMissing(line, target, "failed to kill container:")
	case "delete", "rm":
		if rest, ok := strings.CutPrefix(line, "container not found:"); ok && cliTargetListMatches(rest, target) {
			return true
		}
		return appleStateMissingLine(line, target, "failed to delete container:")
	case "logs":
		if rest, ok := strings.CutPrefix(line, "container not found:"); ok && cliTargetListMatches(rest, target) {
			return true
		}
		return appleLogsMissingLine(line, target) || appleExecMissingLine(line, target)
	case "cp":
		if rest, ok := strings.CutPrefix(line, "container not found:"); ok && cliTargetListMatches(rest, target) {
			return true
		}
		return appleCopyMissingLine(line, target)
	default:
		return false
	}
}

// appleTypedNotFoundLine handles the structured ContainerizationError
// spelling emitted by Apple Container 1.3.0. The CLI prints descriptions
// such as notFound: "container not found: id" and can wrap that value in
// internalError: "..." (cause: "...") layers. Only these typed forms
// are accepted; an arbitrary application line containing "not found" is
// deliberately not a backend absence result.
func appleTypedNotFoundLine(line, target, command string) bool {
	message, ok := appleTypedNotFoundMessage(line)
	return ok && appleContainerNotFoundMessage(line, message, target, command)
}

// appleTypedNotFoundMessage walks the shared ContainerizationError parser and
// returns the message of the first notFound node reachable through
// internalError wrappers. Wording is lowercased to preserve the classifier's
// historical case-insensitive matching.
func appleTypedNotFoundMessage(line string) (string, bool) {
	root, ok := parseAppleContainerizationError(line)
	if !ok {
		return "", false
	}
	for node := &root; node != nil; node = node.cause {
		switch node.code {
		case "notfound":
			message := strings.ToLower(strings.TrimSpace(node.message))
			return message, message != ""
		case "internalerror":
			continue
		default:
			return "", false
		}
	}
	return "", false
}

func appleContainerNotFoundMessage(line, message, target, command string) bool {
	message = strings.TrimSpace(strings.Trim(message, `"'`))
	generic, hasGeneric := strings.CutPrefix(message, "container not found:")
	lowerLine := strings.ToLower(line)
	switch command {
	case "inspect":
		// Inspect emits the generic list/inspect spelling. A lifecycle
		// ID error or an exec-specific get failure is not an inspect
		// absence result.
		return hasGeneric && cliTargetListMatches(generic, target)
	case "exec":
		if hasGeneric && cliTargetListMatches(generic, target) {
			return strings.Contains(lowerLine, "failed to get container") ||
				strings.Contains(lowerLine, "operation failed")
		}
		return appleIDMessageMatches(message, target) || appleExecMissingLine(message, target)
	case "stop":
		if hasGeneric && cliTargetListMatches(generic, target) {
			return true
		}
		// Bare notFound ID spelling is a real Apple 1.3 stop form.
		return appleIDMessageMatches(message, target)
	case "start":
		return appleLifecycleMissing(message, target, "failed to start container:")
	case "kill":
		return appleLifecycleMissing(message, target, "failed to kill container:")
	case "delete", "rm":
		if hasGeneric && cliTargetListMatches(generic, target) {
			return strings.Contains(lowerLine, "failed to delete container") ||
				strings.Contains(lowerLine, "operation failed")
		}
		// Bare notFound ID envelopes without a delete root are application
		// output, not a backend delete absence.
		return strings.Contains(lowerLine, "failed to delete container") &&
			appleIDMessageMatches(message, target)
	case "cp":
		return appleCopyNotFoundMessage(line, message, target)
	case "logs":
		// A get-logs root that names another container must not be accepted
		// just because a nested open-logs cause mentions the argv target.
		if idx := strings.Index(lowerLine, appleLogsGetPrefix); idx >= 0 {
			rest := lowerLine[idx+len(appleLogsGetPrefix):]
			if end := strings.IndexAny(rest, `"'`); end >= 0 {
				rest = rest[:end]
			}
			rootID := strings.TrimSpace(rest)
			if rootID != "" && !sameCLITarget(rootID, target) {
				return false
			}
		}
		if hasGeneric && cliTargetListMatches(generic, target) {
			return strings.Contains(lowerLine, "failed to get logs") ||
				strings.Contains(lowerLine, "operation failed") ||
				strings.Contains(lowerLine, appleLogsOpenPrefix)
		}
		if appleLogsEnvelopeRoot(message, target) ||
			appleLogsOpenMissingLine(message, target) ||
			appleLogsMissingLine(message, target) {
			return true
		}
		if appleIDMessageMatches(message, target) || appleExecMissingLine(message, target) {
			return strings.Contains(lowerLine, "failed to get logs") ||
				strings.Contains(lowerLine, "operation failed") ||
				strings.Contains(lowerLine, appleLogsOpenPrefix)
		}
		return false
	default:
		return false
	}
}

func appleIDMessageMatches(message, target string) bool {
	return appleIDMissingLine(strings.TrimSpace(strings.Trim(message, `"'`)), target)
}

// appleCopyWrapperPrefixes are the client-side messages that wrap a failed
// cp. They name the container between the fixed phrase and either the
// closing quote of the typed envelope or the colon that separates a flat
// wrapper from its cause.
var appleCopyWrapperPrefixes = []string{
	"failed to copy into container ",
	"failed to copy from container ",
}

// appleCopyWrapperMessage returns the container named by a cp wrapper and
// the text after it. A malformed or space-bearing ID is rejected so the
// argv target cannot match text that is not a container reference.
func appleCopyWrapperMessage(line string) (containerID, message string, ok bool) {
	for _, prefix := range appleCopyWrapperPrefixes {
		index := strings.Index(line, prefix)
		if index < 0 {
			continue
		}
		rest := line[index+len(prefix):]
		if end := strings.IndexAny(rest, `"':`); end >= 0 {
			containerID = strings.TrimSpace(rest[:end])
			message = strings.TrimSpace(rest[end+1:])
		} else {
			containerID = strings.TrimSpace(rest)
		}
		if containerID == "" || strings.ContainsAny(containerID, " \t\r\n") {
			return "", "", false
		}
		return containerID, message, true
	}
	return "", "", false
}

// appleCopyMissingLine matches a flat cp wrapper around one of the accepted
// absence leaves, such as
// "failed to copy into container myctr: container with ID myctr not found".
func appleCopyMissingLine(line, target string) bool {
	containerID, message, ok := appleCopyWrapperMessage(line)
	if !ok || !sameCLITarget(containerID, target) {
		return false
	}
	if rest, ok := strings.CutPrefix(message, "container not found:"); ok &&
		cliTargetListMatches(rest, target) {
		return true
	}
	return appleIDMessageMatches(message, target) || appleExecMissingLine(message, target)
}

// appleCopyNotFoundMessage matches the typed notFound leaves a cp reports.
// The CLI wraps the server's absence inside
// internalError: "failed to copy into container <id>" (cause: notFound: ...),
// so an internalError root must carry a cp wrapper naming the argv target.
// A top-level notFound (no internalError root) is accepted through its own
// anchored leaf.
func appleCopyNotFoundMessage(line, message, target string) bool {
	containerID, _, hasWrapper := appleCopyWrapperMessage(line)
	if hasWrapper && !sameCLITarget(containerID, target) {
		return false
	}
	if strings.HasPrefix(line, "internalerror:") && !hasWrapper {
		return false
	}
	if rest, ok := strings.CutPrefix(message, "container not found:"); ok &&
		cliTargetListMatches(rest, target) {
		return true
	}
	return appleIDMessageMatches(message, target) || appleExecMissingLine(message, target)
}

func appleExecMissingLine(line, target string) bool {
	rest, ok := strings.CutPrefix(line, "get failed:")
	if !ok {
		return false
	}
	rest = strings.TrimSpace(rest)
	rest, ok = strings.CutPrefix(rest, "container ")
	if !ok || !strings.HasSuffix(rest, " not found") {
		return false
	}
	id := strings.Trim(strings.TrimSpace(strings.TrimSuffix(rest, " not found")), `"'`)
	return id != "" && !strings.ContainsAny(id, " \t\r\n") &&
		(target == "" || strings.EqualFold(id, target))
}

func appleIDMissingLine(line, target string) bool {
	rest, ok := strings.CutPrefix(line, "container with id ")
	if !ok || !strings.HasSuffix(rest, " not found") {
		return false
	}
	id := strings.Trim(strings.TrimSpace(strings.TrimSuffix(rest, " not found")), `"'`)
	return id != "" && !strings.ContainsAny(id, " \t\r\n") &&
		(target == "" || strings.EqualFold(id, target))
}

func appleStateMissingLine(line, target, wrapper string) bool {
	current := line
	for range 3 {
		if appleIDMissingLine(current, target) {
			return true
		}
		rest, ok := strings.CutPrefix(current, wrapper)
		if !ok {
			return false
		}
		current = strings.TrimSpace(rest)
	}
	return false
}

// appleLifecycleMissing matches the absence forms a start or kill reports:
// the generic "container not found:" leaf, the "get failed: container ...
// not found" leaf start receives from its initial get, or the operation
// wrapper around either leaf. The wrapper may be rendered with or without a
// trailing colon depending on the CLI version.
func appleLifecycleMissing(value, target, wrapper string) bool {
	current := strings.TrimSpace(value)
	for range 3 {
		if appleIDMissingLine(current, target) || appleExecMissingLine(current, target) {
			return true
		}
		if rest, ok := strings.CutPrefix(current, "container not found:"); ok &&
			cliTargetListMatches(rest, target) {
			return true
		}
		rest, ok := strings.CutPrefix(current, wrapper)
		if !ok {
			return false
		}
		current = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rest), ":"))
	}
	return false
}

func appleLogsEnvelopeRoot(message, target string) bool {
	// The 1.2/1.3 logs envelope roots at "failed to get logs for container
	// <id>" with nothing after the ID, so an explanatory suffix (an
	// application's own sentence) cannot satisfy the anchor.
	rest, ok := strings.CutPrefix(message, appleLogsGetPrefix)
	return ok && sameCLITarget(rest, target)
}

// appleLogsOpenMissingLine accepts the notFound cause of that envelope: the
// absence reported behind the same "failed to open container logs: " marker
// the flat spelling uses, followed by one of the known missing-ID leaves.
func appleLogsOpenMissingLine(message, target string) bool {
	rest, ok := strings.CutPrefix(message, appleLogsOpenPrefix)
	if !ok {
		return false
	}
	rest = strings.TrimSpace(rest)
	return appleIDMissingLine(rest, target) || appleExecMissingLine(rest, target)
}

func appleLogsMissingLine(line, target string) bool {
	current := line
	// Flat logs absence roots at a logs-specific wrapper. A bare notFound
	// ID leaf without get-logs/open-logs anchoring is application output.
	if rest, ok := strings.CutPrefix(current, appleLogsGetPrefix); ok {
		rest = strings.TrimSpace(rest)
		separator := strings.Index(rest, ":")
		if separator < 0 {
			return false
		}
		logID := strings.TrimSpace(rest[:separator])
		if logID == "" || (target != "" && !sameCLITarget(logID, target)) {
			return false
		}
		current = strings.TrimSpace(rest[separator+1:])
	} else if rest, ok := strings.CutPrefix(current, appleLogsOpenPrefix); ok {
		current = strings.TrimSpace(rest)
	} else {
		return false
	}

	// Client and API versions wrap the same absence in a small, known set
	// of prefixes. Bound the depth so arbitrary nested diagnostics cannot
	// grow the classifier without limit.
	for range 3 {
		if appleIDMissingLine(current, target) || appleExecMissingLine(current, target) {
			return true
		}
		if rest, ok := strings.CutPrefix(current, appleLogsOpenPrefix); ok {
			current = strings.TrimSpace(rest)
			continue
		}
		if rest, ok := strings.CutPrefix(current, "get failed:"); ok {
			current = strings.TrimSpace(rest)
			rest, ok = strings.CutPrefix(current, "container ")
			if ok && strings.HasSuffix(rest, " not found") {
				id := strings.Trim(strings.TrimSpace(strings.TrimSuffix(rest, " not found")), `"'`)
				if id != "" && !strings.ContainsAny(id, " \t\r\n") &&
					(target == "" || sameCLITarget(id, target)) {
					return true
				}
			}
			continue
		}
		return false
	}
	return false
}
