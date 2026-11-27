package container

import (
	"strings"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// Verified against Docker Engine / CLI 29.x (local: 29.7.2).
// Stderr substrings below are matched case-insensitively on CLIError.Stderr.
// Observed wording:
//   - name conflict: "Conflict. The container name \"/x\" is already in use by container …"
//   - image missing: "Error response from daemon: No such image: …"
//   - container missing: "error: no such object: …" (also historically
//     "No such container" / "not found")
const (
	dockerStderrConflict     = "conflict. the container name "
	dockerStderrAlreadyInUse = "already in use"
	dockerStderrName         = "name"
	dockerStderrNoSuchImage  = "no such image:"
	dockerStderrNotFound     = "not found"
	dockerStderrNoSuchObj    = "no such object"
	dockerStderrNoSuchCtr    = "no such container"
)

type dockerContextUnavailableError struct {
	cause  error
	detail string
}

func (e *dockerContextUnavailableError) Error() string {
	return "docker context command unavailable: " + e.detail
}

func (e *dockerContextUnavailableError) Unwrap() error { return e.cause }

type dockerContextInspectError struct {
	*ConfigError
	cause error
}

func (e *dockerContextInspectError) Unwrap() []error { return []error{e.ConfigError, e.cause} }

func isDockerContextUnavailable(err error, stderr string) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(stderr + " " + err.Error())
	return strings.Contains(msg, "unknown command") ||
		strings.Contains(msg, "is not a docker command") ||
		strings.Contains(msg, "unexpected command") ||
		strings.Contains(msg, "not supported") ||
		strings.Contains(msg, "command not found") ||
		strings.Contains(msg, "executable file not found")
}

// imageMissing matches only Docker's image-inspect response. Pull errors
// and arbitrary application output are not local-store absence evidence.
func (dockerEngine) imageMissing(err error) bool {
	return (dockerEngine{}).imageMissingForTarget(err, "")
}

func (dockerEngine) imageMissingForTarget(err error, target string) bool {
	if isContextError(err) {
		return false
	}
	branches := backendCLIErrorBranches(err, "docker")
	if target == "" && ambiguousBranchTargets(branches) {
		return false
	}
	for _, branch := range branches {
		if branch.ctx.operation != "image inspect" || !exactImageTarget(branch, target) {
			continue
		}
		if dockerImageOperationalFailure(branch.stderr) {
			continue
		}
		if hasBranchImageLine(branch, dockerStderrNoSuchImage, branch.ctx.target, true) {
			return true
		}
		if dockerImageMissingStderr(branch) {
			return true
		}
	}
	return false
}

func dockerImageOperationalFailure(stderr string) bool {
	stderr = strings.ToLower(stderr)
	for _, marker := range []string{
		"permission denied", "pull access denied", "access denied", "unauthorized",
		"connection refused", "connection reset", "no such host", "network is unreachable",
		"context deadline exceeded", "i/o timeout",
	} {
		if strings.Contains(stderr, marker) {
			return true
		}
	}
	return false
}

func dockerImageMissingStderr(branch cliErrorBranch) bool {
	allowGenericError := genericErrorWrapperAllowed(branch.ctx.err)
	for _, raw := range splitCLIErrorLines(branch.stderr) {
		line := trimCLIErrorWrapper(raw, branch.ctx.err, allowGenericError)
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "manifest unknown:") {
			rest := line[len("manifest unknown:"):]
			if strings.TrimSpace(rest) == "" || cliTargetListExactMatches(rest, branch.ctx.target) {
				return true
			}
			continue
		}
		if lower == "manifest unknown" ||
			strings.Contains(lower, `"code":"manifest_unknown"`) ||
			strings.Contains(lower, `"code": "manifest_unknown"`) {
			return true
		}
		if strings.HasPrefix(lower, "no matching manifest for ") &&
			strings.Contains(lower, " in the manifest list entries") {
			return true
		}
	}
	return false
}

// nameConflict matches Docker's duplicate container name error on a
// create/run command. A delete or application command containing the same
// words is not evidence that this library lost a name race.
func (dockerEngine) nameConflict(err error) bool {
	return (dockerEngine{}).nameConflictForTarget(err, "")
}

func (dockerEngine) nameConflictForTarget(err error, target string) bool {
	if isContextError(err) {
		return false
	}
	branches := backendCLIErrorBranches(err, "docker")
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
		if hasBranchLine(branch, func(line string) bool {
			return dockerNameConflictLine(line, branch.ctx.target)
		}) {
			return true
		}
	}
	return false
}

func dockerNameConflictLine(line, target string) bool {
	line = strings.ToLower(line)
	if strings.Contains(line, "image configuration") {
		return false
	}
	if !strings.HasPrefix(line, dockerStderrConflict) ||
		!strings.Contains(line, dockerStderrAlreadyInUse) {
		return false
	}
	if target == "" {
		return true
	}
	// Docker always renders the conflicting name as the next quoted token.
	// Do not fall back to a substring match: a container named after an
	// unrelated word ("use", "container") appears in the fixed wording of
	// every conflict line and would classify the conflict for the wrong
	// target.
	rest := strings.TrimSpace(strings.TrimPrefix(line, dockerStderrConflict))
	if len(rest) < 2 || rest[0] != '"' {
		return false
	}
	rest = rest[1:]
	if end := strings.IndexByte(rest, '"'); end >= 0 {
		rest = rest[:end]
	}
	return sameDockerContainerName(rest, target)
}

func sameDockerContainerName(got, want string) bool {
	got = strings.TrimPrefix(strings.Trim(strings.TrimSpace(got), `"'`), "/")
	want = strings.TrimPrefix(strings.Trim(strings.TrimSpace(want), `"'`), "/")
	return strings.EqualFold(got, want)
}

// containerMissing matches a CLI failure for an absent container.
func (dockerEngine) containerMissing(err error) bool {
	if isContextError(err) || !cliErrorBelongsTo(err, "docker") {
		return false
	}
	argv, ok := cliCommandParts(err)
	if !ok {
		return false
	}
	command := strings.ToLower(cli.CommandOperation(argv))
	target := commandTarget(argv, command)
	if target == "" {
		return false
	}
	switch command {
	case "inspect":
		return hasCLIErrorLine(err, func(line string) bool {
			if rest, ok := strings.CutPrefix(line, dockerStderrNoSuchObj+":"); ok && cliTargetListMatches(rest, target) {
				return true
			}
			rest, ok := strings.CutPrefix(line, dockerStderrNoSuchCtr+":")
			return ok && cliTargetListMatches(rest, target)
		})
	case "rm", "stop", "start", "restart", "kill", "pause", "unpause", "exec", "logs", "cp":
		if hasCLIErrorLine(err, func(line string) bool {
			rest, ok := strings.CutPrefix(line, dockerStderrNoSuchCtr+":")
			return ok && cliTargetListMatches(rest, target)
		}) {
			return true
		}
		// Older Docker clients used a target-qualified generic phrase for
		// rm. Keep that narrow fallback; it is not used for exec/logs,
		// whose stderr may be application output.
		if command == "rm" {
			return hasCLIErrorLine(err, func(line string) bool {
				rest, ok := strings.CutPrefix(line, dockerStderrNotFound+":")
				return ok && cliTargetListMatches(strings.TrimSpace(rest), target)
			})
		}
		return false
	}
	return false
}
