package container

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
	"github.com/hirokazumiyaji/container-go/internal/inspect"
)

// appleEngine drives Apple Container's `container` CLI.
type appleEngine struct{}

// Apple Container parses stop --time as a signed 32-bit integer, even when
// the library itself runs on a 64-bit host.
const maxAppleStopSeconds int64 = math.MaxInt32

// Verified against Apple Container CLI 1.2.x–1.3.x (local: 1.3.0).
// Matchers are both command- and binary-aware. Apple 1.3 renders typed
// ContainerizationError values (notFound/internalError with nested causes),
// while older releases used the plain forms below:
//   - inspect: "container not found: …"
//   - exec: "get failed: container <id> not found"
//   - stop/delete: "container with ID <id> not found" (often wrapped by
//     "failed to stop/delete container: …")
//   - logs: "failed to get logs for container <id>: …"
//
// Generic application output containing "not found" is not a backend match;
// an application-prefixed typed message is rejected as well.
const (
	appleStderrNameConflict  = "container with id"
	appleStderrAlreadyExists = "already exists"
	appleStderrImageNotFound = "image not found:"
	appleStderrAlready       = "already"
	appleStderrExist         = "exist"
	appleStderrInUse         = "in use"
	appleStderrTaken         = "taken"
	appleStderrNotFound      = "not found"
)

func (appleEngine) name() string   { return "apple" }
func (appleEngine) binary() string { return "container" }
func (appleEngine) directIP() bool { return true }

func (appleEngine) checkConfig(context.Context, *config) error { return nil }

// imageStoreID is empty: Apple Container talks to one local store.
func (appleEngine) imageStoreID() string { return "" }

func (appleEngine) defaultHost() string { return "127.0.0.1" }

func (appleEngine) probe() cli.Probe {
	return cli.Probe{
		Args:          []string{"system", "status"},
		Hint:          "run `container system start`",
		Binary:        "container",
		Operation:     "system",
		IsUnavailable: appleProbeUnavailable,
	}
}

func appleProbeUnavailable(err error) bool {
	branches := backendCLIErrorBranches(err, "container")
	if len(branches) == 0 {
		return false
	}
	// A reachable daemon can fail for client-side configuration or
	// authentication reasons. Those diagnostics veto only the matching
	// system-status branch.
	for _, branch := range branches {
		if branch.ctx.operation != "system" {
			continue
		}
		if cli.IsProbeConfigurationError(branch.cause) {
			return false
		}
	}
	for _, branch := range branches {
		if branch.ctx.operation != "system" {
			continue
		}
		stderr, stdout := branchLines(branch, true)
		for _, line := range stderr {
			if appleProbeLivenessText(line) {
				return true
			}
		}
		for _, line := range stdout {
			if appleProbeLivenessText(line) {
				return true
			}
		}
	}
	return false
}

func appleProbeLivenessText(line string) bool {
	for _, fragment := range []string{
		"xpc connection",
		"container-apiserver",
		"plugins are unavailable",
		"start the container system services",
		"system is not running",
		"system service is not running",
		"apiserver is not running",
		"not registered with launchd",
		"connection refused",
	} {
		if strings.Contains(line, fragment) {
			return true
		}
	}
	return false
}

func (appleEngine) runArgs(cfg *config, image, envFile string) []string {
	args := []string{"run", "--detach", "--name", cfg.name}
	return append(args, cfg.commonRunArgs(image, envFile, nil)...)
}

func (appleEngine) parseRunID([]byte) string { return "" }

func (appleEngine) inspectArgs(id string) []string { return []string{"inspect", id} }

func (appleEngine) parseInspect(data []byte, id string) (*engineInfo, error) {
	if strings.TrimSpace(string(data)) == "" {
		return nil, newInspectTargetNotFound(id, "empty inspect output")
	}
	containers, err := inspect.Decode(data)
	if err != nil {
		return nil, err
	}
	for _, c := range containers {
		if strings.TrimSpace(c.ID) == "" && strings.TrimSpace(c.Status.State) != "" {
			return nil, fmt.Errorf("decode container inspect output: container id is required")
		}
	}
	var matchedMissingStatus bool
	for _, c := range containers {
		if !appleInspectTargetMatches(c, id) {
			continue
		}
		if strings.TrimSpace(c.Status.State) == "" {
			matchedMissingStatus = true
			continue
		}
		info := &engineInfo{
			state:  appleState(c.Status.State),
			name:   c.ID,
			labels: c.Configuration.Labels,
			image:  c.Configuration.Image.Reference,
		}
		platform := c.Configuration.Platform
		platformMeta := platformMetadataFromParts(
			platform.OS,
			platform.Architecture,
			platform.Variant,
			platform.OSPresent || platform.OS != "",
			platform.ArchPresent || platform.Architecture != "",
			platform.VariantPresent || platform.Variant != "",
		)
		info.platform = platformMeta.normalized()
		info.platformMeta = platformMeta
		if ip, err := c.IPv4(); err == nil {
			info.ip = ip
		}
		for _, p := range c.Configuration.PublishedPorts {
			info.bound = append(info.bound, boundPort{
				containerPort: p.ContainerPort,
				proto:         p.Proto,
				hostAddr:      canonicalIP(p.HostAddress),
				hostPort:      p.HostPort,
			})
		}
		return info, nil
	}
	if matchedMissingStatus {
		return nil, fmt.Errorf("decode container inspect output: container status.state is required")
	}
	return nil, newInspectTargetNotFound(id, "inspect output did not contain the requested target")
}

func appleInspectTargetMatches(c inspect.Container, target string) bool {
	target = strings.TrimPrefix(strings.TrimSpace(target), "/")
	if target == "" {
		return false
	}
	return strings.TrimPrefix(strings.TrimSpace(c.ID), "/") == target
}

func appleState(state string) State {
	switch state {
	case string(StateRunning):
		return StateRunning
	case string(StateStopped):
		return StateStopped
	case string(StateStopping):
		return StateStopping
	case string(StateCreated):
		return StateCreated
	default:
		return StateUnknown
	}
}

func (appleEngine) stopArgs(id string, timeout *time.Duration) ([]string, error) {
	return stopArgsFor(id, timeout, maxAppleStopSeconds)
}

func (appleEngine) deleteArgs(id string) []string {
	return []string{"delete", "--force", id}
}

func (appleEngine) copyToArgs(id, hostPath, containerPath string) []string {
	return []string{"cp", hostPath, id + ":" + containerPath}
}

func (appleEngine) copyFromArgs(id, containerPath, hostPath string) []string {
	return []string{"cp", id + ":" + containerPath, hostPath}
}

// Apple Container's public cp command has no mode that preserves source
// types or prevents it from dereferencing special files, so fail closed.
func (appleEngine) checkCopyFileFromContainer() error {
	return fmt.Errorf(
		"%w: Apple Container cp has no type-preserving/no-follow copy-out mode",
		ErrCopyFileFromContainerUnsupported,
	)
}

func (appleEngine) checkCopyFileFromContainerVersion(context.Context, cli.Runner) error {
	return appleEngine{}.checkCopyFileFromContainer()
}

func (appleEngine) reaperSubcommand() string { return "delete" }

func (appleEngine) reaperDeleteFlags() []string { return nil }

func (appleEngine) execArgs(id string, cfg *execConfig, envFile string, cmd []string) []string {
	args := []string{"exec"}
	if envFile != "" {
		args = append(args, "--env-file", envFile)
	}
	if cfg.user != "" {
		args = append(args, "--user", cfg.user)
	}
	if cfg.workdir != "" {
		args = append(args, "--workdir", cfg.workdir)
	}
	args = append(args, id)
	return append(args, cmd...)
}

func (appleEngine) logsFollowArgs(id string) []string {
	return []string{"logs", "--follow", id}
}

func (appleEngine) logsArgsWithOptions(id string, opts LogsOptions) ([]string, error) {
	if !opts.Since.IsZero() {
		return nil, fmt.Errorf("%w: apple backend does not support logs since", ErrUnsupportedCapability)
	}
	args := []string{"logs"}
	if opts.Tail > 0 {
		args = append(args, "-n", strconv.Itoa(opts.Tail))
	}
	return append(args, id), nil
}

func (appleEngine) logsTailArgs(id string) []string {
	return []string{"logs", "-n", "1000", id}
}

func (appleEngine) listArgs() []string {
	return []string{"ls", "--all", "--format", "json"}
}

// parseStoppedManaged filters client-side: the Apple CLI exposes no
// label or status filter.
func (appleEngine) parseStoppedManaged(data []byte) ([]string, error) {
	containers, err := inspect.Decode(data)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, c := range containers {
		if c.Configuration.Labels[managedLabel] == "true" && c.Status.State == string(StateStopped) {
			ids = append(ids, c.ID)
		}
	}
	return ids, nil
}

func (appleEngine) imageInspectArgs(image, _ string) []string {
	return []string{"image", "inspect", image}
}

func (appleEngine) pullImageArgs(image, platform string) []string {
	if platform != "" {
		return []string{"image", "pull", "--platform", platform, image}
	}
	return []string{"image", "pull", image}
}

// imageMissing matches only Apple's image-inspect error. A pull failure
// with similar text is not evidence that the local image check was missing.
func (appleEngine) imageMissing(err error) bool {
	return (appleEngine{}).imageMissingForTarget(err, "")
}

func (appleEngine) imageMissingForTarget(err error, target string) bool {
	branches := backendCLIErrorBranches(err, "container")
	if target == "" && ambiguousBranchTargets(branches) {
		return false
	}
	for _, branch := range branches {
		if branch.ctx.operation != "image inspect" || !exactImageTarget(branch, target) {
			continue
		}
		if hasBranchImageLine(branch, appleStderrImageNotFound, branch.ctx.target, true) {
			return true
		}
		if hasAppleTypedImageLine(branch, branch.ctx.target) {
			return true
		}
	}
	return false
}

func (appleEngine) parseImageExists(data []byte, platform string) bool {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil || len(raw) == 0 {
		return false
	}
	if platform == "" {
		return true
	}
	var images []struct {
		Variants []struct {
			Platform struct {
				Os           string `json:"os"`
				Architecture string `json:"architecture"`
				Variant      string `json:"variant"`
			} `json:"platform"`
		} `json:"variants"`
	}
	if err := json.Unmarshal(data, &images); err != nil {
		return true
	}
	wantOS, wantArch, wantVariant := splitPlatform(platform)
	for _, img := range images {
		if len(img.Variants) == 0 {
			return true
		}
		for _, v := range img.Variants {
			if wantOS != "" && v.Platform.Os != wantOS {
				continue
			}
			if wantArch != "" && v.Platform.Architecture != wantArch {
				continue
			}
			if wantVariant != "" && v.Platform.Variant != wantVariant {
				continue
			}
			return true
		}
	}
	return false
}

func splitPlatform(p string) (os, arch, variant string) {
	parts := strings.Split(p, "/")
	if len(parts) > 0 {
		os = parts[0]
	}
	if len(parts) > 1 {
		arch = parts[1]
	}
	if len(parts) > 2 {
		variant = parts[2]
	}
	return os, arch, variant
}

func (appleEngine) platformCompatible(selector, actual string) bool {
	return platformSelectorMatches(selector, actual)
}

func (appleEngine) listReuseGroupArgs(string) []string {
	return []string{"ls", "--all", "--format", "json"}
}

func (appleEngine) parseReuseGroupIDs(data []byte, group string) ([]string, error) {
	containers, err := inspect.Decode(data)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, c := range containers {
		if c.Configuration.Labels[reuseGroupLabel] == group {
			ids = append(ids, c.ID)
		}
	}
	return ids, nil
}

// nameConflict matches Apple Container's duplicate-name wording on a
// create/run command. Other commands may legitimately contain the words
// "already" or "exists" in application/configuration diagnostics.
func (appleEngine) nameConflict(err error) bool {
	return (appleEngine{}).nameConflictForTarget(err, "")
}

func (appleEngine) nameConflictForTarget(err error, target string) bool {
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
		if hasBranchLine(branch, func(line string) bool {
			return appleNameConflictLine(line, branch.ctx.target)
		}) {
			return true
		}
		if hasBranchLine(branch, func(line string) bool {
			return appleTypedNameConflictLine(line, branch.ctx.target)
		}) {
			return true
		}
	}
	return false
}

func appleNameConflictLine(line, target string) bool {
	line = strings.TrimSpace(strings.ToLower(line))
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
	if rest, ok := strings.CutPrefix(line, "already exists: container"); ok {
		id := strings.TrimSpace(rest)
		id = strings.Trim(id, `"'`)
		if id != "" && !strings.ContainsAny(id, " \t\r\n") &&
			(target == "" || sameCLITarget(id, target)) {
			return true
		}
	}
	if strings.Contains(line, appleStderrAlready) &&
		(strings.Contains(line, appleStderrExist) ||
			strings.Contains(line, appleStderrInUse) ||
			strings.Contains(line, appleStderrTaken)) {
		if target == "" {
			return true
		}
		if strings.Contains(line, strings.ToLower(target)) {
			return true
		}
	}
	return false
}
// containerMissing matches the command-specific Apple Container forms
// for an absent container. The command, binary, and exact target are
// required so application output containing "not found" is not treated
// as a backend result.
func (appleEngine) containerMissing(err error) bool {
	if !cliErrorBelongsTo(err, "container") {
		return false
	}
	command, args, ok := cliCommandParts(err)
	if !ok {
		return false
	}
	target := cliCommandTarget(command, args)
	if target == "" {
		return false
	}
	switch command {
	case "inspect", "exec", "stop", "delete", "rm", "logs":
		return hasCLIErrorLine(err, func(line string) bool {
			return appleContainerMissingLine(line, target, command)
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

func appleTypedContainerIDNotFoundLine(line, target string) bool {
	message, ok := appleTypedNotFoundMessage(line)
	return ok && appleIDMessageMatches(message, target)
}

// appleTypedNotFoundMessage unwraps only the typed wrapper grammar. The
// cause value is treated as a quoted string at each level, then escapes are
// decoded before the next wrapper is examined. This handles both escaped
// forms emitted by the CLI and the older unescaped nested spelling without
// searching arbitrary stderr for the words "not found".
func appleTypedNotFoundMessage(line string) (string, bool) {
	current := strings.ToLower(strings.TrimSpace(line))
	if strings.HasPrefix(current, "error:") {
		current = strings.TrimSpace(strings.TrimPrefix(current, "error:"))
	}
	for range 16 {
		current = strings.TrimSpace(current)
		current = strings.TrimSpace(strings.Trim(current, "()"))
		switch {
		case strings.HasPrefix(current, "notfound:"):
			message := decodeAppleQuotedValue(strings.TrimSpace(strings.TrimPrefix(current, "notfound:")))
			return message, message != ""
		case strings.HasPrefix(current, "internalerror:"), strings.HasPrefix(current, "cause:"):
			index := strings.Index(current, "cause:")
			if index < 0 {
				return "", false
			}
			current = decodeAppleQuotedValue(strings.TrimSpace(current[index+len("cause:"):]))
			if current == "" {
				return "", false
			}
		default:
			return "", false
		}
	}
	return "", false
}

// decodeAppleQuotedValue removes one outer quoted value and decodes the
// escaping used when Swift's diagnostic description is rendered. The
// fallback is intentionally permissive for the CLI's older unescaped nested
// form, but it still requires the value to be bounded by a quote.
func decodeAppleQuotedValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') {
		quote := value[0]
		value = value[1:]
		if value[len(value)-1] == quote {
			value = value[:len(value)-1]
		}
	}
	var b strings.Builder
	b.Grow(len(value))
	escaped := false
	for _, r := range value {
		if !escaped {
			if r == '\\' {
				escaped = true
				continue
			}
			b.WriteRune(r)
			continue
		}
		switch r {
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case '"', '\\':
			b.WriteRune(r)
		default:
			b.WriteByte('\\')
			b.WriteRune(r)
		}
		escaped = false
	}
	if escaped {
		b.WriteByte('\\')
	}
	return strings.TrimSpace(b.String())
}

func appleContainerNotFoundMessage(line, message, target, command string) bool {
	message = strings.TrimSpace(strings.Trim(message, `"'`))
	generic, hasGeneric := strings.CutPrefix(message, "container not found:")
	switch command {
	case "inspect":
		// Inspect emits the generic list/inspect spelling. A lifecycle
		// ID error or an exec-specific get failure is not an inspect
		// absence result.
		return hasGeneric && cliTargetListMatches(generic, target)
	case "exec":
		if hasGeneric && cliTargetListMatches(generic, target) {
			return strings.Contains(strings.ToLower(line), "failed to get container")
		}
		return appleIDMessageMatches(message, target) || appleExecMissingLine(message, target)
	case "stop", "delete", "rm":
		if hasGeneric && cliTargetListMatches(generic, target) {
			return true
		}
		return appleIDMessageMatches(message, target)
	case "logs":
		if hasGeneric && cliTargetListMatches(generic, target) {
			return true
		}
		return appleIDMessageMatches(message, target) || appleExecMissingLine(message, target)
	default:
		return false
	}
}

func appleIDMessageMatches(message, target string) bool {
	return appleIDMissingLine(strings.TrimSpace(strings.Trim(message, `"'`)), target)
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

func appleLogsMissingLine(line, target string) bool {
	const prefix = "failed to get logs for container "
	rest, ok := strings.CutPrefix(line, prefix)
	if !ok {
		return appleIDMissingLine(line, target)
	}
	rest = strings.TrimSpace(rest)
	separator := strings.Index(rest, ":")
	if separator < 0 {
		return false
	}
	logID := strings.TrimSpace(rest[:separator])
	if logID == "" || (target != "" && !strings.EqualFold(logID, target)) {
		return false
	}
	nested := strings.TrimSpace(rest[separator+1:])
	const openPrefix = "failed to open container logs: "
	for range 3 {
		if appleIDMissingLine(nested, target) || appleExecMissingLine(nested, target) {
			return true
		}
		openRest, ok := strings.CutPrefix(nested, openPrefix)
		if !ok {
			return false
		}
		nested = strings.TrimSpace(openRest)
	}
	return false
}
