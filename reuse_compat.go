package container

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/hirokazumiyaji/container-go/internal/domain"
)

// checkReuseIdentity verifies the ownership facts required before a
// reuse attach or destructive recycle. A reuse marker alone is not proof:
// the managed marker, a valid generation, and (for Docker) a full UID are
// all required.
func checkReuseIdentity(info *engineInfo, cfg *config) error {
	if info == nil {
		return fmt.Errorf("reuse %s: inspect returned no container identity", cfg.name)
	}
	expected := domain.OwnershipExpected{
		RequireManaged:  true,
		RequireReuse:    true,
		RequireCreation: true,
	}
	if requiresImmutableID(cfg.eng) {
		expected.RequireImmutableID = true
	} else {
		expected.RequireNoImmutableID = true
	}
	switch domain.VerifyOwnership(expected, ownershipObserved(info)) {
	case domain.OwnershipFailureNone:
		return nil
	case domain.OwnershipFailureManaged:
		return fmt.Errorf("reuse %s: existing container is not managed by container-go", cfg.name)
	case domain.OwnershipFailureReuse:
		return fmt.Errorf("reuse: existing container was not created with WithReuse")
	case domain.OwnershipFailureCreation:
		return identityError(fmt.Sprintf("reuse %s: existing container has no valid creation generation", cfg.name))
	case domain.OwnershipFailureIdentity:
		if requiresImmutableID(cfg.eng) {
			return identityError(fmt.Sprintf("reuse %s: existing container has no valid immutable ID", cfg.name))
		}
		return identityError(fmt.Sprintf("reuse %s: name-addressed container unexpectedly has an immutable ID", cfg.name))
	default:
		return identityError(fmt.Sprintf("reuse %s: existing container identity is invalid", cfg.name))
	}
}

// checkReuseOwnedIdentity reports whether a stopped container may be
// deleted and recreated for this reuse request. The requested image is
// the identity resolved immediately before the create/attach decision,
// not the caller's mutable tag.
func checkReuseOwnedIdentity(info *engineInfo, requested imageIdentity, original string, cfg *config) error {
	if err := checkReuseOwnership(info, cfg); err != nil {
		return err
	}
	return checkReuseImageIdentity(info, requested, original, cfg)
}

// checkReuseOwnership verifies the ownership labels, generation, and reuse
// group that prove the observed container belongs to this request.
func checkReuseOwnership(info *engineInfo, cfg *config) error {
	if err := checkReuseIdentity(info, cfg); err != nil {
		return err
	}
	if info.labels[reuseGroupLabel] != cfg.reuseGroup {
		return fmt.Errorf("%w: reuse group does not match the existing container", ErrReuseMismatch)
	}
	return nil
}

// checkReuseImageIdentity compares the resolved requested identity with the
// identity inspect reported. The caller has already resolved the image and
// verified its presence and digest; this comparison has no I/O.
func checkReuseImageIdentity(info *engineInfo, requested imageIdentity, original string, cfg *config) error {
	if requested.pinned {
		actual := imageFromInfo(info)
		if !requestedImageIdentitiesCompatible(requested, actual) {
			return fmt.Errorf("%w: reuse %s: image %q does not match existing %q", ErrReuseMismatch, cfg.name, original, info.image)
		}
		return nil
	}
	if !cfg.eng.imageReferencesCompatible(original, info.image) {
		return fmt.Errorf("%w: reuse %s: image %q does not match existing %q", ErrReuseMismatch, cfg.name, original, info.image)
	}
	return nil
}

// checkReuseOwned reports whether an existing container may be adopted
// or recycled for this reuse request.
func checkReuseOwned(info *engineInfo, image string, cfg *config) error {
	return checkReuseOwnedIdentity(info, imageIdentity{reference: image}, image, cfg)
}

// checkReuseCompatIdentity applies the resolved image and port checks
// for a reuse request.
func checkReuseCompatIdentity(info *engineInfo, requested imageIdentity, original string, cfg *config) error {
	if err := checkReuseOwnedIdentity(info, requested, original, cfg); err != nil {
		return err
	}
	return checkReuseConfigCompat(info, cfg)
}

func checkReuseConfigCompat(info *engineInfo, cfg *config) error {
	if err := checkReuseCreationConfig(info, cfg); err != nil {
		return err
	}
	if compat, ok := cfg.eng.(engineNetworkCompat); ok {
		if err := compat.checkReuseNetworkCompat(info, cfg); err != nil {
			return err
		}
	}
	if cfg.platform != "" {
		if info.platform == "" || platformSelectorUnverifiable(cfg.platform, info.platform) {
			return fmt.Errorf("%w: reuse %s: platform %q could not be verified", ErrReuseMismatch, cfg.name, cfg.platform)
		}
		if !enginePlatformCompatible(cfg.eng, cfg.platform, info) {
			return fmt.Errorf("%w: reuse %s: platform %q does not match existing %q", ErrReuseMismatch, cfg.name, cfg.platform, info.platform)
		}
	}
	if err := checkReuseRunOptions(info, cfg); err != nil {
		return fmt.Errorf("%w: reuse %s: %w", ErrReuseMismatch, cfg.name, err)
	}
	return checkReusePortCompat(info, cfg)
}

func checkReusePortCompat(info *engineInfo, cfg *config) error {
	// Auto-published exposed ports only appear as host bindings on
	// published-port backends. Explicit WithPublishedPort always needs
	// validation, including on direct-IP engines.
	if !cfg.eng.directIP() {
		for _, spec := range cfg.exposed {
			b, ok := matchingExposedBinding(info.bound, spec, cfg.eng)
			if !ok {
				return fmt.Errorf("%w: %w: reuse %s: exposed port %s missing on existing container", ErrReuseMismatch, errReusePortPending, cfg.name, spec)
			}
			if err := checkReuseBindingReachable(cfg.eng, b); err != nil {
				return fmt.Errorf("%w: reuse %s: exposed port %s: %w", ErrReuseMismatch, cfg.name, spec, err)
			}
		}
	}
	for _, p := range cfg.published {
		b, ok := matchingPublishedBinding(info.bound, p, cfg.eng)
		if !ok {
			if cfg.eng.directIP() {
				return fmt.Errorf("%w: reuse %s: published port %s missing on existing container", ErrReuseMismatch, cfg.name, p.Raw)
			}
			return fmt.Errorf("%w: %w: reuse %s: published port %s missing on existing container", ErrReuseMismatch, errReusePortPending, cfg.name, p.Raw)
		}
		if err := checkReuseBindingReachable(cfg.eng, b); err != nil {
			return fmt.Errorf("%w: reuse %s: published port %s: %w", ErrReuseMismatch, cfg.name, p.Raw, err)
		}
	}
	return nil
}

// checkReuseRunOptions verifies that the existing container's inspected
// resource and security configuration matches what this caller requested.
// Requested CPU and shm limits must be reported and equal. Ulimit and
// extra-host lists are compared as sets, and a requested hostname must match.
// An unrequested CPU or shm limit places no constraint because the container
// may legitimately have inherited one from another creator.
//
// Capabilities use a requested-subset rule: every capability the caller asked
// to add or drop must be present in the inspected configuration, but an empty
// request accepts any inspected set. Docker may report a default or normalized
// capability set even when the caller passed no --cap-add/--cap-drop, so exact
// equality would reject a compatible container. An inspected ALL satisfies any
// requested capability.
func checkReuseRunOptions(info *engineInfo, cfg *config) error {
	reported := info.runOptions
	if cfg.cpus > 0 {
		if !reported.cpusReported {
			return fmt.Errorf("cpus %s could not be verified on the existing container", formatCPUs(cfg.cpus))
		}
		if math.Abs(reported.cpus-cfg.cpus) > 1e-9 {
			return fmt.Errorf("cpus %s does not match the existing container's %s", formatCPUs(cfg.cpus), formatCPUs(reported.cpus))
		}
	}
	if !cfg.shmSize.IsZero() {
		if !reported.shmSizeReported || reported.shmSizeBytes != cfg.shmSize.Bytes() {
			return fmt.Errorf("shm size %q does not match the existing container", cfg.shmSize.String())
		}
	}
	if reported.privileged != cfg.privileged {
		if cfg.privileged {
			return errors.New("existing container is not privileged")
		}
		return errors.New("existing container is privileged")
	}
	if missing := missingCapabilities(cfg.capAdd, reported.capAdd); len(missing) > 0 {
		return fmt.Errorf("requested added capabilities %v are missing from the existing container's %v",
			missing, canonicalCapabilities(reported.capAdd))
	}
	if missing := missingCapabilities(cfg.capDrop, reported.capDrop); len(missing) > 0 {
		return fmt.Errorf("requested dropped capabilities %v are missing from the existing container's %v",
			missing, canonicalCapabilities(reported.capDrop))
	}
	if !sameUlimits(cfg.ulimits, reported.ulimits) {
		return errors.New("ulimits do not match the existing container")
	}
	if !sameExtraHosts(cfg.extraHosts, reported.extraHosts) {
		return errors.New("extra hosts do not match the existing container")
	}
	if cfg.hostname != "" && !strings.EqualFold(reported.hostname, cfg.hostname) {
		return fmt.Errorf("hostname %q does not match the existing container's %q", cfg.hostname, reported.hostname)
	}
	return nil
}

// missingCapabilities returns the requested capabilities absent from reported.
// An empty request has no missing capabilities, and an inspected ALL satisfies
// every request because it already adds or drops the whole set.
func missingCapabilities(requested, reported []string) []string {
	want := canonicalCapabilities(requested)
	if len(want) == 0 {
		return nil
	}
	have := canonicalCapabilities(reported)
	haveSet := make(map[string]struct{}, len(have))
	for _, capability := range have {
		haveSet[capability] = struct{}{}
	}
	if _, ok := haveSet["ALL"]; ok {
		return nil
	}
	missing := make([]string, 0, len(want))
	for _, capability := range want {
		if _, ok := haveSet[capability]; !ok {
			missing = append(missing, capability)
		}
	}
	return missing
}

func canonicalCapabilities(caps []string) []string {
	seen := make(map[string]struct{}, len(caps))
	out := make([]string, 0, len(caps))
	for _, capability := range caps {
		key := capabilityKey(capability)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func sameUlimits(a, b []ulimitSpec) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]ulimitSpec(nil), a...)
	y := append([]ulimitSpec(nil), b...)
	sort.Slice(x, func(i, j int) bool { return x[i].name < x[j].name })
	sort.Slice(y, func(i, j int) bool { return y[i].name < y[j].name })
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

func sameExtraHosts(a, b []extraHostSpec) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]extraHostSpec(nil), a...)
	y := append([]extraHostSpec(nil), b...)
	less := func(hosts []extraHostSpec, i, j int) bool {
		hi, hj := strings.ToLower(hosts[i].host), strings.ToLower(hosts[j].host)
		if hi != hj {
			return hi < hj
		}
		return hosts[i].ip < hosts[j].ip
	}
	sort.Slice(x, func(i, j int) bool { return less(x, i, j) })
	sort.Slice(y, func(i, j int) bool { return less(y, i, j) })
	for i := range x {
		if !strings.EqualFold(x[i].host, y[i].host) || x[i].ip != y[i].ip {
			return false
		}
	}
	return true
}

//nolint:unparam // image is the handle's reference; tests happen to pass one value
func checkReuseCompat(info *engineInfo, image string, cfg *config) error {
	return checkReuseCompatIdentity(info, imageIdentity{reference: image}, image, cfg)
}

// imageFromInfo retains the identity reported while inspecting a
// container. When Docker has recorded an immutable repo digest (or ID)
// that matches an index root, imageFromInfo recovers the pinned
// identity that reuse can compare without consulting the daemon again.
func imageFromInfo(info *engineInfo) imageIdentity {
	if info == nil {
		return imageIdentity{}
	}
	platform := info.platform
	variantDigest := info.imageVariantDigest
	imageID := info.imageID
	if canonicalID, ok := canonicalDockerImageID(imageID); ok {
		imageID = canonicalID
	}
	if validImageDigest(info.imageDigest) {
		reference := info.image
		if domain.ImageReferenceBase(reference) == "" {
			reference = imageID
		}
		if domain.ImageReferenceBase(reference) == "" && isImageID(imageID) {
			return imageIdentity{
				reference:     imageID,
				digest:        info.imageDigest,
				rootDigest:    info.imageDigest,
				repository:    "",
				id:            imageID,
				pinned:        true,
				platform:      platform,
				variantDigest: variantDigest,
			}
		}
		if base := domain.ImageReferenceBase(reference); base != "" {
			// Container inspect may report a tag plus a separate
			// configuration descriptor digest. Synthesize the same
			// index@digest reference used for a new run so reuse compares
			// the root content rather than a mutable tag spelling.
			synthesized := base + "@" + info.imageDigest
			if !imageRE.MatchString(synthesized) {
				return imageIdentity{}
			}
			return imageIdentity{
				reference:     synthesized,
				digest:        info.imageDigest,
				rootDigest:    info.imageDigest,
				repository:    domain.ImageRepository(base),
				id:            imageID,
				pinned:        true,
				platform:      platform,
				variantDigest: variantDigest,
			}
		}
		// A digest without repository provenance is not a safe identity.
		return imageIdentity{}
	}
	if isImageID(imageID) {
		return imageIdentity{reference: imageID, id: imageID, pinned: true, platform: platform, variantDigest: variantDigest}
	}
	if digest := domain.ImageDigest(info.image); validImageDigest(digest) && domain.ImageReferenceBase(info.image) != "" {
		return imageIdentity{reference: info.image, digest: digest, rootDigest: digest, repository: domain.ImageRepository(info.image), pinned: true, platform: platform, variantDigest: variantDigest}
	}
	// A bare sha256:... in a container's image field is not enough to
	// establish a Docker local ID. Only info.imageID above is verified
	// backend identity data.
	if info.image != "" && !domain.IsBareImageReference(info.image) {
		return imageIdentity{reference: info.image, repository: domain.ImageRepository(info.image), platform: platform, variantDigest: variantDigest}
	}
	return imageIdentity{}
}

func checkReuseBindingReachable(eng engine, b boundPort) error {
	if eng.directIP() {
		return nil
	}
	_, err := dockerBindingConnectHost(b, eng)
	return err
}

func matchingExposedBinding(bound []boundPort, spec portSpec, eng engine) (boundPort, bool) {
	binding, ok := domain.MatchingExposedBinding(domainPortBindings(bound), spec, eng.isRemote())
	return boundPortFromDomain(binding), ok
}

func matchingPublishedBinding(bound []boundPort, p publishSpec, eng engine) (boundPort, bool) {
	binding, ok := domain.MatchingPublishedBinding(domainPortBindings(bound), p, eng.isRemote())
	return boundPortFromDomain(binding), ok
}
func isUnqualifiedImageReference(ref string) bool {
	base := domain.ImageReferenceBase(ref)
	if base == "" {
		return false
	}
	base = domain.ImageRepositoryBaseWithoutTag(base)
	first, _, _ := strings.Cut(base, "/")
	return !domain.IsRegistry(first)
}

func isDockerRegistryReference(ref string) bool {
	base := domain.ImageReferenceBase(ref)
	if base == "" {
		return false
	}
	base = domain.ImageRepositoryBaseWithoutTag(base)
	first, _, _ := strings.Cut(base, "/")
	return domain.CanonicalDockerHubRegistry(first) == "docker.io"
}

// imageRepositoriesCompatible compares canonical repository keys captured
// from backend inspect responses. Unlike path compatibility it is strict:
// once a backend has supplied a registry-qualified identity, another
// registry is never interchangeable.
func imageRepositoriesCompatible(a, b string) bool {
	ak, bk := domain.ImageRepository(a), domain.ImageRepository(b)
	return ak != "" && bk != "" && strings.EqualFold(ak, bk)
}

func reuseInfoForCaller(ctx context.Context, cfg *config, base *Container, requested imageIdentity, original string) (*engineInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var pendingPort error
	for ctx.Err() == nil {
		info, err := inspectReuseBase(ctx, base, cfg)
		if err == nil {
			pendingPort = nil
			generationVerified := reuseGenerationMatches(base, info)
			var ownershipErr, imageErr, configErr error
			var waitForAddress bool
			if generationVerified && info.state == StateRunning {
				ownershipErr = checkReuseOwnership(info, cfg)
				imageErr = ownershipErr
				if ownershipErr == nil {
					imageErr = checkReuseImageIdentity(info, requested, original, cfg)
				}
				if ownershipErr == nil && imageErr == nil {
					if configNeedsDefaultNetwork(cfg) {
						name, err := base.ensureDefaultNetworkForConfig(ctx, cfg)
						if err != nil {
							return nil, err
						}
						info.defaultNetwork = name
					}
					configErr = checkReuseConfigCompat(info, cfg)
					if errors.Is(configErr, errReusePortPending) {
						pendingPort = configErr
					}
					waitForAddress = configErr == nil && cfg.eng.directIP() &&
						(len(cfg.exposed) > 0 || len(cfg.published) > 0) && info.ip == ""
				}
			}
			decision := domain.DecideReuseCompat(domain.ReuseCompatInput{
				State:              info.state,
				GenerationVerified: generationVerified,
				OwnershipVerified:  ownershipErr == nil,
				ImageCompatible:    imageErr == nil,
				ConfigCompatible:   configErr == nil,
				PortPending:        errors.Is(configErr, errReusePortPending),
				WaitForAddress:     waitForAddress,
			})
			switch decision.Action {
			case domain.ReuseAttachRunning:
				return info, nil
			case domain.ReuseStartExisting:
				// Poll below: the generation is still starting, a requested
				// port is not published yet, or its address is not reported.
			case domain.ReuseFailMismatch:
				switch {
				case !generationVerified:
					return nil, &reuseValidationError{fmt.Errorf("reuse %s: %w", cfg.name, ErrGenerationReplaced)}
				case ownershipErr != nil:
					return nil, &reuseValidationError{ownershipErr}
				case imageErr != nil:
					return nil, &reuseValidationError{imageErr}
				case configErr != nil && !errors.Is(configErr, errReusePortPending):
					return nil, &reuseValidationError{configErr}
				default:
					return nil, &reuseValidationError{fmt.Errorf("reuse %s: %s", cfg.name, decision.Reason)}
				}
			default:
				return nil, &reuseValidationError{fmt.Errorf("reuse %s: unexpected decision %q", cfg.name, decision.Action)}
			}
		} else {
			if ctx.Err() != nil {
				break
			}
			if isNotFound(err) {
				return nil, &reuseValidationError{fmt.Errorf("reuse %s: %w", cfg.name, ErrGenerationReplaced)}
			}
			if err := inspectRetryError(ctx, err); err != nil {
				if ctx.Err() != nil {
					break
				}
				return nil, err
			}
		}
		if waitForReusePoll(ctx, cfg.effectiveReusePollInterval()) != nil {
			break
		}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) && pendingPort != nil {
		return nil, fmt.Errorf("%w: %w", pendingPort, ctx.Err())
	}
	return nil, ctx.Err()
}

// reuseGenerationMatches reports whether the inspected container still
// addresses the base generation the caller joined. The base identity is
// snapshotted under its lock; the comparison itself has no side effects.
func reuseGenerationMatches(base *Container, info *engineInfo) bool {
	if base == nil {
		return true
	}
	base.pack().mu.Lock()
	creation := base.creation
	uid := base.immutableID()
	base.pack().mu.Unlock()
	if creation != "" && info.labels[creationLabel] != creation {
		return false
	}
	return uid == "" || info.uid == "" || info.uid == uid
}

// reuseEnvFingerprintValue returns the opt-in digest of the environment
// applied to this request, or an empty string when the caller did not request
// environment matching. The digest covers sorted key/value pairs; raw values
// never leave this function.
func (c *config) reuseEnvFingerprintValue() string {
	if !c.reuseEnvFingerprint {
		return ""
	}
	pairs := make([][2]string, 0, len(c.env))
	for key, value := range c.env {
		pairs = append(pairs, [2]string{key, value})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i][0] < pairs[j][0] })
	data, _ := json.Marshal(pairs)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func (c *config) reuseConfigHash() string {
	mounts := make([]string, 0, len(c.mounts))
	for _, mount := range c.mounts {
		data, _ := json.Marshal(mount)
		mounts = append(mounts, string(data))
	}
	sort.Strings(mounts)
	cmd := make([]string, len(c.cmd))
	for i, arg := range c.cmd {
		cmd[i] = strconv.Quote(arg)
	}
	labels := make(map[string]string, len(c.labels))
	for key, value := range c.labels {
		labels[key] = strconv.Quote(value)
	}
	value := struct {
		Memory         uint64            `json:",omitempty"`
		User           string            `json:",omitempty"`
		Workdir        string            `json:",omitempty"`
		Entrypoint     string            `json:",omitempty"`
		Cmd            []string          `json:",omitempty"`
		Mounts         []string          `json:",omitempty"`
		Labels         map[string]string `json:",omitempty"`
		ReadOnlyRootFS bool              `json:",omitempty"`
		Init           bool              `json:",omitempty"`
		DNS            []string          `json:",omitempty"`
		StopSignal     string            `json:",omitempty"`
	}{
		Memory:         c.memory.Bytes(),
		User:           strconv.Quote(c.user),
		Workdir:        strconv.Quote(c.workdir),
		Entrypoint:     strconv.Quote(c.entrypoint),
		Cmd:            cmd,
		Mounts:         mounts,
		Labels:         labels,
		ReadOnlyRootFS: c.readOnlyRootFS,
		Init:           c.initProcess,
		DNS:            c.dnsServers,
		StopSignal:     c.stopSignal,
	}
	data, _ := json.Marshal(value)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func checkReuseCreationConfig(info *engineInfo, cfg *config) error {
	if info.labels[domain.LabelReuseConfig] != cfg.reuseConfigHash() {
		return fmt.Errorf("%w: creation configuration does not match the existing container; recreate it with the requested options", ErrReuseMismatch)
	}
	if cfg.reuseEnvFingerprint {
		want := cfg.reuseEnvFingerprintValue()
		got := info.labels[domain.LabelReuseEnvFingerprint]
		if got == "" {
			return fmt.Errorf("%w: reuse %s: environment fingerprint is missing from the existing container; recreate it with WithReuseEnvFingerprint", ErrReuseMismatch, cfg.name)
		}
		if got != want {
			return fmt.Errorf("%w: reuse %s: environment fingerprint does not match the existing container", ErrReuseMismatch, cfg.name)
		}
	}
	return nil
}
