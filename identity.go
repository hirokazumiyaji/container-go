package container

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// validCreationGeneration reports whether a value is one of the
// generation IDs this library creates. Empty or malformed values cannot
// prove that two name-addressed observations refer to the same Apple
// Container generation.
func validCreationGeneration(generation string) bool {
	return creationRE.MatchString(generation)
}

// validDockerUID reports whether a value is a full Docker container ID.
// Docker IDs are 64 lowercase hexadecimal characters; accepting a short
// or otherwise malformed value would make identity comparisons unsafe.
func validDockerUID(uid string) bool {
	return dockerIDRE.MatchString(uid)
}

// validDockerImageID reports whether a Docker image identity is a
// content-addressed image ID. A mutable tag is not an OCI identity and
// must not be used to resolve the platform of a running container.
func validDockerImageID(id string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(id, prefix) {
		return false
	}
	hex := strings.TrimPrefix(id, prefix)
	if len(hex) != 64 {
		return false
	}
	for _, r := range hex {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func validateReuseIdentity(eng engine, info *engineInfo) error {
	if info == nil {
		return errIdentity("empty inspect result")
	}
	switch eng.name() {
	case "docker":
		if !validDockerUID(info.uid) {
			return errIdentity("Docker inspect returned no valid immutable container ID")
		}
	case "apple":
		if !validCreationGeneration(info.labels[creationLabel]) {
			return errIdentity("inspect returned no valid Apple creation generation")
		}
		if info.uid != "" {
			return errIdentity("Apple inspect unexpectedly returned a container ID")
		}
	default:
		return errIdentity("unknown backend cannot provide a reusable identity")
	}
	return nil
}

type identityError string

func (e identityError) Error() string { return string(e) }

func errIdentity(message string) error { return identityError(message) }

// generationReplaced returns the stable public sentinel with the
// container name attached. Keep the sentinel's original wording: callers
// have historically compared its Error text as well as using errors.Is.
func generationReplaced(id string) error {
	return fmt.Errorf("%w: %s", ErrGenerationReplaced, id)
}

// withGenerationReplaced retains the original cause while making the
// replacement sentinel discoverable with errors.Is. This matters for a
// reused Docker handle whose immutable target has disappeared.
func withGenerationReplaced(id string, cause error) error {
	if cause == nil {
		return generationReplaced(id)
	}
	return errors.Join(generationReplaced(id), cause)
}

// sameReuseGeneration compares identities fail closed. A Docker handle
// is identified by its full immutable UID; a name-only handle is
// identified by a valid Apple creation generation. Missing or malformed
// values are never treated as a match, even when both sides contain
// the same empty or invalid value.
func sameReuseGeneration(before, fresh *engineInfo) bool {
	if before == nil || fresh == nil {
		return false
	}
	if before.uid != "" || fresh.uid != "" {
		return validDockerUID(before.uid) &&
			validDockerUID(fresh.uid) &&
			before.uid == fresh.uid
	}
	beforeGeneration := before.labels[creationLabel]
	freshGeneration := fresh.labels[creationLabel]
	return validCreationGeneration(beforeGeneration) &&
		validCreationGeneration(freshGeneration) &&
		beforeGeneration == freshGeneration
}

// sameEngineIdentity is the backend-specific form used for a handle
// already bound to an engine. In particular, an Apple identity must not
// silently fall back to matching two missing UID fields.
func sameEngineIdentity(eng engine, before, fresh *engineInfo) bool {
	if before == nil || fresh == nil {
		return false
	}
	switch eng.name() {
	case "docker":
		return validDockerUID(before.uid) &&
			validDockerUID(fresh.uid) &&
			before.uid == fresh.uid
	case "apple":
		return before.uid == "" && fresh.uid == "" &&
			validCreationGeneration(before.labels[creationLabel]) &&
			validCreationGeneration(fresh.labels[creationLabel]) &&
			before.labels[creationLabel] == fresh.labels[creationLabel]
	default:
		return sameReuseGeneration(before, fresh)
	}
}

// platformParts splits an OCI platform into its optional OS,
// architecture, and variant components. More than three components is
// malformed and is represented by an empty OS so callers fail closed.
func platformParts(platform string) (os, arch, variant string) {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(platform)), "/")
	if len(parts) > 3 {
		return "", "", ""
	}
	for _, part := range parts {
		if part == "" {
			return "", "", ""
		}
	}
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

func samePlatform(before, fresh string) bool {
	beforeOS, beforeArch, beforeVariant := platformParts(before)
	freshOS, freshArch, freshVariant := platformParts(fresh)
	if before == "" && fresh == "" {
		return true
	}
	if beforeOS == "" || freshOS == "" {
		return false
	}
	return beforeOS == freshOS &&
		beforeArch == freshArch &&
		beforeVariant == freshVariant
}

// platformMatches reports whether observed satisfies requested. A
// requested OS/architecture/variant is a prefix match so callers may
// request only the components they care about, but an observed platform
// may not omit a requested component or contain malformed extra fields.
func platformMatches(requested, observed string) bool {
	if requested == "" {
		return true
	}
	if observed == "" {
		return false
	}
	wantOS, wantArch, wantVariant := platformParts(requested)
	if wantOS == "" {
		return false
	}
	gotOS, gotArch, gotVariant := platformParts(observed)
	if gotOS == "" || (gotVariant != "" && gotArch == "") {
		return false
	}
	if wantOS != "" && wantOS != gotOS {
		return false
	}
	if wantArch != "" && (gotArch == "" || wantArch != gotArch) {
		return false
	}
	if wantVariant != "" && (gotVariant == "" || wantVariant != gotVariant) {
		return false
	}
	return true
}

// platformNeedsResolution reports whether the top-level container
// inspect value is incomplete for the requested platform. A complete
// but different value is already sufficient to reject the request; it
// must not trigger a second, potentially mutable image lookup.
func platformNeedsResolution(requested, observed string) bool {
	if requested == "" {
		return false
	}
	_, wantArch, wantVariant := platformParts(requested)
	gotOS, gotArch, gotVariant := platformParts(observed)
	if gotOS == "" {
		return true
	}
	if wantArch != "" && gotArch == "" {
		return true
	}
	if wantVariant != "" && gotVariant == "" {
		return true
	}
	return false
}

// containerPlatformResolver is implemented by backends whose container
// inspect does not expose a complete OCI platform directly.
type containerPlatformResolver interface {
	resolvePlatform(context.Context, cli.Runner, *engineInfo, string) (string, error)
}

func resolveContainerPlatform(ctx context.Context, eng engine, runner cli.Runner, info *engineInfo, requested string) (string, error) {
	if info == nil {
		return "", errIdentity("empty inspect result")
	}
	resolver, ok := eng.(containerPlatformResolver)
	if !ok || !platformNeedsResolution(requested, info.platform) {
		return info.platform, nil
	}
	return resolver.resolvePlatform(ctx, runner, info, requested)
}

// resolveInfoPlatform returns a copy so a shared reuse flight never has
// one caller's requested platform written into another caller's snapshot.
func resolveInfoPlatform(ctx context.Context, eng engine, runner cli.Runner, info *engineInfo, requested string) (*engineInfo, error) {
	if info == nil {
		return nil, errIdentity("empty inspect result")
	}
	if requested == "" {
		return info, nil
	}
	qCtx, cancel := withDefaultTimeout(ctx, queryTimeout)
	defer cancel()
	platform, err := resolveContainerPlatform(qCtx, eng, runner, info, requested)
	if err != nil {
		return nil, err
	}
	copyInfo := *info
	copyInfo.platform = platform
	return &copyInfo, nil
}
