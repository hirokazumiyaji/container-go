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

func dockerCreation(info *engineInfo) string {
	if info == nil {
		return ""
	}
	if info.created != "" {
		return info.created
	}
	return info.createdAt
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
		if !validDockerImageID(info.imageID) && !validImageDigest(info.imageDigest) && !validImageDigest(imageDigest(info.image)) {
			return errIdentity("Docker inspect returned no immutable image identity")
		}
		if strings.TrimSpace(dockerCreation(info)) == "" {
			return errIdentity("Docker inspect returned no creation metadata")
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
			before.uid == fresh.uid &&
			strings.TrimSpace(dockerCreation(before)) != "" &&
			strings.TrimSpace(dockerCreation(fresh)) != "" &&
			dockerCreation(before) == dockerCreation(fresh)
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
			before.uid == fresh.uid &&
			strings.TrimSpace(dockerCreation(before)) != "" &&
			strings.TrimSpace(dockerCreation(fresh)) != "" &&
			dockerCreation(before) == dockerCreation(fresh)
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
// architecture, and variant components. More than three components or
// an empty component is malformed and is represented by an empty OS so
// callers fail closed.
func platformParts(platform string) (os, arch, variant string) {
	normalized := strings.ToLower(strings.TrimSpace(platform))
	parts := strings.Split(normalized, "/")
	if len(parts) == 0 || len(parts) > 3 {
		return "", "", ""
	}
	for _, part := range parts {
		if part == "" || strings.TrimSpace(part) != part {
			return "", "", ""
		}
	}
	osName, architecture, variant := splitPlatform(normalized)
	return canonicalOCIPlatform(osName, architecture, variant)
}

// canonicalOCIPlatform normalizes common OCI architecture spellings and
// their unambiguous default variants before shared platform comparisons.
func canonicalOCIPlatform(osName, architecture, variant string) (string, string, string) {
	osName = strings.ToLower(strings.TrimSpace(osName))
	architecture = strings.ToLower(strings.TrimSpace(architecture))
	variant = strings.ToLower(strings.TrimSpace(variant))
	switch architecture {
	case "x86_64", "x86-64", "amd64":
		architecture = "amd64"
		if variant == "v1" || variant == "1" {
			variant = ""
		}
	case "aarch64", "arm64":
		architecture = "arm64"
		if variant == "" || variant == "v8" || variant == "8" {
			variant = "v8"
		}
	case "armhf":
		architecture = "arm"
		if variant == "" {
			variant = "v7"
		}
	case "armel":
		architecture = "arm"
		if variant == "" {
			variant = "v6"
		}
	case "arm":
		// The generic arm architecture has no portable default variant;
		// keep it empty so a requested variant is not guessed.
	case "i386", "i486", "i586", "i686", "386":
		architecture = "386"
		if variant == "v1" || variant == "1" {
			variant = ""
		}
	}
	return osName, architecture, variant
}

func validPlatformValue(platform string) bool {
	os, _, _ := platformParts(platform)
	return os != ""
}

func checkPlatformCompatibility(requested, observed string) error {
	if requested == "" {
		return nil
	}
	if !validPlatformValue(requested) {
		return fmt.Errorf("requested platform %q is malformed", requested)
	}
	if strings.TrimSpace(observed) == "" {
		return fmt.Errorf("platform %q could not be verified: inspect reported no platform", requested)
	}
	if !validPlatformValue(observed) {
		return fmt.Errorf("platform %q is malformed in inspect output", observed)
	}
	wantOS, wantArch, wantVariant := platformParts(requested)
	gotOS, gotArch, gotVariant := platformParts(observed)
	if wantOS != gotOS {
		return fmt.Errorf("platform %q does not match observed %q", requested, observed)
	}
	if wantArch != "" && (gotArch == "" || wantArch != gotArch) {
		return fmt.Errorf("platform %q does not match observed %q", requested, observed)
	}
	if wantVariant != "" && (gotVariant == "" || wantVariant != gotVariant) {
		return fmt.Errorf("platform %q does not match observed %q", requested, observed)
	}
	return nil
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

// platformObservationUnchanged reports whether a fresh platform observation
// either matches the earlier snapshot or is a less specific report of the
// same platform. A shared reuse flight can hand a caller a baseline that was
// resolved for another caller's explicit platform while this caller's own
// inspect reports only the OS; the missing components are unobserved, not
// changed, and a concrete conflicting value is still rejected.
func platformObservationUnchanged(before, fresh string) bool {
	if samePlatform(before, fresh) {
		return true
	}
	beforeOS, beforeArch, beforeVariant := platformParts(before)
	freshOS, freshArch, freshVariant := platformParts(fresh)
	if beforeOS == "" || freshOS == "" || beforeOS != freshOS {
		return false
	}
	if freshArch == "" {
		return true
	}
	if freshArch != beforeArch {
		return false
	}
	return freshVariant == "" || freshVariant == beforeVariant
}

// platformMatches reports whether observed satisfies requested. A
// requested OS/architecture/variant is a prefix match so callers may
// request only the components they care about, but an observed platform
// may not omit a requested component or contain malformed extra fields.
func platformMatches(requested, observed string) bool {
	if requested == "" {
		return true
	}
	if !validPlatformValue(requested) || !validPlatformValue(observed) {
		return false
	}
	wantOS, wantArch, wantVariant := platformParts(requested)
	gotOS, gotArch, gotVariant := platformParts(observed)
	if wantOS != gotOS {
		return false
	}
	if wantArch != "" && wantArch != gotArch {
		return false
	}
	if wantVariant != "" && wantVariant != gotVariant {
		return false
	}
	return true
}

// platformNeedsResolution reports whether the top-level container
// inspect value is incomplete for the requested platform. A complete
// but different value is already sufficient to reject the request; it
// must not trigger a second, potentially mutable image lookup.
func platformNeedsResolution(requested, observed string) bool {
	if requested == "" || !validPlatformValue(requested) || !validPlatformValue(observed) {
		return false
	}
	wantOS, wantArch, wantVariant := platformParts(requested)
	gotOS, gotArch, gotVariant := platformParts(observed)
	// An OS mismatch is already a definitive incompatibility. Never
	// resolve a different OS through a mutable image lookup.
	if wantOS != gotOS {
		return false
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
	if requested == "" {
		return info.platform, nil
	}
	if !validPlatformValue(requested) {
		return "", fmt.Errorf("requested platform %q is malformed", requested)
	}
	if info.platform == "" {
		return "", fmt.Errorf("platform %q could not be verified: inspect reported no platform", requested)
	}
	if !validPlatformValue(info.platform) {
		return "", fmt.Errorf("platform %q is malformed in inspect output", info.platform)
	}
	wantOS, _, _ := platformParts(requested)
	gotOS, _, _ := platformParts(info.platform)
	if wantOS != gotOS {
		return info.platform, nil
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
