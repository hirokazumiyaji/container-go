package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// reuseFlights collapses concurrent WithReuse get-or-create calls that
// share a name into one ensure operation. Each caller still applies its
// own compatibility check and wait strategy afterward.
var reuseFlights flightGroup[*Container]

func reuseRun(ctx context.Context, image string, cfg *config) (*Container, error) {
	key := cfg.eng.name() + "\x00" + cfg.name
	base, err := reuseFlights.do(ctx, key, func() (*Container, error) {
		// Shared ensure must not die with the first caller's cancel;
		// waiters keep waiting on their own contexts.
		flightCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reuseAttachTimeout)
		defer cancel()
		return reuseEnsureContainer(flightCtx, image, cfg)
	})
	if err != nil {
		return nil, err
	}

	info := base.info
	if info == nil {
		info, err = inspectNamed(ctx, cfg, cfg.name)
		if err != nil {
			return nil, err
		}
	}
	// The shared ensure may have been started with another caller's image
	// or options. Resolve this caller's request independently before
	// comparing it with the live container.
	resolvedImage, err := cfg.ensureImageRef(ctx, image)
	if err != nil {
		return nil, err
	}
	if err := checkReuseCompatIdentity(info, resolvedImage, image, cfg); err != nil {
		return nil, err
	}
	if cfg.eng.name() == "apple" && resolvedImage.pinned {
		// Re-inspect immediately before returning an attached Apple
		// container. This closes the same final addressability window for
		// reuse adoption that verifyAppleCreatedImage closes for create.
		probe := namedContainer(cfg, cfg.name)
		probe.image = resolvedImage
		if err := verifyAppleCreatedImage(ctx, probe, resolvedImage); err != nil {
			return nil, err
		}
	}

	containerImage := imageFromInfo(info)
	if resolvedImage.pinned {
		containerImage = resolvedImage
	}
	ctr := &Container{
		id:        base.id,
		runner:    base.runner,
		eng:       base.eng,
		exposed:   cfg.exposed,
		published: cfg.published,
		reused:    true,
		info:      info,
		creation:  info.labels[creationLabel],
		uid:       info.uid,
		image:     containerImage,
	}
	if err := reuseWait(ctx, cfg, ctr); err != nil {
		return nil, err
	}
	return ctr, nil
}

// reuseEnsureContainer creates or attaches to the named container
// without per-caller wait or port compatibility checks. Those run in
// reuseRun so every concurrent caller applies its own configuration.
func reuseEnsureContainer(ctx context.Context, image string, cfg *config) (*Container, error) {
	recreated := false
	var resolvedImage imageIdentity
	imageResolved := false
	resolveImage := func() error {
		if imageResolved {
			return nil
		}
		var err error
		resolvedImage, err = cfg.ensureImageRef(context.WithoutCancel(ctx), image)
		if err != nil {
			return err
		}
		imageResolved = true
		return nil
	}

	for {
		if err := ctx.Err(); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return nil, fmt.Errorf("reuse %s: timed out waiting for a usable container", cfg.name)
			}
			return nil, err
		}

		info, err := inspectNamed(ctx, cfg, cfg.name)
		if err != nil {
			if !isContainerNotFound(cfg.eng, lifecycleInspect, cfg.name, err) {
				return nil, err
			}
			// Resolve before create so compatibility is based on the
			// immutable image that will actually be passed to run.
			if err := resolveImage(); err != nil {
				return nil, err
			}
			// Creation carries its own runTimeout budget detached from
			// the attach deadline: a leader pulling a large image must
			// not be cut off after reuseAttachTimeout.
			ctr, createErr := reuseCreateResolved(context.WithoutCancel(ctx), image, cfg, resolvedImage)
			if createErr == nil {
				return ctr, nil
			}
			// nameConflict: another process won create. createRaceMissing
			// covers Apple's concurrent-create race where run reaches
			// "Starting container" then reports the ID as not found.
			// Re-inspect and attach (or recreate) until the deadline.
			if cfg.eng.nameConflict(lifecycleRun, cfg.name, createErr) || cfg.eng.createRaceMissing(lifecycleRun, cfg.name, createErr) {
				time.Sleep(reusePollInterval)
				continue
			}
			return nil, createErr
		}

		switch info.state {
		case StateCreated, StateStopping, StateUnknown:
			time.Sleep(reusePollInterval)
			continue
		case StateStopped:
			if recreated {
				return nil, fmt.Errorf("reuse %s: container stayed stopped after recreate", cfg.name)
			}
			if err := resolveImage(); err != nil {
				return nil, err
			}
			// Only recycle containers this library created for reuse
			// with a compatible resolved image; never delete foreign
			// leftovers or a container whose image cannot be verified.
			if err := checkReuseOwnedIdentity(info, resolvedImage, image, cfg); err != nil {
				return nil, err
			}
			if err := deleteStoppedReuse(ctx, cfg, info); err != nil {
				return nil, err
			}
			recreated = true
			continue
		case StateRunning:
			if err := resolveImage(); err != nil {
				return nil, err
			}
			return &Container{
				id:        cfg.name,
				runner:    cfg.runner,
				eng:       cfg.eng,
				exposed:   cfg.exposed,
				published: cfg.published,
				reused:    true,
				info:      info,
				creation:  info.labels[creationLabel],
				uid:       info.uid,
				image:     resolvedImage,
			}, nil
		default:
			time.Sleep(reusePollInterval)
		}
	}
}

func reuseCreate(ctx context.Context, image string, cfg *config) (*Container, error) {
	runCtx, cancel := withDefaultTimeout(context.WithoutCancel(ctx), runTimeout)
	defer cancel()
	resolvedImage, err := cfg.ensureImageRef(runCtx, image)
	if err != nil {
		return nil, err
	}
	return reuseCreateResolved(ctx, image, cfg, resolvedImage)
}

func reuseCreateResolved(ctx context.Context, image string, cfg *config, resolvedImage imageIdentity) (*Container, error) {
	var envFile string
	if len(cfg.env) > 0 {
		path, dir, err := writeEnvFile(cfg.env)
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(dir)
		envFile = path
	}
	if cfg.creation == "" {
		cfg.creation = newCreationID()
	}

	// The leader's create gets an independent runTimeout budget even
	// when the caller's context carries a tighter attach deadline.
	runCtx, cancel := withDefaultTimeout(context.WithoutCancel(ctx), runTimeout)
	defer cancel()
	stdout, _, err := cfg.runner.Run(runCtx, cfg.eng.runArgs(cfg, resolvedImage.reference, envFile)...)
	if err != nil {
		classified := cli.Classify(ctx, cfg.runner, err, cfg.eng.probe())
		if cfg.eng.nameConflict(lifecycleRun, cfg.name, err) || cfg.eng.nameConflict(lifecycleRun, cfg.name, classified) ||
			cfg.eng.createRaceMissing(lifecycleRun, cfg.name, err) || cfg.eng.createRaceMissing(lifecycleRun, cfg.name, classified) {
			// Leave attach/retry to reuseEnsureContainer; do not delete
			// a peer's in-flight container on a not-found race.
			return nil, err
		}
		cleanupFailedCreate(ctx, cfg, err, classified)
		return nil, classified
	}

	ctr := &Container{
		id:        cfg.name,
		runner:    cfg.runner,
		eng:       cfg.eng,
		exposed:   cfg.exposed,
		published: cfg.published,
		reused:    true,
		creation:  cfg.creation,
		uid:       cfg.eng.parseRunID(stdout),
		image:     resolvedImage,
	}
	info, err := ctr.cachedInfo(ctx)
	if err != nil {
		return nil, cleanupReuseCreate(ctx, ctr, err)
	}
	// Validate the image identity and requested ports before exposing the
	// new shared container to the flight. A failed post-create check must
	// not leave an unusable container behind.
	if err := checkReuseCompatIdentity(info, resolvedImage, image, cfg); err != nil {
		return nil, cleanupReuseCreate(ctx, ctr, err)
	}
	if err := verifyAppleCreatedImage(context.WithoutCancel(ctx), ctr, resolvedImage); err != nil {
		return nil, cleanupReuseCreate(ctx, ctr, err)
	}
	for _, f := range cfg.files {
		if err := ctr.CopyToContainer(ctx, f.HostPath, f.ContainerPath); err != nil {
			return nil, cleanupReuseCreate(ctx, ctr, err)
		}
	}
	return ctr, nil
}

func cleanupReuseCreate(ctx context.Context, ctr *Container, cause error) error {
	if err := ctr.Terminate(context.WithoutCancel(ctx)); err != nil {
		return fmt.Errorf("%w (container %s left behind: %v)", cause, ctr.id, err)
	}
	return cause
}

// deleteStoppedReuse removes a stopped reuse container through a
// handle bound to its inspected generation, so Terminate re-checks the
// generation and deletes by immutable ID. A replaced generation means
// another process already recreated the name; the caller loops and
// attaches to the fresh generation instead of deleting it.
func deleteStoppedReuse(ctx context.Context, cfg *config, info *engineInfo) error {
	ctr := namedContainer(cfg, cfg.name)
	ctr.creation = info.labels[creationLabel]
	err := ctr.Terminate(ctx)
	if errors.Is(err, ErrGenerationReplaced) {
		return nil
	}
	return err
}

func reuseWait(ctx context.Context, cfg *config, ctr *Container) error {
	if cfg.waitStrategy == nil {
		return nil
	}
	if err := cfg.waitStrategy.WaitUntilReady(ctx, waitTarget{c: ctr}); err != nil {
		tail := ctr.logTail(context.WithoutCancel(ctx))
		if tail != "" {
			return fmt.Errorf("reuse %s failed to become ready: %w\ncontainer logs:\n%s", ctr.id, err, tail)
		}
		return fmt.Errorf("reuse %s failed to become ready: %w", ctr.id, err)
	}
	return nil
}

func inspectNamed(ctx context.Context, cfg *config, id string) (*engineInfo, error) {
	return namedContainer(cfg, id).inspectFresh(ctx)
}

func namedContainer(cfg *config, id string) *Container {
	return &Container{
		id:        id,
		runner:    cfg.runner,
		eng:       cfg.eng,
		exposed:   cfg.exposed,
		published: cfg.published,
	}
}

// imageFromInfo retains the identity reported while inspecting a
// container, including the digest form when the backend exposes one.
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
		if imageReferenceBase(reference) == "" {
			reference = imageID
		}
		if imageReferenceBase(reference) == "" && isImageID(imageID) {
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
		if base := imageReferenceBase(reference); base != "" {
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
				repository:    imageRepository(base),
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
	if digest := imageDigest(info.image); validImageDigest(digest) && imageReferenceBase(info.image) != "" {
		return imageIdentity{reference: info.image, digest: digest, rootDigest: digest, repository: imageRepository(info.image), pinned: true, platform: platform, variantDigest: variantDigest}
	}
	// A bare sha256:... in a container's image field is not enough to
	// establish a Docker local ID. Only info.imageID above is verified
	// backend identity data.
	if info.image != "" && !isBareImageReference(info.image) {
		return imageIdentity{reference: info.image, repository: imageRepository(info.image), platform: platform, variantDigest: variantDigest}
	}
	return imageIdentity{}
}

// checkReuseOwnedIdentity reports whether a stopped container may be
// deleted and recreated for this reuse request. The requested image is
// the identity resolved immediately before the create/attach decision,
// not the caller's mutable tag.
func checkReuseOwnedIdentity(info *engineInfo, requested imageIdentity, original string, cfg *config) error {
	if info.labels[reuseLabel] != "true" {
		return fmt.Errorf("reuse %s: existing container was not created with WithReuse", cfg.name)
	}
	if requested.pinned {
		actual := imageFromInfo(info)
		if !requestedImageIdentitiesCompatible(requested, actual) {
			return fmt.Errorf("reuse %s: image %q does not match existing %q", cfg.name, original, info.image)
		}
		return nil
	}
	compatible := imagesCompatible(original, info.image)
	if cfg.eng.name() == "apple" {
		compatible = appleImageReferencesCompatible(original, info.image)
	}
	if !compatible {
		return fmt.Errorf("reuse %s: image %q does not match existing %q", cfg.name, original, info.image)
	}
	return nil
}

// checkReuseCompatIdentity applies the resolved image and port checks
// for a reuse request.
func checkReuseCompatIdentity(info *engineInfo, requested imageIdentity, original string, cfg *config) error {
	if err := checkReuseOwnedIdentity(info, requested, original, cfg); err != nil {
		return err
	}
	// Auto-published exposed ports only appear as host bindings on
	// published-port backends. Explicit WithPublishedPort always needs
	// validation, including on direct-IP engines.
	if !cfg.eng.directIP() {
		for _, spec := range cfg.exposed {
			if !hasBoundPort(info.bound, spec.port, spec.proto) {
				return fmt.Errorf("reuse %s: exposed port %s missing on existing container", cfg.name, spec)
			}
		}
	}
	for _, p := range cfg.published {
		if !hasPublishedBinding(info.bound, p) {
			return fmt.Errorf("reuse %s: published port %s missing on existing container", cfg.name, p.raw)
		}
	}
	return nil
}

func hasBoundPort(bound []boundPort, port int, proto string) bool {
	for _, b := range bound {
		if b.containerPort == port && b.proto == proto && b.hostPort > 0 {
			return true
		}
	}
	return false
}

func hasPublishedBinding(bound []boundPort, p publishSpec) bool {
	for _, b := range bound {
		if b.containerPort != p.containerPort || b.proto != p.proto {
			continue
		}
		if p.hostPort != 0 && b.hostPort != p.hostPort {
			continue
		}
		if p.hostAddr != "" && b.hostAddr != "" && b.hostAddr != p.hostAddr {
			continue
		}
		return true
	}
	return false
}

// imagesCompatible reports whether a requested image reference matches
// what inspect reported. Short Docker Hub names are normalized
// (docker.io/library/..., latest) before comparison; arbitrary
// registry/namespace suffix matches are rejected. When the request pins
// a digest, both the normalized base and the digest must match.
func imagesCompatible(requested, actual string) bool {
	if requested == "" || actual == "" {
		return requested == actual
	}
	// A bare digest/ID has no repository namespace. Comparing it by its
	// digest alone would make unrelated registries interchangeable.
	if isBareImageReference(requested) || isBareImageReference(actual) {
		return false
	}
	if requested == actual {
		return true
	}
	reqDigest := imageDigest(requested)
	actDigest := imageDigest(actual)
	if reqDigest != "" {
		if !strings.EqualFold(reqDigest, actDigest) {
			return false
		}
		reqBase := imageReferenceBase(requested)
		actBase := imageReferenceBase(actual)
		return reqBase != "" && actBase != "" &&
			normalizeImageRef(reqBase) == normalizeImageRef(actBase)
	}
	req := imageReferenceBase(requested)
	act := imageReferenceBase(actual)
	return req != "" && act != "" && normalizeImageRef(req) == normalizeImageRef(act)
}

// imageRepository returns the canonical repository portion of an image
// reference, discarding any tag or digest. It is used for a digest
// response that has no tag but still must not cross a registry or
// namespace boundary. A bare ID/digest has no repository and returns
// the empty string.
func imageRepository(ref string) string {
	base := imageReferenceBase(ref)
	if base == "" {
		return ""
	}
	if i := strings.LastIndex(base, ":"); i >= 0 && !strings.Contains(base[i+1:], "/") {
		base = base[:i]
	}
	return strings.TrimSuffix(normalizeImageRef(base), ":latest")
}

// normalizeImageRef expands Docker Hub short names to a canonical
// registry/repo:tag form. The default registry is docker.io, the
// default namespace for single-component repos is library, and the
// default tag is latest.
func normalizeImageRef(ref string) string {
	tag := "latest"
	name := ref
	if i := strings.LastIndex(name, ":"); i >= 0 && !strings.Contains(name[i+1:], "/") {
		tag = name[i+1:]
		name = name[:i]
		if tag == "" {
			tag = "latest"
		}
	}
	var registry, repo string
	parts := strings.Split(name, "/")
	switch {
	case len(parts) == 1:
		registry = "docker.io"
		repo = "library/" + parts[0]
	case len(parts) == 2 && !isRegistry(parts[0]):
		registry = "docker.io"
		repo = name
	case isRegistry(parts[0]):
		registry = canonicalDockerHubRegistry(parts[0])
		repo = strings.Join(parts[1:], "/")
		if registry == "docker.io" && !strings.Contains(repo, "/") {
			repo = "library/" + repo
		}
	default:
		registry = "docker.io"
		repo = name
	}
	if repo == "" {
		repo = name
	}
	return registry + "/" + repo + ":" + tag
}

func isRegistry(s string) bool {
	return strings.Contains(s, ".") || strings.Contains(s, ":") || s == "localhost"
}

func canonicalDockerHubRegistry(registry string) string {
	switch strings.ToLower(registry) {
	case "docker.io", "registry-1.docker.io", "index.docker.io":
		return "docker.io"
	default:
		return registry
	}
}

func isUnqualifiedImageReference(ref string) bool {
	base := imageReferenceBase(ref)
	if base == "" {
		return false
	}
	base = imageRepositoryBaseWithoutTag(base)
	first, _, _ := strings.Cut(base, "/")
	return !isRegistry(first)
}

func isDockerRegistryReference(ref string) bool {
	base := imageReferenceBase(ref)
	if base == "" {
		return false
	}
	base = imageRepositoryBaseWithoutTag(base)
	first, _, _ := strings.Cut(base, "/")
	return canonicalDockerHubRegistry(first) == "docker.io"
}

func imageRepositoryBaseWithoutTag(base string) string {
	if i := strings.LastIndex(base, ":"); i >= 0 && !strings.Contains(base[i+1:], "/") {
		return base[:i]
	}
	return base
}

// imageRepositoryPathsCompatible compares the repository path and tag
// while allowing one side to be an unqualified backend-default name. It is
// intentionally separate from imagesCompatible: the latter retains the
// historical Docker-Hub-only policy for arbitrary public comparisons, while
// Apple inspect responses may legitimately contain a custom default
// registry.
func imageRepositoryPathsCompatible(a, b string) bool {
	aName, aTag, aExplicit := imageRepositoryParts(a)
	bName, bTag, bExplicit := imageRepositoryParts(b)
	if aName == "" || bName == "" {
		return false
	}
	if aExplicit && bExplicit {
		ra, rb := imageRegistryOf(a), imageRegistryOf(b)
		if ra == "" || rb == "" || !strings.EqualFold(ra, rb) {
			return false
		}
	}
	if !tagsCompatible(aTag, bTag) {
		return false
	}
	if aExplicit && bExplicit {
		return aName == bName
	}
	if aExplicit {
		return repositoryNameMatches(bName, aName)
	}
	if bExplicit {
		return repositoryNameMatches(aName, bName)
	}
	return normalizeImageRef(stripImageDigest(a)) == normalizeImageRef(stripImageDigest(b))
}

func imageRegistryOf(ref string) string {
	base := imageRepositoryBaseWithoutTag(imageReferenceBase(ref))
	if base == "" {
		return ""
	}
	first, _, _ := strings.Cut(base, "/")
	if !isRegistry(first) {
		return ""
	}
	return canonicalDockerHubRegistry(first)
}

func imageRepositoryParts(ref string) (name, tag string, explicit bool) {
	base := imageReferenceBase(ref)
	if base == "" {
		return "", "", false
	}
	name = base
	if i := strings.LastIndex(name, ":"); i >= 0 && !strings.Contains(name[i+1:], "/") {
		tag = name[i+1:]
		name = name[:i]
	}
	parts := strings.Split(name, "/")
	if len(parts) > 0 && isRegistry(parts[0]) {
		explicit = true
		registry := canonicalDockerHubRegistry(parts[0])
		name = strings.Join(parts[1:], "/")
		if registry == "docker.io" && !strings.Contains(name, "/") {
			name = "library/" + name
		}
	}
	return name, tag, explicit
}

func tagsCompatible(a, b string) bool {
	if a == "" {
		a = "latest"
	}
	if b == "" {
		b = "latest"
	}
	return a == b
}

func repositoryNameMatches(unqualified, qualified string) bool {
	if unqualified == qualified {
		return true
	}
	parts := strings.Split(unqualified, "/")
	if len(parts) == 1 {
		return qualified == unqualified || qualified == "library/"+unqualified
	}
	return false
}

// imageRepositoriesCompatible compares canonical repository keys captured
// from backend inspect responses. Unlike path compatibility it is strict:
// once a backend has supplied a registry-qualified identity, another
// registry is never interchangeable.
func imageRepositoriesCompatible(a, b string) bool {
	ak, bk := imageRepository(a), imageRepository(b)
	return ak != "" && bk != "" && strings.EqualFold(ak, bk)
}

func imageDigest(ref string) string {
	i := strings.Index(ref, "@")
	if i < 0 {
		return ""
	}
	return ref[i+1:]
}

func stripImageDigest(ref string) string {
	if i := strings.Index(ref, "@"); i >= 0 {
		return ref[:i]
	}
	return ref
}

// PruneReuseGroup force-removes every container tagged with the given
// WithReuseGroup value, running or stopped. Use it as a CI teardown
// step; ordinary Prune still only removes stopped managed containers.
func PruneReuseGroup(ctx context.Context, group string) ([]string, error) {
	if group == "" {
		return nil, fmt.Errorf("reuse group must not be empty")
	}
	eng, err := detectEngine()
	if err != nil {
		return nil, err
	}
	return pruneReuseGroupWith(ctx, &cli.ExecRunner{Binary: eng.binary()}, eng, group)
}

func pruneReuseGroupWith(ctx context.Context, r cli.Runner, eng engine, group string) ([]string, error) {
	return pruneListed(ctx, r, eng, eng.listReuseGroupArgs(group), func(data []byte) ([]string, error) {
		return eng.parseReuseGroupIDs(data, group)
	}, "prune reuse group "+group)
}
